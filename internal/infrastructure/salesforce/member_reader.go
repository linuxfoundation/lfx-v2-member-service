// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package salesforce provides a Salesforce SOQL-backed implementation of
// port.MemberReader. Salesforce is the source of truth; NATS KV is used as a
// per-record TTL cache in front of it. Cache misses are transparent to callers.
package salesforce

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// soqlMembershipsTemplateRef is the stable identifier used as the first segment
// of SOQL membership batch cache keys for ListMembershipsForProject queries.
const soqlMembershipsTemplateRef = "memberships-by-project"

// MemberReader implements port.MemberReader using Salesforce SOQL as the source
// of truth and NATS KV as a per-record TTL cache. The cache is a read-through
// layer: on a miss the record is fetched from Salesforce, written to KV, and
// returned. On a stale hit the cached value is returned immediately while a
// background refresh is triggered. The KV bucket TTL (24 hours) governs hard
// eviction.
type MemberReader struct {
	tiers            *MemberRepo
	memberships      *MembershipRepo
	contacts         *KeyContactRepo
	keyContactReader *KeyContactReader
	resolver         port.ProjectResolver
	cache            *nats.Storage
}

// NewMemberReader creates a MemberReader backed by the given Salesforce repos,
// project resolver, and NATS KV cache. All arguments are required.
func NewMemberReader(
	tiers *MemberRepo,
	memberships *MembershipRepo,
	contacts *KeyContactRepo,
	keyContactReader *KeyContactReader,
	resolver port.ProjectResolver,
	cache *nats.Storage,
) *MemberReader {
	return &MemberReader{
		tiers:            tiers,
		memberships:      memberships,
		contacts:         contacts,
		keyContactReader: keyContactReader,
		resolver:         resolver,
		cache:            cache,
	}
}

// Ensure MemberReader satisfies the port at compile time.
var _ port.MemberReader = (*MemberReader)(nil)

// refreshInBackground spawns a best-effort goroutine to re-fetch a record from
// Salesforce and update the KV cache. The goroutine runs with a detached
// context so it is not cancelled when the HTTP response is sent. Errors are
// logged at debug level only and do not affect the caller.
func (r *MemberReader) refreshInBackground(fetchFn func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		if err := fetchFn(ctx); err != nil {
			slog.Debug("background cache refresh failed", "error", err)
		}
	}()
}

// refreshTimeout is the deadline applied to background cache refresh goroutines.
const refreshTimeout = 30 * time.Second

// ─── Tiers ───────────────────────────────────────────────────────────────────

// ListTiersForProject returns all MembershipTier records for the given v2
// project UID. The UID is resolved to a Salesforce Project__c.Id via the
// ProjectResolver before the SOQL query is issued.
//
// Note: individual tiers are NOT written to the KV cache here. The list result
// is not cached as a unit, and there is no reliable way to key a filtered
// subset. Use GetTier for single-record lookups that benefit from caching.
func (r *MemberReader) ListTiersForProject(ctx context.Context, projectUID string) ([]*model.MembershipTier, error) {
	sfid, err := r.resolver.SFIDFromUID(ctx, projectUID)
	if err != nil {
		return nil, fmt.Errorf("resolving project SFID for UID %s: %w", projectUID, err)
	}

	tiers, err := r.tiers.FetchTiersByProjectSFID(ctx, sfid)
	if err != nil {
		return nil, fmt.Errorf("listing tiers for project %s: %w", projectUID, err)
	}

	// Stamp the v2 project UID onto each tier. The converter populates
	// ProjectSlug from the decoded Project__r relationship but cannot resolve
	// the v2 UID without a resolver call per record; doing it once here for the
	// whole batch is more efficient.
	for _, t := range tiers {
		t.ProjectUID = projectUID
	}

	return tiers, nil
}

// GetTier returns the MembershipTier identified by tierUID. The cache is
// consulted first; on a fresh or stale hit the cached value is returned (stale
// hits also trigger a background refresh). On an expired entry or a miss the
// record is fetched from Salesforce by decoding the UUID v8 back to its
// Salesforce Product2 SFID, written to KV, and returned.
func (r *MemberReader) GetTier(ctx context.Context, tierUID string) (*model.MembershipTier, error) {
	result, err := r.cache.GetTier(ctx, tierUID)
	if err != nil {
		slog.WarnContext(ctx, "cache read error for tier; falling through to Salesforce",
			"tier_uid", tierUID,
			"error", err,
		)
	} else {
		switch result.Status {
		case nats.CacheStatusFresh, nats.CacheStatusStale:
			// Treat a cached entry with an empty ProjectUID as a miss so that
			// poisoned entries written before the resolver was reachable are
			// self-healed on the next fetch.
			if result.Value.ProjectUID == "" {
				slog.WarnContext(ctx, "cached tier has empty project_uid; treating as cache miss",
					"tier_uid", tierUID,
				)
				break
			}
			if result.Status == nats.CacheStatusStale {
				r.refreshInBackground(func(ctx context.Context) error {
					_, fetchErr := r.fetchTierFromSalesforce(ctx, tierUID)
					return fetchErr
				})
			}
			return result.Value, nil
		}
		// CacheStatusExpired and CacheStatusMiss fall through to Salesforce.
	}

	return r.fetchTierFromSalesforce(ctx, tierUID)
}

// fetchTierFromSalesforce fetches a single tier from Salesforce by decoding the
// UID to a SFID, writes the result to the KV cache, and returns it. ProjectUID
// is resolved from the tier's ProjectSlug via the resolver.
func (r *MemberReader) fetchTierFromSalesforce(ctx context.Context, tierUID string) (*model.MembershipTier, error) {
	sfid, err := sfuuid.Normalize18(tierUID)
	if err != nil {
		// Defensive: uid was not a valid SFID — use as-is and let Salesforce reject.
		sfid = tierUID
	}

	tier, err := r.tiers.FetchTierBySFID(ctx, sfid)
	if err != nil {
		return nil, fmt.Errorf("fetching tier %s from Salesforce: %w", tierUID, err)
	}
	if tier == nil {
		return nil, errs.NewNotFound("tier not found", fmt.Errorf("uid: %s", tierUID))
	}

	// Resolve the v2 project UID from the slug so ProjectUID is always populated.
	if tier.ProjectSlug != "" {
		if projectUID, resolveErr := r.resolver.UIDFromSlug(ctx, tier.ProjectSlug); resolveErr == nil {
			tier.ProjectUID = projectUID
		} else {
			slog.WarnContext(ctx, "failed to resolve project UID from slug for tier",
				"tier_uid", tier.UID,
				"project_slug", tier.ProjectSlug,
				"error", resolveErr,
			)
		}
	}

	// Only cache the tier when ProjectUID is resolved. A record with an empty
	// ProjectUID would poison the cache and cause all subsequent ownership checks
	// to fail without triggering a re-fetch from Salesforce.
	if tier.ProjectUID != "" {
		if putErr := r.cache.PutTier(ctx, tier); putErr != nil {
			slog.WarnContext(ctx, "failed to cache tier after Salesforce fetch",
				"tier_uid", tier.UID,
				"error", putErr,
			)
		}
	} else {
		slog.WarnContext(ctx, "skipping tier cache write: ProjectUID is empty",
			"tier_uid", tier.UID,
			"project_slug", tier.ProjectSlug,
		)
	}

	return tier, nil
}

// ─── Memberships ─────────────────────────────────────────────────────────────

// ListMembershipsForProject returns a single logical page of ProjectMembership
// records for the given v2 project UID. Results are served from the NATS KV
// batch cache when available; on a miss the first SF batch is fetched
// synchronously and the remainder is swept in the background.
//
// Cache layout (see nats.MembershipBatchCacheKey):
//   - Batch 0: keyed by (templateRef, sfid, sort, [tierSFID], [search:<term>], "0").
//     This is the only entry eligible for stale-while-revalidate; refreshing it
//     starts a new linked chain under a fresh iterator.
//   - Batch N (N>0): keyed by (templateRef, sfid, sort, [tierSFID], [search:<term>], iterator)
//     where iterator is the NextBatchIterator value stored in batch N-1.
//   - The search segment is only present when CompanyNameSearch is non-empty.
//     It is always lowercased so "Google" and "google" share the same cache entry.
//
// The consumer-facing PageCursor encodes (BatchIndex, BatchOffset, PageSize,
// NextBatchIterator) so that the next request can locate its batch directly
// without holding any live Salesforce locator.
func (r *MemberReader) ListMembershipsForProject(ctx context.Context, projectUID string, filters model.MembershipFilters, pageSize int) (model.MembershipPage, error) {
	sfid, err := r.resolver.SFIDFromUID(ctx, projectUID)
	if err != nil {
		return model.MembershipPage{}, fmt.Errorf("resolving project SFID for UID %s: %w", projectUID, err)
	}

	// Decode the inbound cursor. An empty PageToken means first page of batch 0.
	cursor, err := DecodeCursor(filters.PageToken)
	if err != nil {
		return model.MembershipPage{}, fmt.Errorf("invalid page token: %w", err)
	}
	// Honour the page size agreed when the sequence was started.
	if cursor.PageSize > 0 {
		pageSize = cursor.PageSize
	}
	pageSize = NormalizePageSize(pageSize)

	batchParams := r.membershipBatchKeyParams(sfid, filters)
	batchKey := nats.MembershipBatchCacheKey(soqlMembershipsTemplateRef, batchParams, cursor.BatchIndex, cursor.NextBatchIterator)

	// ── Cache read ───────────────────────────────────────────────────────────
	cacheResult, cacheErr := r.cache.GetMembershipBatch(ctx, batchKey)
	if cacheErr != nil {
		slog.WarnContext(ctx, "membership batch cache read error; falling through to Salesforce",
			"cache_key", batchKey,
			"error", cacheErr,
		)
	} else {
		switch cacheResult.Status {
		case nats.CacheStatusFresh:
			page, decodeErr := sliceCachedBatch(cacheResult.Value, projectUID, cursor, pageSize, cursor.BatchIndex, batchParams)
			if decodeErr == nil {
				return page, nil
			}
			slog.WarnContext(ctx, "failed to decode fresh cached batch; re-fetching",
				"cache_key", batchKey, "error", decodeErr,
			)
		case nats.CacheStatusStale:
			page, decodeErr := sliceCachedBatch(cacheResult.Value, projectUID, cursor, pageSize, cursor.BatchIndex, batchParams)
			if decodeErr == nil {
				// Only batch 0 triggers a full refresh; later batches are
				// rewritten as a side-effect of refreshing batch 0.
				if cursor.BatchIndex == 0 {
					r.refreshInBackground(func(bgCtx context.Context) error {
						return r.fetchAndCacheAllBatches(bgCtx, projectUID, sfid, filters, batchParams)
					})
				}
				return page, nil
			}
			slog.WarnContext(ctx, "failed to decode stale cached batch; re-fetching",
				"cache_key", batchKey, "error", decodeErr,
			)
		}
		// CacheStatusExpired and CacheStatusMiss fall through to Salesforce.
	}

	// ── Cache miss / expired: fetch batch 0 from Salesforce ──────────────────
	//
	// We always re-fetch from batch 0 on a miss, regardless of which batch the
	// cursor points to. This ensures coherence: a new linked chain is written
	// tail-first by fetchAndCacheAllBatches, and the updated batch 0 entry is
	// returned to the caller.
	if err := r.fetchAndCacheAllBatches(ctx, projectUID, sfid, filters, batchParams); err != nil {
		return model.MembershipPage{}, err
	}

	// Read back the freshly-written batch 0 to build the response. If the
	// write just above succeeded this should always be a fresh hit.
	freshResult, readErr := r.cache.GetMembershipBatch(ctx, nats.MembershipBatchCacheKey(soqlMembershipsTemplateRef, batchParams, 0, ""))
	if readErr != nil || freshResult.Status == nats.CacheStatusMiss {
		return model.MembershipPage{}, fmt.Errorf("failed to read back batch 0 after Salesforce fetch for project %s", projectUID)
	}

	// After a miss we always serve from batch 0, offset 0 — the caller's
	// cursor is stale (its batch was evicted) so we reset to the beginning.
	resetCursor := PageCursor{PageSize: pageSize}
	page, decodeErr := sliceCachedBatch(freshResult.Value, projectUID, resetCursor, pageSize, 0, batchParams)
	if decodeErr != nil {
		return model.MembershipPage{}, fmt.Errorf("decoding freshly-fetched batch 0 for project %s: %w", projectUID, decodeErr)
	}
	return page, nil
}

// fetchAndCacheAllBatches fetches the first SF batch synchronously, then
// follows any remaining locators in a background goroutine. Batches are written
// tail-first: batch N+1 is written before batch N so that batch N's
// NextBatchIterator is always valid by the time it is readable. Batch 0 is
// written last (synchronously, before this function returns) so the caller can
// immediately read it back.
func (r *MemberReader) fetchAndCacheAllBatches(
	ctx context.Context,
	projectUID string,
	projectSFID string,
	filters model.MembershipFilters,
	batchParams []string,
) error {
	firstBatch, err := r.memberships.FetchFirstMembershipBatch(ctx, projectSFID, filters)
	if err != nil {
		return fmt.Errorf("fetching first membership batch for project %s: %w", projectUID, err)
	}

	for _, m := range firstBatch.Records {
		m.ProjectUID = projectUID
	}

	if firstBatch.SFLocator == "" {
		// Single batch: write directly and we are done.
		key0 := nats.MembershipBatchCacheKey(soqlMembershipsTemplateRef, batchParams, 0, "")
		return r.cache.PutMembershipBatch(ctx, key0, firstBatch.Records, "", firstBatch.TotalSize)
	}

	// More batches exist: start the background sweep. The sweep writes all
	// subsequent batches tail-first, then calls writeBatch0 with the iterator
	// for batch 1 so the chain is fully linked before batch 0 is readable.
	batch0Records := firstBatch.Records
	locator := firstBatch.SFLocator
	totalSize := firstBatch.TotalSize

	writeBatch0 := func(bgCtx context.Context, nextIter string) error {
		key0 := nats.MembershipBatchCacheKey(soqlMembershipsTemplateRef, batchParams, 0, "")
		return r.cache.PutMembershipBatch(bgCtx, key0, batch0Records, nextIter, totalSize)
	}

	// sweepRemaining follows all locators starting from the given one,
	// writes each batch tail-first (N+1 before N), and finally calls
	// writeBatch0 with the iterator linking batch 0 → batch 1.
	sweepRemaining := func(bgCtx context.Context) error {
		remaining, _, fetchErr := QueryAllPages[soqlAsset](bgCtx, r.memberships.client, "", locator)
		if fetchErr != nil {
			return fmt.Errorf("sweeping remaining membership batches for project %s: %w", projectUID, fetchErr)
		}

		// Convert all remaining records.
		converted := make([]*model.ProjectMembership, 0, len(remaining))
		for _, asset := range remaining {
			m, convErr := convertSOQLToProjectMembership(asset)
			if convErr != nil {
				slog.WarnContext(bgCtx, "skipping membership with invalid SFID during sweep",
					"sfid", asset.ID, "error", convErr,
				)
				continue
			}
			m.ProjectUID = projectUID
			converted = append(converted, m)
		}

		// Split into sfQueryBatchSize chunks and write tail-first.
		chunks := splitIntoBatches(converted, sfQueryBatchSize)
		iterators := make([]string, len(chunks))
		for i := range iterators {
			iterators[i] = newBatchIterator()
		}

		// Write from the last chunk backwards so each entry's iterator is
		// valid before the preceding entry references it.
		nextIter := "" // last chunk has no successor
		for i := len(chunks) - 1; i >= 0; i-- {
			// Batch index in the overall sequence is i+1 (batch 0 is the first
			// SF fetch; these are batches 1..N).
			key := nats.MembershipBatchCacheKey(soqlMembershipsTemplateRef, batchParams, i+1, iterators[i])
			if putErr := r.cache.PutMembershipBatch(bgCtx, key, chunks[i], nextIter, 0); putErr != nil {
				slog.WarnContext(bgCtx, "failed to write membership batch to cache",
					"batch_index", i+1, "cache_key", key, "error", putErr,
				)
				// Do not propagate: partial cache coverage is acceptable; the
				// miss path will re-sweep from batch 0.
				return nil
			}
			nextIter = iterators[i]
		}

		// Now write batch 0 with the iterator linking it to batch 1.
		return writeBatch0(bgCtx, nextIter)
	}

	// Write batch 0 without a next-iterator first so it is immediately
	// readable (logical pages within batch 0 can be served right away).
	// The sweep will overwrite it with the correct iterator once batch 1
	// is written.
	key0 := nats.MembershipBatchCacheKey(soqlMembershipsTemplateRef, batchParams, 0, "")
	if putErr := r.cache.PutMembershipBatch(ctx, key0, batch0Records, "", totalSize); putErr != nil {
		return fmt.Errorf("writing batch 0 for project %s: %w", projectUID, putErr)
	}

	r.refreshInBackground(sweepRemaining)
	return nil
}

// membershipBatchKeyParams returns the stable SOQL key parameters for the given
// project SFID and filters. These are the same for all batches in a sequence.
func (r *MemberReader) membershipBatchKeyParams(projectSFID string, filters model.MembershipFilters) []string {
	params := []string{projectSFID, string(filters.EffectiveSortOrder())}
	if filters.TierUID != "" {
		tierSFID, err := sfuuid.Normalize18(filters.TierUID)
		if err != nil {
			tierSFID = filters.TierUID
		}
		params = append(params, tierSFID)
	}
	if filters.CompanyNameSearch != "" {
		// CompanyNameSearch is already lowercased by the caller before being
		// stored in MembershipFilters, so it is safe to use directly here.
		// This ensures "Google" and "google" always produce the same cache key
		// and the same SOQL LIKE predicate.
		params = append(params, "search:"+filters.CompanyNameSearch)
	}
	return params
}

// sliceCachedBatch decodes a MembershipBatchCacheEntry, stamps projectUID onto
// every record, slices the logical page at [cursor.BatchOffset :
// cursor.BatchOffset+pageSize], and builds the next-page cursor. batchIndex is
// the 0-based index of this batch in the sequence; batchParams is used to
// construct the next-batch cache key segment embedded in the cursor.
//
// cursor.NextBatchIterator is the iterator used to locate THIS batch (for
// batch N>0). It must be threaded through into intra-batch continuation cursors
// so subsequent requests within the same batch can still find it by key.
func sliceCachedBatch(
	entry *nats.MembershipBatchCacheEntry,
	projectUID string,
	cursor PageCursor,
	pageSize int,
	batchIndex int,
	batchParams []string,
) (model.MembershipPage, error) {
	if entry == nil {
		return model.MembershipPage{}, fmt.Errorf("nil cache entry")
	}

	records, err := entry.DecodeBatchRecords()
	if err != nil {
		return model.MembershipPage{}, fmt.Errorf("decoding batch records: %w", err)
	}
	for _, m := range records {
		m.ProjectUID = projectUID
	}

	start := cursor.BatchOffset
	if start > len(records) {
		start = len(records)
	}
	end := start + pageSize
	if end > len(records) {
		end = len(records)
	}
	page := records[start:end]

	nextOffset := end
	var nextCursor *PageCursor

	if nextOffset < len(records) {
		// More records remain in this batch — advance offset, stay in same batch.
		// For batch N>0 we must carry cursor.NextBatchIterator forward so the
		// next request can still find this batch's cache entry by key.
		nextCursor = &PageCursor{
			BatchIndex:        batchIndex,
			BatchOffset:       nextOffset,
			PageSize:          pageSize,
			NextBatchIterator: cursor.NextBatchIterator,
		}
	} else if entry.NextBatchIterator != "" {
		// Exhausted this batch and a next batch exists. Embed the iterator so
		// the next request can locate batch N+1 directly.
		nextCursor = &PageCursor{
			BatchIndex:        batchIndex + 1,
			BatchOffset:       0,
			PageSize:          pageSize,
			NextBatchIterator: entry.NextBatchIterator,
		}
	}
	// nil nextCursor means this is the last page across all batches.

	var nextPageToken string
	if nextCursor != nil {
		nextPageToken = EncodeCursor(*nextCursor)
	}

	return model.MembershipPage{
		Memberships:   page,
		NextPageToken: nextPageToken,
		TotalSize:     entry.TotalSize,
	}, nil
}

// newBatchIterator generates an 8-character URL-safe random token used to key
// subsequent batches in a linked cache chain.
func newBatchIterator() string {
	b := make([]byte, 6) // 6 bytes → 8 base64url chars (no padding).
	if _, err := rand.Read(b); err != nil {
		// Fallback: use a timestamp-derived string. This is safe because
		// iterator collisions only cause cache misses, not data corruption.
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// splitIntoBatches partitions records into chunks of at most chunkSize. The
// last chunk may be smaller than chunkSize.
func splitIntoBatches(records []*model.ProjectMembership, chunkSize int) [][]*model.ProjectMembership {
	if chunkSize <= 0 || len(records) == 0 {
		return nil
	}
	var chunks [][]*model.ProjectMembership
	for i := 0; i < len(records); i += chunkSize {
		end := i + chunkSize
		if end > len(records) {
			end = len(records)
		}
		chunks = append(chunks, records[i:end])
	}
	return chunks
}

// GetMembership returns the ProjectMembership identified by membershipUID. The
// cache is consulted first; on a fresh or stale hit the cached value is returned
// (stale hits also trigger a background refresh). On an expired entry or a miss
// the record is fetched from Salesforce by decoding the UUID v8 back to its
// Salesforce Asset SFID, written to KV, and returned.
func (r *MemberReader) GetMembership(ctx context.Context, membershipUID string) (*model.ProjectMembership, error) {
	result, err := r.cache.GetMembership(ctx, membershipUID)
	if err != nil {
		slog.WarnContext(ctx, "cache read error for membership; falling through to Salesforce",
			"membership_uid", membershipUID,
			"error", err,
		)
	} else {
		switch result.Status {
		case nats.CacheStatusFresh, nats.CacheStatusStale:
			// Treat a cached entry with an empty ProjectUID as a miss so that
			// poisoned entries written before the resolver was reachable are
			// self-healed on the next fetch.
			if result.Value.ProjectUID == "" {
				slog.WarnContext(ctx, "cached membership has empty project_uid; treating as cache miss",
					"membership_uid", membershipUID,
				)
				break
			}
			if result.Status == nats.CacheStatusStale {
				readRevision := result.Revision
				r.refreshInBackground(func(ctx context.Context) error {
					_, fetchErr := r.fetchMembershipFromSalesforce(ctx, membershipUID, readRevision)
					return fetchErr
				})
			}
			return result.Value, nil
		}
		// CacheStatusExpired and CacheStatusMiss fall through to Salesforce.
	}

	// result.Revision is 0 on a miss or cache read error, making the write-back
	// create-only; on an expired or poisoned entry it conditions the overwrite.
	return r.fetchMembershipFromSalesforce(ctx, membershipUID, result.Revision)
}

// fetchMembershipFromSalesforce fetches a single membership from Salesforce by
// decoding the UID to a SFID, writes the result to the KV cache conditioned on
// readRevision (the KV revision the caller read the entry at; 0 when none
// existed), and returns it. A Conflict on the write-back means the entry was
// evicted or rewritten while this fetch was in flight: the possibly-stale
// result must not repopulate the cache, so the write is skipped. ProjectUID is
// resolved from the membership's ProjectSlug via the resolver.
func (r *MemberReader) fetchMembershipFromSalesforce(ctx context.Context, membershipUID string, readRevision uint64) (*model.ProjectMembership, error) {
	sfid, err := sfuuid.Normalize18(membershipUID)
	if err != nil {
		// Defensive: uid was not a valid SFID — use as-is and let Salesforce reject.
		sfid = membershipUID
	}

	membership, err := r.memberships.FetchMembershipBySFID(ctx, sfid)
	if err != nil {
		return nil, fmt.Errorf("fetching membership %s from Salesforce: %w", membershipUID, err)
	}
	if membership == nil {
		return nil, errs.NewNotFound("membership not found", fmt.Errorf("uid: %s", membershipUID))
	}

	// Resolve the v2 project UID from the slug so ProjectUID is always populated.
	if membership.ProjectSlug != "" {
		if projectUID, resolveErr := r.resolver.UIDFromSlug(ctx, membership.ProjectSlug); resolveErr == nil {
			membership.ProjectUID = projectUID
		} else {
			slog.WarnContext(ctx, "failed to resolve project UID from slug for membership",
				"membership_uid", membership.UID,
				"project_slug", membership.ProjectSlug,
				"error", resolveErr,
			)
		}
	}

	// Only cache the membership when ProjectUID is resolved. A record with an
	// empty ProjectUID would poison the cache and cause all subsequent ownership
	// checks to fail without triggering a re-fetch from Salesforce.
	if membership.ProjectUID != "" {
		if putErr := r.cache.PutMembershipAtRevision(ctx, membership, readRevision); putErr != nil {
			if errs.IsConflict(putErr) {
				slog.InfoContext(ctx, "membership cache write-back skipped: entry changed since read",
					"membership_uid", membership.UID,
				)
			} else {
				slog.WarnContext(ctx, "failed to cache membership after Salesforce fetch",
					"membership_uid", membership.UID,
					"error", putErr,
				)
			}
		}
	} else {
		slog.WarnContext(ctx, "skipping membership cache write: ProjectUID is empty",
			"membership_uid", membership.UID,
			"project_slug", membership.ProjectSlug,
		)
	}

	return membership, nil
}

// ─── Key contacts ─────────────────────────────────────────────────────────────

// ListKeyContactsForMembership returns all KeyContact records for the
// given membership UID. The contacts are cached as a group under the membership
// UID; a fresh or stale cache hit is served directly (stale hits trigger a
// background refresh). A miss or expired entry triggers a SOQL fetch by Asset
// SFID.
func (r *MemberReader) ListKeyContactsForMembership(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	result, err := r.cache.GetKeyContactsForMembership(ctx, membershipUID)
	if err != nil {
		slog.WarnContext(ctx, "cache read error for key contacts; falling through to Salesforce",
			"membership_uid", membershipUID,
			"error", err,
		)
	} else {
		switch result.Status {
		case nats.CacheStatusFresh:
			return result.Value, nil
		case nats.CacheStatusStale:
			r.refreshInBackground(func(ctx context.Context) error {
				_, fetchErr := r.fetchKeyContactsFromSalesforce(ctx, membershipUID)
				return fetchErr
			})
			return result.Value, nil
		}
		// CacheStatusExpired and CacheStatusMiss fall through to Salesforce.
	}

	return r.fetchKeyContactsFromSalesforce(ctx, membershipUID)
}

// fetchKeyContactsFromSalesforce fetches all key contacts for a membership from
// Salesforce by decoding the membership UID to an Asset SFID, writes the result
// to the KV cache, and returns it. ProjectUID is resolved from each contact's
// ProjectSlug via the resolver.
func (r *MemberReader) fetchKeyContactsFromSalesforce(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	sfid, err := sfuuid.Normalize18(membershipUID)
	if err != nil {
		// Defensive: uid was not a valid SFID — use as-is and let Salesforce reject.
		sfid = membershipUID
	}

	contacts, err := r.contacts.FetchKeyContactsByAssetSFID(ctx, sfid)
	if err != nil {
		return nil, fmt.Errorf("fetching key contacts for membership %s from Salesforce: %w", membershipUID, err)
	}

	// Resolve the v2 project UID for each contact from its ProjectSlug.
	for _, c := range contacts {
		if c.ProjectSlug != "" {
			if projectUID, resolveErr := r.resolver.UIDFromSlug(ctx, c.ProjectSlug); resolveErr == nil {
				c.ProjectUID = projectUID
			} else {
				slog.WarnContext(ctx, "failed to resolve project UID from slug for key contact",
					"contact_uid", c.UID,
					"project_slug", c.ProjectSlug,
					"error", resolveErr,
				)
			}
		}
	}

	if putErr := r.cache.PutKeyContactsForMembership(ctx, membershipUID, contacts); putErr != nil {
		slog.WarnContext(ctx, "failed to cache key contacts after Salesforce fetch",
			"membership_uid", membershipUID,
			"error", putErr,
		)
	}

	return contacts, nil
}

// GetKeyContact returns the KeyContact identified by keyContactUID. There
// is no group-level KV cache for individual contacts — the contacts bucket
// stores all contacts for a membership together. The record is fetched directly
// from Salesforce by SFID. ProjectUID is resolved from the contact's ProjectSlug
// via the resolver.
func (r *MemberReader) GetKeyContact(ctx context.Context, keyContactUID string) (*model.KeyContact, error) {
	contact, _, err := r.keyContactReader.AssembleKeyContact(ctx, keyContactUID)
	if err != nil {
		return nil, err
	}

	// Resolve the v2 project UID from the slug so ProjectUID is always populated.
	if contact.ProjectSlug != "" {
		if projectUID, resolveErr := r.resolver.UIDFromSlug(ctx, contact.ProjectSlug); resolveErr == nil {
			contact.ProjectUID = projectUID
		} else {
			slog.WarnContext(ctx, "failed to resolve project UID from slug for key contact",
				"contact_uid", contact.UID,
				"project_slug", contact.ProjectSlug,
				"error", resolveErr,
			)
		}
	}

	return contact, nil
}

// ListKeyContactsForOrg returns all KeyContact records across every membership
// backed by the given 18-char Account SFID. Implemented as a two-hop:
// FetchMembershipsByAccountSFID to get all memberships, then
// FetchKeyContactsByAssetSFID per membership. N+1 is bounded by the number of
// memberships an org holds (small) and this path is only used on rare events
// (invite acceptance, key-contact delete guard).
func (r *MemberReader) ListKeyContactsForOrg(ctx context.Context, orgSFID string) ([]*model.KeyContact, error) {
	memberships, err := r.memberships.FetchMembershipsByAccountSFID(ctx, orgSFID)
	if err != nil {
		return nil, err
	}

	var out []*model.KeyContact
	for _, m := range memberships {
		kcs, err := r.contacts.FetchKeyContactsByAssetSFID(ctx, m.UID)
		if err != nil {
			return nil, err
		}
		out = append(out, kcs...)
	}
	return out, nil
}

// ─── Readiness ───────────────────────────────────────────────────────────────

// IsReady reports whether the underlying NATS KV cache is reachable. Salesforce
// connectivity is not checked here — failures surface on the first SOQL call.
func (r *MemberReader) IsReady(ctx context.Context) error {
	return r.cache.IsReady(ctx)
}
