// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats provides NATS JetStream KV-backed implementations of the domain
// storage ports. Storage is a simple UID-keyed cache: records are stored and
// retrieved by their UUID key only. There are no lookup indexes, no key scans,
// and no bulk-purge operations — those patterns belonged to the old sync job
// and are intentionally absent here.
package nats

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// Key prefix constants used within the single membership-cache bucket to
// namespace different record types and avoid key collisions.
const (
	keyPrefixTier        = "tier."
	keyPrefixMembership  = "membership."
	keyPrefixKeyContacts = "key-contacts."
	keyPrefixProjectSFID = "project-sfid."
	keyPrefixProjectUID  = "project-uid."
	keyPrefixSOQLPage    = "soql."
)

// Storage is a thin NATS KV cache keyed by record UID. It is used by the
// Salesforce-backed MemberReader to store and retrieve individual records.
// All reads return a CacheResult with CacheStatusMiss when the key is absent;
// the caller is responsible for fetching from Salesforce and writing back via
// Put*. Each stored value is wrapped in a CachedValue envelope that carries
// soft-TTL timestamps for stale-while-revalidate behaviour.
type Storage struct {
	client    *NATSClient
	ttlConfig TTLConfig
}

// NewStorage creates a new Storage backed by the given NATS client, using
// DefaultTTLConfig for soft-TTL envelope timestamps.
func NewStorage(client *NATSClient) *Storage {
	return NewStorageWithTTL(client, DefaultTTLConfig)
}

// NewStorageWithTTL creates a new Storage backed by the given NATS client,
// using the supplied TTLConfig for soft-TTL envelope timestamps. This is
// primarily useful in tests where shorter durations are desirable.
func NewStorageWithTTL(client *NATSClient, ttl TTLConfig) *Storage {
	return &Storage{client: client, ttlConfig: ttl}
}

// ─── MembershipTier ──────────────────────────────────────────────────────────

// GetTier retrieves a MembershipTier by UID. Returns a CacheResult whose Status
// is CacheStatusMiss when no entry exists for the key.
func (s *Storage) GetTier(ctx context.Context, uid string) (CacheResult[*model.MembershipTier], error) {
	return getCached[*model.MembershipTier](ctx, s, keyPrefixTier+uid)
}

// PutTier writes a MembershipTier to the KV bucket, keyed by its UID, wrapped
// in a CachedValue envelope using the Storage TTLConfig.
func (s *Storage) PutTier(ctx context.Context, tier *model.MembershipTier) error {
	if tier == nil {
		return errs.NewValidation("tier cannot be nil")
	}
	return putCached(ctx, s, keyPrefixTier+tier.UID, tier)
}

// ─── ProjectMembership ───────────────────────────────────────────────────────

// GetMembership retrieves a ProjectMembership by UID. Returns a CacheResult
// whose Status is CacheStatusMiss when no entry exists for the key.
func (s *Storage) GetMembership(ctx context.Context, uid string) (CacheResult[*model.ProjectMembership], error) {
	return getCached[*model.ProjectMembership](ctx, s, keyPrefixMembership+uid)
}

// PutMembership writes a ProjectMembership to the KV bucket, keyed by its UID,
// wrapped in a CachedValue envelope using the Storage TTLConfig.
func (s *Storage) PutMembership(ctx context.Context, membership *model.ProjectMembership) error {
	if membership == nil {
		return errs.NewValidation("membership cannot be nil")
	}
	return putCached(ctx, s, keyPrefixMembership+membership.UID, membership)
}

// DeleteMembership evicts the cached ProjectMembership for the UID from the
// soft-TTL bucket so the next read re-fetches fresh; a missing key is a no-op.
func (s *Storage) DeleteMembership(ctx context.Context, membershipUID string) error {
	if membershipUID == "" {
		return errs.NewValidation("membershipUID cannot be empty")
	}

	kv, ok := s.client.kvStore[constants.KVBucketNameCache]
	if !ok {
		return errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameCache))
	}

	key := keyPrefixMembership + membershipUID
	if err := kv.Delete(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return errs.NewUnexpected(
			fmt.Sprintf("failed to delete key %q from bucket %q", key, constants.KVBucketNameCache), err)
	}

	return nil
}

// ─── KeyContact ──────────────────────────────────────────────────────────────

// GetKeyContactsForMembership retrieves all key contacts cached for the given
// membership UID. The contacts are stored as a JSON array under the membership
// UID key (prefixed with "key-contacts.") in the membership-cache bucket.
// Returns a CacheResult whose Status is CacheStatusMiss when no entry exists.
func (s *Storage) GetKeyContactsForMembership(ctx context.Context, membershipUID string) (CacheResult[[]*model.KeyContact], error) {
	return getCached[[]*model.KeyContact](ctx, s, keyPrefixKeyContacts+membershipUID)
}

// PutKeyContactsForMembership writes the full slice of key contacts for a
// membership into the KV bucket as a single entry keyed by membership UID,
// wrapped in a CachedValue envelope using the Storage TTLConfig.
func (s *Storage) PutKeyContactsForMembership(ctx context.Context, membershipUID string, contacts []*model.KeyContact) error {
	if contacts == nil {
		contacts = []*model.KeyContact{}
	}
	return putCached(ctx, s, keyPrefixKeyContacts+membershipUID, contacts)
}

// DeleteKeyContactsForMembership removes the key-contacts cache entry for the
// given membership UID. This is called after a write mutation so the next read
// fetches fresh data from Salesforce rather than serving stale contacts.
// Returns nil if the key does not exist (a missing entry is already invalidated).
func (s *Storage) DeleteKeyContactsForMembership(ctx context.Context, membershipUID string) error {
	if membershipUID == "" {
		return errs.NewValidation("membershipUID cannot be empty")
	}

	kv, ok := s.client.kvStore[constants.KVBucketNameCache]
	if !ok {
		return errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameCache))
	}

	key := keyPrefixKeyContacts + membershipUID
	if err := kv.Delete(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return errs.NewUnexpected(
			fmt.Sprintf("failed to delete key %q from bucket %q", key, constants.KVBucketNameCache), err)
	}

	return nil
}

// ─── Project resolver cache ──────────────────────────────────────────────────

// GetProjectSFID retrieves the Salesforce Project__c.Id cached for the given
// v2 project UID. Returns CacheStatusMiss when no entry exists.
func (s *Storage) GetProjectSFID(ctx context.Context, projectUID string) (CacheResult[string], error) {
	return getCached[string](ctx, s, keyPrefixProjectSFID+projectUID)
}

// PutProjectSFID writes a project UID → Salesforce Project__c.Id mapping to
// the KV bucket, wrapped in a CachedValue envelope.
func (s *Storage) PutProjectSFID(ctx context.Context, projectUID, sfid string) error {
	return putCached(ctx, s, keyPrefixProjectSFID+projectUID, sfid)
}

// GetProjectUID retrieves the v2 project UID cached for the given project slug.
// Returns CacheStatusMiss when no entry exists.
func (s *Storage) GetProjectUID(ctx context.Context, slug string) (CacheResult[string], error) {
	return getCached[string](ctx, s, keyPrefixProjectUID+slug)
}

// PutProjectUID writes a project slug → v2 project UID mapping to the KV
// bucket, wrapped in a CachedValue envelope.
func (s *Storage) PutProjectUID(ctx context.Context, slug, uid string) error {
	return putCached(ctx, s, keyPrefixProjectUID+slug, uid)
}

// ─── SOQL membership batch cache ─────────────────────────────────────────────

// MembershipBatchCacheKey builds the NATS KV key for a cached SF batch.
//
// Batch 0 (the root entry) is keyed by stable query parameters only:
//
//	soql.memberships-by-project.{b64(sfid)}.{b64(sort)}[.{b64(tierSFID)}][.{b64(search:<term>)}].0
//
// Batches 1+ are keyed by the same prefix plus the iterator token that was
// generated by the previous batch and stored in its NextBatchIterator field:
//
//	soql.memberships-by-project.{b64(sfid)}.{b64(sort)}[.{b64(tierSFID)}][.{b64(search:<term>)}].{iterator}
//
// The optional search segment is present only when CompanyNameSearch is
// non-empty. The term is always lowercased before inclusion so that "Google"
// and "google" map to the same cache entry.
//
// The iterator is an 8-character alphanumeric random string generated at write
// time and embedded in the preceding batch's cache entry. This forms a singly-
// linked list: to find batch N+1 you must first read batch N. Consumers cannot
// speculatively fetch batch N+1 in parallel with batch N, which prevents
// torn-read coherence issues during background refreshes.
//
// NATS KV keys must not contain spaces or certain special characters; all
// variable segments are URL-safe base64-encoded. Dots separate segments.
func MembershipBatchCacheKey(templateRef string, params []string, batchIndex int, iterator string) string {
	parts := make([]string, 0, 3+len(params))
	parts = append(parts, templateRef)
	for _, p := range params {
		parts = append(parts, base64.RawURLEncoding.EncodeToString([]byte(p)))
	}
	if batchIndex == 0 {
		parts = append(parts, "0")
	} else {
		// iterator is already a short alphanumeric string; no encoding needed,
		// but we base64-encode for consistency and to guarantee NATS key safety.
		parts = append(parts, base64.RawURLEncoding.EncodeToString([]byte(iterator)))
	}
	return keyPrefixSOQLPage + strings.Join(parts, ".")
}

// MembershipBatchCacheEntry is the value stored for a single SF batch in the
// membership list cache. The records are gzip-compressed JSON to keep entries
// well under the NATS server's 1 MB max_payload limit (a 1000-record batch of
// full ProjectMembership objects compresses from ~800 KB to ~60 KB).
type MembershipBatchCacheEntry struct {
	// RecordsGZ is the gzip-compressed JSON encoding of
	// []*model.ProjectMembership for this batch.
	RecordsGZ []byte `json:"records_gz"`
	// NextBatchIterator is an 8-character random alphanumeric token that forms
	// the key segment for the next batch's cache entry. Empty when this is the
	// last batch (no more SF pages). The background goroutine writes the next
	// batch first, then writes this entry with the iterator populated, so
	// consumers can never observe a dangling iterator.
	NextBatchIterator string `json:"next_batch_iterator,omitempty"`
	// TotalSize is the Salesforce-reported total record count. Populated on
	// batch 0 only; zero on subsequent batches.
	TotalSize int `json:"total_size,omitempty"`
}

// DecodeBatchRecords decompresses and JSON-decodes the records stored in a
// MembershipBatchCacheEntry. Returns an error if decompression or decoding
// fails.
func (e *MembershipBatchCacheEntry) DecodeBatchRecords() ([]*model.ProjectMembership, error) {
	if len(e.RecordsGZ) == 0 {
		return nil, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(e.RecordsGZ))
	if err != nil {
		return nil, fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gr.Close() //nolint:errcheck // gzip.Reader.Close never returns a meaningful error.

	raw, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("decompressing batch records: %w", err)
	}

	var records []*model.ProjectMembership
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("unmarshalling batch records: %w", err)
	}
	return records, nil
}

// encodeBatchRecords JSON-marshals and gzip-compresses a slice of
// ProjectMembership records, returning the compressed bytes.
func encodeBatchRecords(records []*model.ProjectMembership) ([]byte, error) {
	raw, err := json.Marshal(records)
	if err != nil {
		return nil, fmt.Errorf("marshalling batch records: %w", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip-compressing batch records: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("flushing gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}

// GetMembershipBatch retrieves a cached MembershipBatchCacheEntry by key.
// Returns CacheStatusMiss when no entry exists for the key.
func (s *Storage) GetMembershipBatch(ctx context.Context, key string) (CacheResult[*MembershipBatchCacheEntry], error) {
	return getCached[*MembershipBatchCacheEntry](ctx, s, key)
}

// PutMembershipBatch writes a MembershipBatchCacheEntry to the KV bucket under
// the given key, wrapped in a CachedValue envelope. records are
// gzip-compressed before storage; nextBatchIterator is the token needed to look
// up the subsequent batch (empty for the last batch); totalSize should be set
// only for batch 0.
func (s *Storage) PutMembershipBatch(ctx context.Context, key string, records []*model.ProjectMembership, nextBatchIterator string, totalSize int) error {
	if key == "" {
		return errs.NewValidation("key cannot be empty")
	}

	gz, err := encodeBatchRecords(records)
	if err != nil {
		return errs.NewUnexpected("failed to encode membership batch", err)
	}

	entry := &MembershipBatchCacheEntry{
		RecordsGZ:         gz,
		NextBatchIterator: nextBatchIterator,
		TotalSize:         totalSize,
	}
	return putCached(ctx, s, key, entry)
}

// ─── Readiness ───────────────────────────────────────────────────────────────
func (s *Storage) IsReady(ctx context.Context) error {
	return s.client.IsReady(ctx)
}

// ─── low-level generic helpers ───────────────────────────────────────────────

// getCached fetches and JSON-decodes a CachedValue[T] from the named key in
// the membership-cache bucket. On a NATS key-not-found error it returns a
// CacheResult with CacheStatusMiss (not an error). On any other failure it
// returns a non-nil error. The CacheResult.Status is derived from the
// envelope's soft-TTL timestamps.
func getCached[T any](ctx context.Context, s *Storage, key string) (CacheResult[T], error) {
	var zero CacheResult[T]

	if key == "" {
		return zero, errs.NewValidation("key cannot be empty")
	}

	kv, ok := s.client.kvStore[constants.KVBucketNameCache]
	if !ok {
		return zero, errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameCache))
	}

	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return CacheResult[T]{Status: CacheStatusMiss}, nil
		}
		return zero, errs.NewUnexpected(
			fmt.Sprintf("failed to get key %q from bucket %q", key, constants.KVBucketNameCache), err)
	}

	var envelope CachedValue[T]
	if unmarshalErr := json.Unmarshal(entry.Value(), &envelope); unmarshalErr != nil {
		slog.WarnContext(ctx, "failed to unmarshal cached value; treating as miss",
			"key", key,
			"error", unmarshalErr,
		)
		return CacheResult[T]{Status: CacheStatusMiss}, nil
	}

	return CacheResult[T]{Value: envelope.Data, Status: envelope.Status()}, nil
}

// putCached JSON-encodes value inside a CachedValue envelope and writes it to
// key in the membership-cache bucket.
func putCached[T any](ctx context.Context, s *Storage, key string, value T) error {
	if key == "" {
		return errs.NewValidation("key cannot be empty")
	}

	kv, ok := s.client.kvStore[constants.KVBucketNameCache]
	if !ok {
		return errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameCache))
	}

	envelope := newCachedValue(value, s.ttlConfig)

	data, err := json.Marshal(envelope)
	if err != nil {
		return errs.NewUnexpected(
			fmt.Sprintf("failed to marshal value for key %q in bucket %q", key, constants.KVBucketNameCache), err)
	}

	if _, err := kv.Put(ctx, key, data); err != nil {
		slog.WarnContext(ctx, "failed to write record to KV cache",
			"bucket", constants.KVBucketNameCache,
			"key", key,
			"error", err,
		)
		return errs.NewUnexpected(
			fmt.Sprintf("failed to put key %q in bucket %q", key, constants.KVBucketNameCache), err)
	}

	return nil
}
