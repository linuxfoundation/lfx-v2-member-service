// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// repairPendingPrefix is the key namespace for pending repair markers.
// Full key form: "pending.{reindex_type}.{sfid}".
const repairPendingPrefix = "pending."

// repairReindexTypes is the fixed set of reindex targets the CDC quota guard
// and the delete_access failure marker can produce. Both the writer
// (consumer) and the drain (API) validate against it. The two
// *DeleteAccess types are markers only — see
// constants.ReindexTypeB2BOrgDeleteAccess — and are never passed to
// reindexItem/the automated drain.
var repairReindexTypes = map[string]struct{}{
	constants.ReindexTypeB2BOrg:                        {},
	constants.ReindexTypeProjectMembership:             {},
	constants.ReindexTypeKeyContact:                    {},
	constants.ReindexTypeB2BOrgDeleteAccess:            {},
	constants.ReindexTypeProjectMembershipDeleteAccess: {},
}

// repairMarkerValue is the minimal pending-marker payload. Only skipped_at is
// stored — the type and SFID live in the key. A later skip for the same target
// rewrites the same marker with a fresh skipped_at.
type repairMarkerValue struct {
	SkippedAt time.Time `json:"skipped_at"`
}

// CDCRepairStore implements port.CDCRepairStore over the cdc-repair NATS KV
// bucket (authoritative, no TTL, history 1). It has no distributed lock:
// concurrent drains are made safe by idempotent targeted reindex plus the
// revision-conditional DeletePending.
type CDCRepairStore struct {
	client *NATSClient
}

// NewCDCRepairStore creates a CDCRepairStore backed by the given NATS client.
// The cdc-repair bucket must have been initialised via KeyValueStore.
func NewCDCRepairStore(client *NATSClient) *CDCRepairStore {
	return &CDCRepairStore{client: client}
}

// Ensure CDCRepairStore satisfies the port at compile time.
var _ port.CDCRepairStore = (*CDCRepairStore)(nil)

// validateReindexType checks reindexType against the fixed repair target set.
func validateReindexType(reindexType string) error {
	if _, ok := repairReindexTypes[reindexType]; !ok {
		return errs.NewValidation(fmt.Sprintf("cdc-repair: unsupported reindex type %q", reindexType))
	}
	return nil
}

// validateTarget checks the reindex type and canonical 18-character SFID.
func validateTarget(reindexType, sfid string) error {
	if err := validateReindexType(reindexType); err != nil {
		return err
	}
	// IsSFID only validates the base-62 alphabet; it does not verify that an
	// 18-char input's suffix is the correct case-encoding checksum for its
	// first 15 characters. Recompute and compare to catch a mismatched or
	// arbitrary suffix.
	canonical, err := sfuuid.Normalize18(sfid)
	if err != nil || canonical != sfid {
		return errs.NewValidation(fmt.Sprintf("cdc-repair: sfid %q is not a canonical 18-character SFID", sfid))
	}
	return nil
}

// pendingKey builds the "pending.{type}.{sfid}" key.
func pendingKey(reindexType, sfid string) string {
	return repairPendingPrefix + reindexType + "." + sfid
}

// kv returns the cdc-repair KV handle or an error if it was not initialised.
func (s *CDCRepairStore) kv() (jetstream.KeyValue, error) {
	kv, ok := s.client.kvStore[constants.KVBucketNameCDCRepair]
	if !ok {
		return nil, errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameCDCRepair))
	}
	return kv, nil
}

// PutPending writes (or overwrites) a pending marker for (reindexType, sfid).
func (s *CDCRepairStore) PutPending(ctx context.Context, reindexType, sfid string) error {
	if err := validateTarget(reindexType, sfid); err != nil {
		return err
	}
	kv, err := s.kv()
	if err != nil {
		return err
	}
	data, err := json.Marshal(repairMarkerValue{SkippedAt: time.Now()})
	if err != nil {
		return errs.NewUnexpected("cdc-repair: failed to marshal marker", err)
	}
	if _, putErr := kv.Put(ctx, pendingKey(reindexType, sfid), data); putErr != nil {
		return errs.NewUnexpected(fmt.Sprintf("cdc-repair: failed to write marker for %s/%s", reindexType, sfid), putErr)
	}
	return nil
}

// ListPending returns up to limit pending markers for reindexType with their KV
// revisions. It consumes a bounded, type-filtered key stream and stops early —
// it never enumerates the full bucket. Ordering is not promised.
func (s *CDCRepairStore) ListPending(ctx context.Context, reindexType string, limit int) ([]port.RepairMarker, error) {
	if err := validateReindexType(reindexType); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	kv, err := s.kv()
	if err != nil {
		return nil, err
	}

	// Filter to this type's pending namespace: "pending.{type}.>".
	lister, err := kv.ListKeysFiltered(ctx, repairPendingPrefix+reindexType+".>")
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, errs.NewUnexpected("cdc-repair: failed to list pending keys", err)
	}
	// Stop as soon as limit keys are collected rather than draining the full
	// filtered stream (draining the whole backlog on every call makes repeated
	// drains quadratic in backlog size). The underlying watcher's message
	// callback can be mid-send on the (buffered) updates channel when Stop is
	// called, so a background goroutine keeps draining after Stop to unblock
	// it and let the consumer tear down cleanly instead of leaking.
	keys := make([]string, 0, limit)
	for key := range lister.Keys() {
		keys = append(keys, key)
		if len(keys) >= limit {
			break
		}
	}
	if stopErr := lister.Stop(); stopErr != nil {
		slog.WarnContext(ctx, "cdc-repair: failed to stop key lister", "type", reindexType, "error", stopErr)
	}
	go func() {
		for range lister.Keys() {
		}
	}()

	markers := make([]port.RepairMarker, 0, len(keys))
	for _, key := range keys {
		entry, getErr := kv.Get(ctx, key)
		if getErr != nil {
			if errors.Is(getErr, jetstream.ErrKeyNotFound) {
				// Concurrently drained between list and get — skip.
				continue
			}
			return nil, errs.NewUnexpected(fmt.Sprintf("cdc-repair: failed to get marker %q", key), getErr)
		}
		sfid := sfidFromPendingKey(key, reindexType)
		if sfid == "" {
			// Malformed key — log-and-retain semantics are the caller's; skip here.
			continue
		}
		var val repairMarkerValue
		// A malformed value is retained (not treated as NotFound); SkippedAt is
		// left zero so the caller can still act on the marker.
		if unmarshalErr := json.Unmarshal(entry.Value(), &val); unmarshalErr != nil {
			slog.WarnContext(ctx, "cdc-repair: malformed marker value; retaining with zero skipped_at",
				"type", reindexType, "uid", sfid, "error", unmarshalErr)
		}
		markers = append(markers, port.RepairMarker{
			Type:      reindexType,
			SFID:      sfid,
			SkippedAt: val.SkippedAt,
			Revision:  entry.Revision(),
		})
	}
	return markers, nil
}

// DeletePending revision-conditionally deletes the marker for
// (reindexType, sfid). A revision mismatch is returned as Conflict so the newer
// marker survives; an already-absent marker is treated as success.
func (s *CDCRepairStore) DeletePending(ctx context.Context, reindexType, sfid string, revision uint64) error {
	if err := validateTarget(reindexType, sfid); err != nil {
		return err
	}
	kv, err := s.kv()
	if err != nil {
		return err
	}
	delErr := kv.Delete(ctx, pendingKey(reindexType, sfid), jetstream.LastRevision(revision))
	if delErr == nil {
		return nil
	}
	if errors.Is(delErr, jetstream.ErrKeyNotFound) {
		// Already gone (drained concurrently) — nothing to do.
		return nil
	}
	// Any other error (notably a last-revision mismatch) means the marker was
	// rewritten since selection; retain it for the next drain.
	return errs.NewConflict(fmt.Sprintf("cdc-repair: marker for %s/%s changed since selection", reindexType, sfid))
}

// sfidFromPendingKey extracts the SFID from a "pending.{type}.{sfid}" key.
// Returns "" when the key does not match the expected shape.
func sfidFromPendingKey(key, reindexType string) string {
	prefix := repairPendingPrefix + reindexType + "."
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return strings.TrimPrefix(key, prefix)
}
