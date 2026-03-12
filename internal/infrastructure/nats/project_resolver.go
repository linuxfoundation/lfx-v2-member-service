// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	projectResolverCacheTTL     = 10 * time.Minute
	projectResolverRPCTimeout   = 5 * time.Second
	projectResolverCacheCleanup = 20 * time.Minute
)

// projectResolverEntry is a single cached entry mapping a v2 project UID to the
// corresponding B2B Salesforce Project__c SFID.
type projectResolverEntry struct {
	b2bSFID   string
	expiresAt time.Time
}

// projectResolver translates inbound v2 project UIDs to the B2B Salesforce
// Project__c SFIDs used as keys in the membership lookup index. It uses a
// two-stage lookup:
//
//  1. NATS RPC to v1-sync-helper (subject: lfx.lookup_v1_mapping, key:
//     "project.uid.{v2_uid}") → B2C Salesforce project SFID.
//  2. KV fetch of "salesforce-project__c.{b2c_sfid}" from v1-objects → decode
//     the "saleforce_id" field (note: intentional typo in the source data)
//     which holds the matching B2B Project__c.Id SFID.
//
// Results are cached in-memory for projectResolverCacheTTL to avoid repeated
// RPC+KV round-trips on paginated requests. A nil entry is cached on definitive
// misses (project not mapped) to suppress redundant lookups.
//
// When either the NATS connection or the v1-objects KV bucket is unavailable,
// the resolver returns an empty string so the caller falls back to treating the
// filter value as a raw B2B SFID.
type projectResolver struct {
	mu     sync.Mutex
	cache  map[string]*projectResolverEntry
	lastGC time.Time
}

// newProjectResolver creates a new projectResolver with an empty cache.
func newProjectResolver() *projectResolver {
	return &projectResolver{
		cache:  make(map[string]*projectResolverEntry),
		lastGC: time.Now(),
	}
}

// ResolveB2BSFID translates a v2 project UID to the corresponding B2B
// Salesforce Project__c SFID. Returns an empty string when the UID cannot be
// resolved (project not in v2 mappings, or infrastructure unavailable).
//
// The method is safe for concurrent use. Results are cached for
// projectResolverCacheTTL. A negative (empty) result is also cached to avoid
// hammering the RPC endpoint for projects that are not mapped.
func (r *projectResolver) ResolveB2BSFID(ctx context.Context, client *NATSClient, v2UID string) string {
	// Opportunistically evict expired entries to bound cache size.
	r.evictExpired()

	r.mu.Lock()
	if entry, ok := r.cache[v2UID]; ok {
		r.mu.Unlock()
		// A cached empty string means a confirmed miss — don't retry until TTL expires.
		return entry.b2bSFID
	}
	r.mu.Unlock()

	b2bSFID := r.resolve(ctx, client, v2UID)

	r.mu.Lock()
	r.cache[v2UID] = &projectResolverEntry{
		b2bSFID:   b2bSFID,
		expiresAt: time.Now().Add(projectResolverCacheTTL),
	}
	r.mu.Unlock()

	return b2bSFID
}

// resolve performs the actual two-stage lookup without consulting the cache.
func (r *projectResolver) resolve(ctx context.Context, client *NATSClient, v2UID string) string {
	conn := client.Conn()
	if conn == nil {
		slog.DebugContext(ctx, "project resolver: NATS connection unavailable, skipping UID translation",
			"v2_uid", v2UID,
		)
		return ""
	}

	// Stage 1: reverse-map the v2 UID to the B2C Salesforce project SFID via
	// the v1-sync-helper RPC endpoint. The mapping key is "project.uid.{uid}".
	b2cSFID := r.lookupB2CSFID(ctx, client, v2UID)
	if b2cSFID == "" {
		slog.DebugContext(ctx, "project resolver: no B2C SFID found for v2 UID",
			"v2_uid", v2UID,
		)
		return ""
	}

	// Stage 2: fetch the B2C project record from v1-objects and read the
	// "saleforce_id" field (note: intentional typo in source data) which
	// contains the matching B2B Project__c.Id SFID.
	b2bSFID := r.lookupB2BSFID(ctx, client, b2cSFID)
	if b2bSFID == "" {
		slog.DebugContext(ctx, "project resolver: no B2B SFID found in v1-objects for B2C SFID",
			"v2_uid", v2UID,
			"b2c_sfid", b2cSFID,
		)
		return ""
	}

	slog.DebugContext(ctx, "project resolver: resolved v2 UID to B2B SFID",
		"v2_uid", v2UID,
		"b2c_sfid", b2cSFID,
		"b2b_sfid", b2bSFID,
	)

	return b2bSFID
}

// lookupB2CSFID calls the v1-sync-helper NATS RPC endpoint with the key
// "project.uid.{v2_uid}" and returns the B2C Salesforce project SFID.
// Returns an empty string on any error or when the mapping does not exist.
func (r *projectResolver) lookupB2CSFID(ctx context.Context, client *NATSClient, v2UID string) string {
	conn := client.Conn()

	mappingKey := fmt.Sprintf("project.uid.%s", v2UID)

	reqCtx, cancel := context.WithTimeout(ctx, projectResolverRPCTimeout)
	defer cancel()

	msg, err := conn.RequestWithContext(reqCtx, constants.V1MappingLookupSubject, []byte(mappingKey))
	if err != nil {
		slog.DebugContext(ctx, "project resolver: RPC lookup failed",
			"mapping_key", mappingKey,
			"error", err,
		)
		return ""
	}

	response := strings.TrimSpace(string(msg.Data))

	if response == "" || strings.HasPrefix(response, "error: ") {
		if response != "" {
			slog.DebugContext(ctx, "project resolver: RPC lookup returned error",
				"mapping_key", mappingKey,
				"response", response,
			)
		}
		return ""
	}

	return response
}

// lookupB2BSFID fetches the v1-objects KV entry for the B2C project record
// ("salesforce-project__c.{b2c_sfid}") and extracts the "saleforce_id" field
// (intentional typo in source data) which contains the B2B Project__c.Id.
// Returns an empty string when the KV entry is absent or the field is missing.
func (r *projectResolver) lookupB2BSFID(ctx context.Context, client *NATSClient, b2cSFID string) string {
	kv := client.V1ObjectsKV()
	if kv == nil {
		slog.DebugContext(ctx, "project resolver: v1-objects KV not available, skipping B2B SFID lookup",
			"b2c_sfid", b2cSFID,
		)
		return ""
	}

	kvKey := fmt.Sprintf("salesforce-project__c.%s", b2cSFID)

	entry, err := kv.Get(ctx, kvKey)
	if err != nil {
		if err == jetstream.ErrKeyNotFound {
			slog.DebugContext(ctx, "project resolver: B2C project record not found in v1-objects",
				"b2c_sfid", b2cSFID,
				"kv_key", kvKey,
			)
		} else {
			slog.WarnContext(ctx, "project resolver: failed to fetch B2C project record from v1-objects",
				"b2c_sfid", b2cSFID,
				"kv_key", kvKey,
				"error", err,
			)
		}
		return ""
	}

	// Decode as a generic map; the record may be JSON or msgpack but is typically
	// JSON in the v1-objects bucket for salesforce-project__c entries.
	var record map[string]any
	if jsonErr := json.Unmarshal(entry.Value(), &record); jsonErr != nil {
		slog.WarnContext(ctx, "project resolver: failed to decode B2C project record",
			"b2c_sfid", b2cSFID,
			"error", jsonErr,
		)
		return ""
	}

	// "saleforce_id" (note: intentional typo present in source data) holds the
	// B2B Salesforce org's Project__c.Id — the SFID used as the lookup key in
	// the membership index.
	b2bSFID, _ := record["saleforce_id"].(string)

	return strings.TrimSpace(b2bSFID)
}

// evictExpired removes expired entries from the cache under the write lock.
// It runs at most once per projectResolverCacheCleanup interval to limit
// lock contention on hot request paths.
func (r *projectResolver) evictExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if now.Before(r.lastGC.Add(projectResolverCacheCleanup)) {
		return
	}

	for uid, entry := range r.cache {
		if now.After(entry.expiresAt) {
			delete(r.cache, uid)
		}
	}

	r.lastGC = now
}

// isV2UUID reports whether s looks like a v2 UUID (RFC 4122 format). Filter
// values that pass this check are candidates for UID-to-SFID translation;
// values that don't are assumed to already be raw Salesforce SFIDs and are
// used as-is.
func isV2UUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
