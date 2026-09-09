// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
)

// White-box tests for the coverage-aware sibling listers, the Inactive-path
// PendingRevoke drain, and the CDC delete replay-cursor hold.

// stubSiblingLister answers every membership with the same slice or error.
type stubSiblingLister struct {
	siblings []*model.KeyContact
	err      error
}

func (s stubSiblingLister) ListKeyContactsForMembership(context.Context, string) ([]*model.KeyContact, error) {
	return s.siblings, s.err
}

// ── coverageAwareLister ───────────────────────────────────────────────────────

func TestCoverageAwareLister_CoveredEmptyMembership_IsCertain(t *testing.T) {
	lister := coverageAwareLister{
		covered: map[string]struct{}{"asset-1": {}},
		inner:   stubSiblingLister{},
	}

	siblings, err := lister.ListKeyContactsForMembership(context.Background(), "asset-1")

	require.NoError(t, err, "a covered membership with no siblings is a certain answer")
	assert.Empty(t, siblings)
}

func TestCoverageAwareLister_UncoveredNoFallback_Errors(t *testing.T) {
	lister := coverageAwareLister{
		covered: map[string]struct{}{"asset-1": {}},
		inner:   stubSiblingLister{},
	}

	_, err := lister.ListKeyContactsForMembership(context.Background(), "asset-other")

	require.Error(t, err, "an uncovered membership must not read as sibling-free")
	assert.ErrorIs(t, err, errSiblingScanUncovered)
}

func TestCoverageAwareLister_UncoveredWithFallback_ServesLive(t *testing.T) {
	live := stubSiblingLister{siblings: []*model.KeyContact{{UID: "kc-live"}}}
	lister := coverageAwareLister{
		covered:  map[string]struct{}{"asset-1": {}},
		inner:    stubSiblingLister{},
		fallback: live,
	}

	siblings, err := lister.ListKeyContactsForMembership(context.Background(), "asset-other")

	require.NoError(t, err)
	require.Len(t, siblings, 1)
	assert.Equal(t, "kc-live", siblings[0].UID)
}

// ── batchedSiblingLister ──────────────────────────────────────────────────────

func TestBatchedSiblingLister_PrefetchesActiveMemberships(t *testing.T) {
	reader := &mock.MockKeyContactsByMembershipReader{
		Contacts: []*model.KeyContact{
			{UID: "kc-2", MembershipUID: "asset-1", Status: "Active"},
		},
	}
	kcs := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Status: "Active"},
	}

	lister := batchedSiblingLister(context.Background(), reader, nil, kcs)

	require.NotNil(t, lister, "an all-Active page must not disable the sibling check")
	require.Equal(t, 1, reader.Calls)
	siblings, err := lister.ListKeyContactsForMembership(context.Background(), "asset-1")
	require.NoError(t, err)
	require.Len(t, siblings, 1)
	assert.Equal(t, 1, reader.Calls, "a covered membership must be served from the prefetch")
}

func TestBatchedSiblingLister_UncoveredMembership_FallsBackToLiveRead(t *testing.T) {
	reader := &mock.MockKeyContactsByMembershipReader{
		Contacts: []*model.KeyContact{
			{UID: "kc-old", MembershipUID: "asset-old", Status: "Active"},
		},
	}
	kcs := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Status: "Active"},
	}

	lister := batchedSiblingLister(context.Background(), reader, nil, kcs)

	require.NotNil(t, lister)
	siblings, err := lister.ListKeyContactsForMembership(context.Background(), "asset-old")
	require.NoError(t, err, "an un-prefetched membership must be read live, not faked empty")
	require.Len(t, siblings, 1)
	assert.Equal(t, "kc-old", siblings[0].UID)
	assert.Equal(t, 2, reader.Calls, "the uncovered lookup must go back to Salesforce")
}

func TestBatchedSiblingLister_EmptyPage_ReturnsLiveLister(t *testing.T) {
	reader := &mock.MockKeyContactsByMembershipReader{
		Contacts: []*model.KeyContact{
			{UID: "kc-x", MembershipUID: "asset-x", Status: "Active"},
		},
	}

	lister := batchedSiblingLister(context.Background(), reader, nil, nil)

	require.NotNil(t, lister, "no prefetchable memberships must still leave the check enabled")
	siblings, err := lister.ListKeyContactsForMembership(context.Background(), "asset-x")
	require.NoError(t, err)
	require.Len(t, siblings, 1)
}

func TestBatchedSiblingLister_PrefetchError_ErrorsPerLookup(t *testing.T) {
	reader := &mock.MockKeyContactsByMembershipReader{Err: assert.AnError}
	kcs := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Status: "Inactive"},
	}

	lister := batchedSiblingLister(context.Background(), reader, nil, kcs)

	require.NotNil(t, lister)
	_, err := lister.ListKeyContactsForMembership(context.Background(), "asset-1")
	assert.ErrorIs(t, err, assert.AnError, "a failed prefetch must make every scan inconclusive")
}

// ── sliceSiblingLister ────────────────────────────────────────────────────────

func TestSliceSiblingLister_UncoveredWithoutReader_Errors(t *testing.T) {
	contacts := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Status: "Active"},
	}

	lister := sliceSiblingLister(contacts, nil, nil)

	_, err := lister.ListKeyContactsForMembership(context.Background(), "asset-other")
	assert.ErrorIs(t, err, errSiblingScanUncovered,
		"a membership outside the slice must read as inconclusive, not sibling-free")
}

func TestSliceSiblingLister_UncoveredWithReader_ServesLive(t *testing.T) {
	reader := &mock.MockKeyContactsByMembershipReader{
		Contacts: []*model.KeyContact{
			{UID: "kc-old", MembershipUID: "asset-old", Status: "Active"},
		},
	}
	contacts := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Status: "Active"},
	}

	lister := sliceSiblingLister(contacts, reader, nil)

	siblings, err := lister.ListKeyContactsForMembership(context.Background(), "asset-old")
	require.NoError(t, err)
	require.Len(t, siblings, 1)
	assert.Equal(t, "kc-old", siblings[0].UID)
}

// ── Inactive-path PendingRevoke drain ─────────────────────────────────────────

func TestPublishKeyContactFGA_InactiveMarkerOnlyEntry_DrainsPendingRevoke(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {
				PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-old", Username: "alice"},
				Revision:      1,
			},
		},
	}
	lister := stubSiblingLister{}

	PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:    "kc-1",
		Status: "Inactive",
	}, lister)

	removes := internalRemoveMessages(t, []any{pub.LastAccessData})
	require.Len(t, removes, 1, "the orphaned pending pair must be revoked on deactivation")
	assert.Equal(t, "asset-old", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	assert.Equal(t, 1, pub.FlushCount, "the drain must confirm delivery before clearing the marker")
	assert.Equal(t, []string{"kc-1"}, grants.Deletes, "the drained marker-only entry must be removed")
}

func TestPublishKeyContactFGA_InactiveUncertainDrain_RetainsMarker(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pending := &port.KeyContactGrantRef{MembershipUID: "asset-old", Username: "alice"}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {PendingRevoke: pending, Revision: 1},
		},
	}
	lister := stubSiblingLister{err: assert.AnError}

	PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:    "kc-1",
		Status: "Inactive",
	}, lister)

	assert.Nil(t, pub.LastAccessData, "an inconclusive scan must publish nothing")
	entry, found := grants.Entries["kc-1"]
	require.True(t, found, "the marker is the revoke's only address and must survive")
	assert.Equal(t, pending, entry.PendingRevoke)
}

// ── revokeKeyContactPairIfUnjustified recheck (T4) ────────────────────────────

func TestRevokeKeyContactPairIfUnjustified_RecheckFindsRace_RepairsGrant(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	// The first scan sees no live sibling, but a racing writer grants the same
	// pair to kc-new before the recheck runs.
	recheck := stubSiblingLister{siblings: []*model.KeyContact{
		{UID: "kc-new", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Active"},
	}}

	outcome, justifiedBy, err := revokeKeyContactPairIfUnjustified(context.Background(), pub, stubSiblingLister{}, keyContactPairRevoke{
		membershipUID: "asset-1",
		username:      "alice",
		email:         "alice@example.com",
		reason:        "test",
		flush:         true,
		recheck:       recheck,
	})

	require.NoError(t, err)
	assert.Equal(t, revokePublished, outcome, "the remove itself succeeded and is still reported as published")
	assert.Nil(t, justifiedBy, "revokePublished never carries a justifying sibling")
	assert.Equal(t, 2, pub.FlushCount, "the remove and the compensating put must each be flushed")
	assert.Equal(t, []string{"access", "flush", "access", "flush"}, pub.CallOrder,
		"a raced grant must be repaired with a compensating member_put after the remove")
	put, ok := pub.LastAccessData.(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	assert.Equal(t, "member_put", put.Operation, "the last publish must be the compensating put, not the remove")
}

func TestRevokeKeyContactPairIfUnjustified_RecheckError_OutcomeUnchanged(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	recheck := stubSiblingLister{err: assert.AnError}

	outcome, justifiedBy, err := revokeKeyContactPairIfUnjustified(context.Background(), pub, stubSiblingLister{}, keyContactPairRevoke{
		membershipUID: "asset-1",
		username:      "alice",
		email:         "alice@example.com",
		reason:        "test",
		flush:         true,
		recheck:       recheck,
	})

	require.NoError(t, err, "a failed recheck must not change the outcome or error of the already-published remove")
	assert.Equal(t, revokePublished, outcome)
	assert.Nil(t, justifiedBy)
	assert.Equal(t, 1, pub.FlushCount, "a recheck error must not publish a compensating put")
	assert.Equal(t, []string{"access", "flush"}, pub.CallOrder)
}

// ── pairDurablyOwned (T5) ──────────────────────────────────────────────────────

func TestPairDurablyOwned_UnindexedSibling_WritesEntry(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{}}
	sib := &model.KeyContact{UID: "kc-sib"}

	owned := pairDurablyOwned(context.Background(), grants, sib, "asset-1", "alice")

	assert.True(t, owned, "an unindexed sibling must have the pair written for it")
	assert.Equal(t, "asset-1", grants.Entries["kc-sib"].MembershipUID)
	assert.Equal(t, "alice", grants.Entries["kc-sib"].Username)
}

func TestPairDurablyOwned_SiblingHoldsDifferentPair_Retains(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-sib": {MembershipUID: "asset-2", Username: "bob", Revision: 5},
	}}
	sib := &model.KeyContact{UID: "kc-sib"}

	owned := pairDurablyOwned(context.Background(), grants, sib, "asset-1", "alice")

	assert.False(t, owned, "a sibling already owning a different pair must not be overwritten")
	assert.Equal(t, port.KeyContactGrant{MembershipUID: "asset-2", Username: "bob", Revision: 5}, grants.Entries["kc-sib"],
		"the sibling's own entry must be untouched")
}

func TestPairDurablyOwned_IndexReadFailure_Retains(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{GetErr: assert.AnError}
	sib := &model.KeyContact{UID: "kc-sib"}

	owned := pairDurablyOwned(context.Background(), grants, sib, "asset-1", "alice")

	assert.False(t, owned, "an index read failure must not be treated as durable ownership")
}

// ── CDC delete replay-cursor hold ─────────────────────────────────────────────

func TestHandleProjectRoleDelete_UncertainRevoke_HoldsReplayCursor(t *testing.T) {
	o := &CDCConsumer{
		publisher:               mock.NewMockMemberPublisher(),
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 1}}},
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{Err: assert.AnError},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	assert.ErrorIs(t, err, errKeyContactRevokeIncomplete,
		"an inconclusive sibling scan must hold the replay cursor for redelivery")
	_, found := o.grantIndex.(*mock.MockKeyContactGrantIndex).Entries["kc-1"]
	assert.True(t, found, "the index entry is the retry address and must be kept")
}

func TestHandleProjectRoleDelete_FailedRevoke_HoldsReplayCursor(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pub.SetAccessError(assert.AnError)
	o := &CDCConsumer{
		publisher:               pub,
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 1}}},
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	assert.ErrorIs(t, err, errKeyContactRevokeIncomplete)
}

func TestHandleProjectRoleDelete_ConfirmedRevoke_AdvancesAndClears(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 1}}}
	o := &CDCConsumer{
		publisher:               mock.NewMockMemberPublisher(),
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              grants,
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	require.NoError(t, err)
	assert.Contains(t, grants.Deletes, "kc-1", "a confirmed revoke must clear the index entry")
}

// ── U1: exhausted grant-index read failure ────────────────────────────────────

func TestLookupKeyContactGrant_ReadFailureExhausted_ReturnsError(t *testing.T) {
	calls := 0
	grants := &mock.MockKeyContactGrantIndex{
		GetFn: func(_ context.Context, _ string) (port.KeyContactGrant, bool, error) {
			calls++
			return port.KeyContactGrant{}, false, assert.AnError
		},
	}
	o := &CDCConsumer{grantIndex: grants}

	_, found, err := o.lookupKeyContactGrant(context.Background(), "kc-1")

	require.Error(t, err, "a read failure exhausted across every retry attempt must be reported, not swallowed")
	assert.False(t, found)
	assert.Equal(t, maxGrantIndexReadAttempts, calls, "must retry up to the bounded attempt budget")
}

func TestHandleProjectRoleDelete_IndexReadFailureExhausted_HoldsReplayCursorWithoutFallback(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		GetFn: func(_ context.Context, _ string) (port.KeyContactGrant, bool, error) {
			return port.KeyContactGrant{}, false, assert.AnError
		},
	}
	o := &CDCConsumer{
		publisher:               pub,
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              grants,
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	assert.ErrorIs(t, err, errKeyContactRevokeIncomplete,
		"an exhausted read failure must hold the replay cursor, not fall back to an unaddressed revoke")
	assert.Nil(t, pub.LastAccessData,
		"no unaddressed revoke must be published while the read failure is unresolved")
}

// ── U1: marker-only entry (live pair already cleared) ─────────────────────────

func TestHandleProjectRoleDelete_MarkerOnlyEntry_RevokesPendingPairAndDeletes(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"}, Revision: 1},
	}}
	o := &CDCConsumer{
		publisher:               mock.NewMockMemberPublisher(),
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              grants,
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	require.NoError(t, err)
	assert.Contains(t, grants.Deletes, "kc-1",
		"a marker-only entry must clear once the pending pair's revoke is published")
}

func TestHandleProjectRoleDelete_MarkerOnlyEntry_FailedRevoke_HoldsCursor(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"}, Revision: 1},
	}}
	pub := mock.NewMockMemberPublisher()
	pub.SetAccessError(assert.AnError)
	o := &CDCConsumer{
		publisher:               pub,
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              grants,
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	assert.ErrorIs(t, err, errKeyContactRevokeIncomplete)
	_, found := grants.Entries["kc-1"]
	assert.True(t, found, "a failed marker revoke must preserve the entry as the retry address")
}

// ── U1: full entry that also carries a marker ─────────────────────────────────

func TestHandleProjectRoleDelete_LivePairWithMarker_DrainsBothAndDeletes(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {
			MembershipUID: "asset-1", Username: "alice",
			PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"},
			Revision:      1,
		},
	}}
	o := &CDCConsumer{
		publisher:               mock.NewMockMemberPublisher(),
		cacheInvalidator:        &mock.MockCacheInvalidator{},
		grantIndex:              grants,
		keyContactsByMembership: &mock.MockKeyContactsByMembershipReader{},
	}

	err := o.handleProjectRoleDelete(context.Background(), "kc-1")

	require.NoError(t, err)
	assert.Contains(t, grants.Deletes, "kc-1",
		"both the live pair and its marker must drain before the entry clears")
}

func TestRevokeKeyContactMarkerOnDelete_Success_DeletesEntry(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"}, Revision: 1},
	}}
	o := &CDCConsumer{publisher: mock.NewMockMemberPublisher(), grantIndex: grants}
	grant := grants.Entries["kc-1"]

	err := o.revokeKeyContactMarkerOnDelete(context.Background(), "kc-1", grant, stubSiblingLister{})

	require.NoError(t, err)
	assert.Contains(t, grants.Deletes, "kc-1")
}

func TestRevokeKeyContactMarkerOnDelete_Uncertain_HoldsCursor(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"}, Revision: 1},
	}}
	o := &CDCConsumer{publisher: mock.NewMockMemberPublisher(), grantIndex: grants}
	grant := grants.Entries["kc-1"]

	err := o.revokeKeyContactMarkerOnDelete(context.Background(), "kc-1", grant, stubSiblingLister{err: assert.AnError})

	assert.ErrorIs(t, err, errKeyContactRevokeIncomplete)
	_, found := grants.Entries["kc-1"]
	assert.True(t, found, "an uncertain scan must preserve the entry as the retry address")
}

func TestDrainKeyContactMarkerAndDelete_Success_DeletesEntry(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {
			MembershipUID: "asset-1", Username: "alice",
			PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"},
			Revision:      1,
		},
	}}
	o := &CDCConsumer{publisher: mock.NewMockMemberPublisher(), grantIndex: grants}
	grant := grants.Entries["kc-1"]

	err := o.drainKeyContactMarkerAndDelete(context.Background(), "kc-1", grant, stubSiblingLister{})

	require.NoError(t, err)
	assert.Contains(t, grants.Deletes, "kc-1")
}

func TestDrainKeyContactMarkerAndDelete_DrainFails_PreservesMarkerHoldsCursor(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {
			MembershipUID: "asset-1", Username: "alice",
			PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-2", Username: "bob"},
			Revision:      1,
		},
	}}
	pub := mock.NewMockMemberPublisher()
	pub.SetAccessError(assert.AnError)
	o := &CDCConsumer{publisher: pub, grantIndex: grants}
	grant := grants.Entries["kc-1"]

	err := o.drainKeyContactMarkerAndDelete(context.Background(), "kc-1", grant, stubSiblingLister{})

	assert.ErrorIs(t, err, errKeyContactRevokeIncomplete)
	stored, found := grants.Entries["kc-1"]
	require.True(t, found, "the marker must be preserved so a later delete can retry the drain")
	assert.Empty(t, stored.MembershipUID, "the live pair was already confirmed revoked and must stay cleared")
	require.NotNil(t, stored.PendingRevoke, "the undrained marker must remain as the retry address")
	assert.Equal(t, "asset-2", stored.PendingRevoke.MembershipUID)
}
