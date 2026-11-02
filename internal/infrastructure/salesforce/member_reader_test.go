// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

const (
	memberReaderTestAssetSFID    = "02iB0000009ABCDIA4"
	memberReaderTestAccountSFID  = "0012M00002FyYwwQAF"
	memberReaderTestProduct2SFID = "01t2M000009ABCDQA4"
	memberReaderTestProjectSFID  = "a092M000001XYZAQA4"
	memberReaderTestProjectSlug  = "linux-foundation"
	memberReaderTestProjectUID   = "proj-uid-resolved"
)

// memberReaderTestAssetJSON builds a single-record SOQL Asset response body
// (as returned by the /query endpoint) for the fixed test SFIDs above, with
// the given CompanyName so tests can distinguish an initial fetch from a
// refreshed one.
func memberReaderTestAssetJSON(companyName string) string {
	return fmt.Sprintf(`{"totalSize":1,"done":true,"records":[{
		"Id":%q,
		"Name":"Membership",
		"Status":"Active",
		"AccountId":%q,
		"Product2Id":%q,
		"Year__c":"2025",
		"Tier__c":"Gold",
		"Auto_Renew__c":true,
		"Price":50000.0,
		"Annual_Full_Price__c":50000.0,
		"Projects__c":%q,
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-20T00:00:00.000+0000",
		"Account":{"Id":%q,"Name":%q},
		"Product2":{"Id":%q,"Name":"Gold Membership"},
		"Projects__r":{"Id":%q,"Slug__c":%q}
	}]}`,
		memberReaderTestAssetSFID, memberReaderTestAccountSFID, memberReaderTestProduct2SFID, memberReaderTestProjectSFID,
		memberReaderTestAccountSFID, companyName,
		memberReaderTestProduct2SFID,
		memberReaderTestProjectSFID, memberReaderTestProjectSlug,
	)
}

// fakeSlugResolver resolves memberReaderTestProjectSlug to a fixed project
// UID; any other slug is a NotFound. Only UIDFromSlug is exercised by
// MemberReader's membership fetch path.
type fakeSlugResolver struct {
	port.ProjectResolver
}

func (fakeSlugResolver) UIDFromSlug(_ context.Context, slug string) (string, error) {
	if slug == memberReaderTestProjectSlug {
		return memberReaderTestProjectUID, nil
	}
	return "", errs.NewNotFound("slug not found")
}

// newMemberReaderTestReader builds a MemberReader whose memberships repo is
// backed by a routed fake Salesforce transport and whose cache is the given
// Storage. Only the fields exercised by GetMembership are populated.
func newMemberReaderTestReader(t *testing.T, rt http.RoundTripper, cache *nats.Storage) *MemberReader {
	t.Helper()
	return &MemberReader{
		memberships: NewMembershipRepo(fakeSalesforce(t, rt)),
		resolver:    fakeSlugResolver{},
		cache:       cache,
	}
}

func routedTransport(companyName string) *routingTransport {
	rt := &routingTransport{}
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))
	rt.route("/query", fakeResponse(http.StatusOK, memberReaderTestAssetJSON(companyName), nil))
	return rt
}

// TestGetMembership_MissAfterTombstone_WriteBackUsesTombstoneRevision covers
// the miss/expired path: after an eviction, GetMembership's cache read
// carries the delete marker's (non-zero) revision, and the synchronous
// write-back into fetchMembershipFromSalesforce must be conditioned on that
// revision rather than a hardcoded 0, or it would conflict against the
// tombstone and silently fail to repopulate the cache.
func TestGetMembership_MissAfterTombstone_WriteBackUsesTombstoneRevision(t *testing.T) {
	ctx := context.Background()
	uid, err := sfuuid.Normalize18(memberReaderTestAssetSFID)
	require.NoError(t, err)

	cache := nats.NewMemoryStorage()
	require.NoError(t, cache.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-old"}, 0))
	require.NoError(t, cache.DeleteMembership(ctx, uid))

	reader := newMemberReaderTestReader(t, routedTransport("Refreshed Corp"), cache)

	membership, err := reader.GetMembership(ctx, uid)
	require.NoError(t, err)
	require.NotNil(t, membership)
	assert.Equal(t, "Refreshed Corp", membership.CompanyName)

	result, err := cache.GetMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, nats.CacheStatusFresh, result.Status, "write-back conditioned on the tombstone revision must succeed")
	assert.Equal(t, "Refreshed Corp", result.Value.CompanyName)
}

// TestGetMembership_StaleHit_BackgroundRefreshUsesReadRevision covers the
// stale-hit path: the value read at the stale revision must be threaded
// into the background refresh's write-back. A refresh using a hardcoded 0
// instead would conflict against the still-present entry and silently fail,
// so the cache would never converge on the refreshed value.
func TestGetMembership_StaleHit_BackgroundRefreshUsesReadRevision(t *testing.T) {
	ctx := context.Background()
	uid, err := sfuuid.Normalize18(memberReaderTestAssetSFID)
	require.NoError(t, err)

	// A negative StaleDuration makes every write immediately stale, including
	// the refreshed entry itself, so the assertion below checks content
	// rather than a Fresh status.
	cache := nats.NewMemoryStorageWithTTL(nats.TTLConfig{StaleDuration: -time.Second, ExpiresDuration: time.Hour})
	require.NoError(t, cache.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: memberReaderTestProjectUID, CompanyName: "Stale Corp"}, 0))

	reader := newMemberReaderTestReader(t, routedTransport("Refreshed Corp"), cache)

	membership, err := reader.GetMembership(ctx, uid)
	require.NoError(t, err)
	require.NotNil(t, membership)
	assert.Equal(t, "Stale Corp", membership.CompanyName, "a stale hit must return the cached value immediately")

	require.Eventually(t, func() bool {
		result, err := cache.GetMembership(ctx, uid)
		return err == nil && result.Value != nil && result.Value.CompanyName == "Refreshed Corp"
	}, time.Second, 10*time.Millisecond, "background refresh must converge on the refreshed value using the read revision")
}

// TestFetchMembershipFromSalesforce_ConflictOnWriteBack_IsNonFatal covers the
// Conflict branch: a write-back conditioned on a stale readRevision must not
// propagate as an error to the caller (the fetched record is still returned),
// and the cache entry must be left untouched rather than overwritten with a
// possibly-stale record.
func TestFetchMembershipFromSalesforce_ConflictOnWriteBack_IsNonFatal(t *testing.T) {
	ctx := context.Background()
	uid, err := sfuuid.Normalize18(memberReaderTestAssetSFID)
	require.NoError(t, err)

	cache := nats.NewMemoryStorage()
	require.NoError(t, cache.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-existing"}, 0))

	reader := newMemberReaderTestReader(t, routedTransport("Refreshed Corp"), cache)

	// readRevision 0 is stale: the entry above is already at revision 1.
	membership, err := reader.fetchMembershipFromSalesforce(ctx, uid, 0)
	require.NoError(t, err, "a write-back Conflict must not be returned to the caller")
	require.NotNil(t, membership)
	assert.Equal(t, "Refreshed Corp", membership.CompanyName, "the freshly-fetched record is still returned despite the skipped write-back")

	result, err := cache.GetMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, "p-existing", result.Value.ProjectUID, "the cache must not be overwritten by the losing write-back")
}

// gzipRecords builds the RecordsGZ payload a MembershipBatchCacheEntry would
// hold, mirroring nats.encodeBatchRecords (unexported, so duplicated here).
func gzipRecords(t *testing.T, records []*model.ProjectMembership) []byte {
	t.Helper()
	raw, err := json.Marshal(records)
	require.NoError(t, err)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err = gw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func membershipsFixture(n int) []*model.ProjectMembership {
	out := make([]*model.ProjectMembership, n)
	for i := range out {
		out[i] = &model.ProjectMembership{UID: "asset-sfid"}
	}
	return out
}

// TestSliceCachedBatch_CursorFingerprintRoundTrip verifies the contract that
// ListMembershipsForProject's page_token validation depends on: a cursor
// minted by sliceCachedBatch for a given batchParams must pass the exact
// fingerprint check performed on the next request for that same batchParams
// — "cursor.FilterFingerprint != FingerprintCursor(batchParams,
// cursor.BatchIndex, cursor.NextBatchIterator)" in member_reader.go — across
// all three cursor shapes it can mint: an intra-batch continuation, a
// batch-transition, and the no-more-pages (nil) case. This is the round-trip
// coverage flagged as missing by the round-7 general-reviewer: the fix is
// here as a pure-function test since sliceCachedBatch takes a plain
// *nats.MembershipBatchCacheEntry and has no cache/repo dependency to mock.
func TestSliceCachedBatch_CursorFingerprintRoundTrip(t *testing.T) {
	batchParams := []string{"project-sfid", "created_desc", "tier-sfid"}

	t.Run("intra-batch continuation cursor passes the next request's fingerprint check", func(t *testing.T) {
		entry := &nats.MembershipBatchCacheEntry{
			RecordsGZ: gzipRecords(t, membershipsFixture(25)),
			TotalSize: 25,
		}
		cursor := PageCursor{BatchIndex: 0, NextBatchIterator: ""}

		// pageSize must be a member of ValidPageSizes: DecodeCursor rejects
		// any other value (see soql_pager.go), and every real call site
		// already normalizes pageSize before reaching sliceCachedBatch.
		page, err := sliceCachedBatch(entry, "project-uid", cursor, 10, 0, batchParams)
		require.NoError(t, err)
		require.NotEmpty(t, page.NextPageToken)

		next, err := DecodeCursor(page.NextPageToken)
		require.NoError(t, err)
		assert.Equal(t, 0, next.BatchIndex)
		assert.Equal(t, 10, next.BatchOffset)

		// Exactly the check ListMembershipsForProject performs on the next call.
		got := FingerprintCursor(batchParams, next.BatchIndex, next.NextBatchIterator)
		assert.Equal(t, next.FilterFingerprint, got, "minted cursor must pass its own fingerprint check on replay")
	})

	t.Run("batch-transition cursor passes the next request's fingerprint check", func(t *testing.T) {
		entry := &nats.MembershipBatchCacheEntry{
			RecordsGZ:         gzipRecords(t, membershipsFixture(10)),
			NextBatchIterator: "nextiter",
			TotalSize:         15,
		}
		cursor := PageCursor{BatchIndex: 0, NextBatchIterator: ""}

		// pageSize == len(records): exhausts batch 0 exactly, so the cursor
		// must point at batch 1 (not an intra-batch offset). Must be a member
		// of ValidPageSizes (see soql_pager.go's DecodeCursor).
		page, err := sliceCachedBatch(entry, "project-uid", cursor, 10, 0, batchParams)
		require.NoError(t, err)
		require.NotEmpty(t, page.NextPageToken)

		next, err := DecodeCursor(page.NextPageToken)
		require.NoError(t, err)
		assert.Equal(t, 1, next.BatchIndex)
		assert.Equal(t, 0, next.BatchOffset)
		assert.Equal(t, "nextiter", next.NextBatchIterator)

		got := FingerprintCursor(batchParams, next.BatchIndex, next.NextBatchIterator)
		assert.Equal(t, next.FilterFingerprint, got, "batch-transition cursor must pass its own fingerprint check on replay")
	})

	t.Run("last page across all batches mints no cursor", func(t *testing.T) {
		entry := &nats.MembershipBatchCacheEntry{
			RecordsGZ: gzipRecords(t, membershipsFixture(2)),
			TotalSize: 2,
		}
		cursor := PageCursor{BatchIndex: 0, NextBatchIterator: ""}

		page, err := sliceCachedBatch(entry, "project-uid", cursor, 10, 0, batchParams)
		require.NoError(t, err)
		assert.Empty(t, page.NextPageToken)
	})

	t.Run("a token minted under different batch-key params fails the fingerprint check", func(t *testing.T) {
		entry := &nats.MembershipBatchCacheEntry{
			RecordsGZ: gzipRecords(t, membershipsFixture(25)),
			TotalSize: 25,
		}
		cursor := PageCursor{BatchIndex: 0, NextBatchIterator: ""}

		page, err := sliceCachedBatch(entry, "project-uid", cursor, 10, 0, batchParams)
		require.NoError(t, err)
		minted, err := DecodeCursor(page.NextPageToken)
		require.NoError(t, err)

		// Simulates the same page_token resent with a changed tier_uid: the
		// recomputed batchParams differ (different tier SFID segment), so the
		// fingerprint recomputed by ListMembershipsForProject must disagree.
		changedBatchParams := []string{"project-sfid", "created_desc", "a-different-tier-sfid"}
		got := FingerprintCursor(changedBatchParams, minted.BatchIndex, minted.NextBatchIterator)
		assert.NotEqual(t, minted.FilterFingerprint, got, "a token minted under one tier_uid must not validate against another")
	})
}
