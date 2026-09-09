// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
