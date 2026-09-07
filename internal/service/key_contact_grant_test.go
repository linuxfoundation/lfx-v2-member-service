// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"testing"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
)

// removeMessages returns the member_remove payloads captured by an
// accessPayloadPublisher, in publication order.
func removeMessages(t *testing.T, p *accessPayloadPublisher) []fgatypes.GenericMemberData {
	t.Helper()
	var out []fgatypes.GenericMemberData
	for _, msg := range p.accessMsgs {
		fgaMsg, ok := msg.(fgatypes.GenericFGAMessage)
		if !ok || fgaMsg.Operation != "member_remove" {
			continue
		}
		data, ok := fgaMsg.Data.(fgatypes.GenericMemberData)
		require.True(t, ok)
		out = append(out, data)
	}
	return out
}

// ── Recording published grants ────────────────────────────────────────────────

func TestPublishKeyContactFGA_RecordsGrantOnColdIndex(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
	}, nil)

	assert.Equal(t, port.KeyContactGrant{MembershipUID: "asset-1", Username: "alice", Revision: 1},
		grants.Entries["kc-1"])
	assert.Empty(t, removeMessages(t, pub),
		"a first grant supersedes nothing, so a backfill over a cold index must emit no revocations")
}

func TestPublishKeyContactFGA_NoUsernameRecordsNothing(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-pending",
		MembershipUID: "asset-1",
	}, nil)

	assert.Empty(t, pub.accessMsgs, "a pending contact has no grant to publish")
	assert.Empty(t, grants.Puts, "and nothing to record")
}

func TestPublishKeyContactFGA_UnchangedGrantTouchesRevisionButPublishesNothing(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
	}, nil)

	assert.Empty(t, removeMessages(t, pub), "an unchanged grant supersedes nothing")
	// The index is still touched — a revision-conditional rewrite of the same
	// pair — so a concurrent revokeKeyContactGrantIfNoLongerLive claiming a
	// stale read of this entry sees the advanced revision and aborts rather
	// than firing a stale revoke for a pair just reconfirmed live.
	require.Len(t, grants.Puts, 1, "an unchanged pair must still advance the revision for a concurrent revoke to detect")
	assert.Equal(t, "asset-1", grants.Entries["kc-1"].MembershipUID)
	assert.Equal(t, uint64(4), grants.Entries["kc-1"].Revision, "the touch must advance the revision, not just re-store the same one")
}

func TestPublishKeyContactFGA_ReparentRevokesPreviousMembership(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-old", Username: "alice", Revision: 3},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-new",
		Username:      "alice",
	}, nil)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "the grant on the previous membership must be revoked")
	assert.Equal(t, "asset-old", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	assert.Equal(t, "asset-new", grants.Entries["kc-1"].MembershipUID)
}

func TestPublishKeyContactFGA_UsernameChangeRevokesPreviousUser(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "bob",
	}, nil)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "the previous user's grant must be revoked")
	assert.Equal(t, "asset-1", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	assert.Equal(t, "bob", grants.Entries["kc-1"].Username)

	// Put before remove: the replacement grant must never leave a window with no
	// access on the membership.
	require.GreaterOrEqual(t, len(pub.accessMsgs), 2)
	firstMsg, ok := pub.accessMsgs[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	assert.Equal(t, "member_put", firstMsg.Operation)
}

func TestPublishKeyContactFGA_RetriesIndexWriteOnConflict(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}
	// First read returns a revision that a concurrent writer has already moved
	// past, so the conditional write is rejected and the grant is re-evaluated
	// against the current value.
	reads := 0
	grants.GetFn = func(_ context.Context, uid string) (port.KeyContactGrant, bool, error) {
		reads++
		if reads == 1 {
			return port.KeyContactGrant{MembershipUID: "asset-1", Username: "alice", Revision: 2}, true, nil
		}
		return grants.Entries[uid], true, nil
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "bob",
	}, nil)

	// Two reads for the rejected write's retry, plus one more when
	// clearPendingRevoke re-reads the entry to clear the PendingRevoke marker
	// after the superseded username's revoke is confirmed delivered.
	assert.Equal(t, 3, reads, "a rejected write must be retried against a fresh read")
	assert.Equal(t, "bob", grants.Entries["kc-1"].Username)
	assert.Nil(t, grants.Entries["kc-1"].PendingRevoke,
		"the pending-revoke marker must be cleared once the superseded grant's revoke is confirmed delivered")
}

// TestPublishKeyContactFGA_PutFailure_DoesNotRevokeSupersededGrant covers a
// If recording the replacement fails, the previous
// grant must not have already been revoked — otherwise the index is left
// pointing at a pair that was just revoked while the replacement it should
// describe was never recorded, and a later delete revokes the stale pair
// again while the live tuple goes unaddressed.
func TestPublishKeyContactFGA_PutFailure_DoesNotRevokeSupersededGrant(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-old", Username: "alice", Revision: 3},
		},
		PutErr: assert.AnError,
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-new",
		Username:      "alice",
	}, nil)

	assert.Empty(t, removeMessages(t, pub),
		"the old grant must not be revoked when the replacement failed to record")
	assert.Equal(t, "asset-old", grants.Entries["kc-1"].MembershipUID,
		"the index must still describe the grant that is actually still live")
}

// TestPublishKeyContactFGA_SupersededRevokePublishFailure_PreservesPendingRevoke
// Access only hands the superseded revoke to
// the local NATS connection. The replacement Put (which commits durably on
// return) has already committed by this point, so if publish fails outright
// the old pair's address must survive as PendingRevoke in the index — losing
// it here would leave the old tuple live with nothing left anywhere to
// address its revoke by.
func TestPublishKeyContactFGA_SupersededRevokePublishFailure_PreservesPendingRevoke(t *testing.T) {
	pub := &errorFGARemovePublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-old", Username: "alice", Revision: 3},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-new",
		Username:      "alice",
	}, nil)

	entry := grants.Entries["kc-1"]
	assert.Equal(t, "asset-new", entry.MembershipUID, "the replacement must still be recorded")
	require.NotNil(t, entry.PendingRevoke,
		"a failed revoke publish must leave the superseded pair's address recorded, not discarded")
	assert.Equal(t, "asset-old", entry.PendingRevoke.MembershipUID)
	assert.Equal(t, "alice", entry.PendingRevoke.Username)
}

// TestPublishKeyContactFGA_SupersededRevokeFlushFailure_PreservesPendingRevoke
// covers the other half of the same finding: a nil error from Access only
// means the message was handed to the local connection, not that the broker
// received it. Flush is what confirms delivery; when Flush fails (or a crash
// interrupts it), the PendingRevoke marker must still not be cleared — an
// unconfirmed revoke is indistinguishable from a lost one.
func TestPublishKeyContactFGA_SupersededRevokeFlushFailure_PreservesPendingRevoke(t *testing.T) {
	pub := &accessPayloadPublisher{}
	pub.flushErr = assert.AnError
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-old", Username: "alice", Revision: 3},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-new",
		Username:      "alice",
	}, nil)

	require.NotEmpty(t, removeMessages(t, pub),
		"the revoke was handed to NATS even though delivery was never confirmed")
	entry := grants.Entries["kc-1"]
	require.NotNil(t, entry.PendingRevoke,
		"an unconfirmed flush must leave the superseded pair's address recorded for retry")
	assert.Equal(t, "asset-old", entry.PendingRevoke.MembershipUID)
}

func TestPublishKeyContactFGA_UnchangedGrantDoesNotRetryPendingRevoke(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {
				MembershipUID: "asset-new",
				Username:      "alice",
				Revision:      4,
				PendingRevoke: &port.KeyContactGrantRef{
					MembershipUID: "asset-old",
					Username:      "alice",
				},
			},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-new",
		Username:      "alice",
	}, nil)

	assert.Empty(t, removeMessages(t, pub),
		"the tuple address may still be justified by another key-contact record and must not be retried blindly")
	assert.NotNil(t, grants.Entries["kc-1"].PendingRevoke,
		"an unchanged publish cannot prove the pending tuple is safe to revoke")
}

func TestPublishKeyContactFGA_NilIndexPublishesUnchanged(t *testing.T) {
	pub := &accessPayloadPublisher{}

	svc.PublishKeyContactFGA(context.Background(), pub, nil, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
	}, nil)

	require.Len(t, pub.accessMsgs, 1, "an unwired index must not change publish behaviour")
}

// ── Status gating ──────────────────────────────────────────────────────────────

func TestPublishKeyContactFGA_InactiveStatus_NoPublishNoRecord(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
		Status:        "Inactive",
	}, nil)

	assert.Empty(t, pub.accessMsgs, "an inactive key contact must never receive a live member_put")
	assert.Empty(t, grants.Puts, "and nothing is recorded for it")
}

func TestPublishKeyContactFGA_InactiveStatusCaseInsensitive_NoPublish(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
		Status:        "inactive",
	}, nil)

	assert.Empty(t, pub.accessMsgs, "the status gate must match case-insensitively")
	assert.Empty(t, grants.Puts)
}

func TestPublishKeyContactFGA_DeactivatedContact_RevokesExistingGrant(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
		Status:        "Inactive",
	}, nil)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "deactivating a contact with an existing grant must revoke it, not reconfirm it")
	assert.Equal(t, "asset-1", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	_, found := grants.Entries["kc-1"]
	assert.False(t, found, "the index entry must be cleared once the revoke is confirmed delivered")
}

func TestPublishKeyContactFGA_ActiveStatus_PublishesNormally(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Username:      "alice",
		Status:        "Active",
	}, nil)

	require.Len(t, pub.accessMsgs, 1, "an active key contact must still publish its grant")
	assert.Equal(t, port.KeyContactGrant{MembershipUID: "asset-1", Username: "alice", Revision: 1},
		grants.Entries["kc-1"])
}

// ── Reference-aware revoke (shared tuples) ────────────────────────────────────

// fakeMembershipLister is a local double for the unexported
// membershipKeyContactLister; structural typing means no mock export is needed.
type fakeMembershipLister struct {
	siblings []*model.KeyContact
	err      error
}

func (f *fakeMembershipLister) ListKeyContactsForMembership(_ context.Context, _ string) ([]*model.KeyContact, error) {
	return f.siblings, f.err
}

func TestPublishKeyContactFGA_InactiveWithLiveSibling_SkipsRevokeClearsOwnEntry(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}
	lister := &fakeMembershipLister{siblings: []*model.KeyContact{
		{UID: "kc-2", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Active"},
	}}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	assert.Empty(t, removeMessages(t, pub),
		"a still-Active sibling on the same membership still justifies the tuple")
	_, found := grants.Entries["kc-1"]
	assert.False(t, found, "this record's own index entry must still be cleared")
}

func TestPublishKeyContactFGA_InactiveWithNoLiveSibling_RevokesAsBefore(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}
	lister := &fakeMembershipLister{siblings: []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Inactive"},
	}}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "no live sibling means the tuple must still be revoked")
	assert.Equal(t, "asset-1", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	_, found := grants.Entries["kc-1"]
	assert.False(t, found)
}

func TestPublishKeyContactFGA_SiblingScanError_SkipsRevokeEntirely(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}
	lister := &fakeMembershipLister{err: assert.AnError}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	assert.Empty(t, removeMessages(t, pub),
		"a failed sibling scan must never strip a possibly still-justified tuple")
	assert.Empty(t, grants.Deletes, "nor mutate the index on uncertain information")
	assert.Equal(t, port.KeyContactGrant{MembershipUID: "asset-1", Username: "alice", Revision: 3},
		grants.Entries["kc-1"], "the entry must be left exactly as it was")
}

func TestPublishKeyContactFGA_SiblingFilteredOut_StillRevokes(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "asset-1", Username: "alice", Revision: 3},
		},
	}
	lister := &fakeMembershipLister{siblings: []*model.KeyContact{
		// Same UID as kc itself: not a sibling.
		{UID: "kc-1", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Active"},
		// Inactive sibling: does not justify the tuple.
		{UID: "kc-2", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Inactive"},
		// Different email: a different tuple entirely.
		{UID: "kc-3", MembershipUID: "asset-1", Email: "bob@example.com", Status: "Active"},
	}}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "none of the returned siblings actually justify the tuple, so it must still be revoked")
	assert.Equal(t, "asset-1", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
}

// ── API writer paths ──────────────────────────────────────────────────────────

func newKCWriterWithGrantIndex(
	storage svc.MemberStorageReader,
	pmReader svc.PMReader,
	pub svc.PublisherForKC,
	userReader svc.UserReaderForKC,
	grants port.KeyContactGrantIndex,
) svc.KeyContactWriter {
	return svc.NewKeyContactWriter(
		svc.WithKCStorage(storage),
		svc.WithKCWriter(mock.NewMockKeyContactWriterWithOK()),
		svc.WithKCProjectMembershipReader(pmReader),
		svc.WithKCPublisher(pub),
		svc.WithKCUserReader(userReader),
		svc.WithKCGrantIndex(grants),
	)
}

func TestKeyContactWriter_Create_RecordsGrant(t *testing.T) {
	pmReader := &seededPMReader{pm: &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: "org-1", ProjectUID: "proj-1"}}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	w := newKCWriterWithGrantIndex(newSeededStorage(), pmReader, pub, userReaderFunc(
		func(_ context.Context, _ string) (string, error) { return "alice", nil }), grants)

	kc, err := w.Create(context.Background(), svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID,
		FirstName:     "Alice",
		LastName:      "Smith",
		Email:         "alice@example.com",
		Role:          "Technical Contact",
	})
	require.NoError(t, err)

	require.Len(t, grants.Puts, 1, "an API-published grant must be recorded so a later CDC delete can revoke it")
	assert.Equal(t, kc.UID, grants.Puts[0].UID)
	assert.Equal(t, testMembershipUID, grants.Puts[0].MembershipUID)
	assert.Equal(t, "alice", grants.Puts[0].Username)
}

func TestKeyContactWriter_Update_EmailChange_WithGrantIndex_RevokesOldGrantEvenOnColdIndex(t *testing.T) {
	// The writer's own old-username revoke must run unconditionally, even with a
	// grant index wired: the index-driven supersede-revoke inside
	// PublishKeyContactFGA only fires when idx.Get finds a stored entry, so on a
	// cold index (no prior recorded grant — e.g. pre-backfill) it does nothing.
	// Without this unconditional revoke, alice would keep access indefinitely.
	current := &model.KeyContact{
		UID: "kc-1", MembershipUID: testMembershipUID, Email: "old@example.com", Username: "alice",
	}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{} // cold: no recorded entry for kc-1
	usernames := map[string]string{"old@example.com": "alice", "new@example.com": "bob"}

	w := newKCWriterWithGrantIndex(newSeededStorage(current), &seededPMReader{pm: &model.ProjectMembership{UID: testMembershipUID}}, pub,
		userReaderFunc(func(_ context.Context, email string) (string, error) { return usernames[email], nil }), grants)

	newEmail := "new@example.com"
	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: "kc-1", Email: &newEmail,
	})
	require.NoError(t, err)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "a cold index cannot revoke the old grant itself, so the writer's own revoke must")
	assert.Equal(t, "alice", removes[0].Username)
	assert.Equal(t, "bob", grants.Entries["kc-1"].Username, "the index must hold the new grant")
}

func TestKeyContactWriter_Delete_FallsBackToRecordedUsername(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-1", MembershipUID: testMembershipUID, Email: "alice@example.com"}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: testMembershipUID, Username: "alice", Revision: 1},
		},
	}

	// Live LFID lookup yields nothing — before the index this silently skipped
	// the revoke and left the grant in place.
	w := newKCWriterWithGrantIndex(newSeededStorage(kc), &seededPMReader{}, pub, userReaderFunc(
		func(_ context.Context, _ string) (string, error) { return "", nil }), grants)

	require.NoError(t, w.Delete(context.Background(), svc.KeyContactDeleteInput{
		MembershipUID: testMembershipUID,
		UID:           "kc-1",
	}))

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "the recorded username must be used rather than skipping the revoke")
	assert.Equal(t, testMembershipUID, removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	assert.Equal(t, []string{"kc-1"}, grants.Deletes)
}

// TestKeyContactWriter_Delete_RevokesStaleIndexedPairDistinctFromLive covers a
// The index can describe a different pair than the contact's
// current live membership when an earlier recordKeyContactGrant Put failed
// (e.g. mid Salesforce reparent) — the replacement's member_put succeeded but
// the swallowed Put failure left the index still pointing at the old pair,
// whose own member_put was never revoked. Delete must revoke that indexed
// pair too, not just the live one, before clearing the entry — otherwise the
// old pair is left dangling forever once the Salesforce record is gone.
func TestKeyContactWriter_Delete_RevokesStaleIndexedPairDistinctFromLive(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-1", MembershipUID: testMembershipUID, Email: "alice@example.com"}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			// A stale entry from a reparent whose index Put failed: it still
			// names the pre-reparent membership, not the contact's current one.
			"kc-1": {MembershipUID: "pm-old-stale", Username: "alice", Revision: 4},
		},
	}

	w := newKCWriterWithGrantIndex(newSeededStorage(kc), &seededPMReader{}, pub, userReaderFunc(
		func(_ context.Context, _ string) (string, error) { return "alice", nil }), grants)

	require.NoError(t, w.Delete(context.Background(), svc.KeyContactDeleteInput{
		MembershipUID: testMembershipUID,
		UID:           "kc-1",
	}))

	removes := removeMessages(t, pub)
	require.Len(t, removes, 2, "both the live pair and the distinct stale indexed pair must be revoked")
	uids := []string{removes[0].UID, removes[1].UID}
	assert.Contains(t, uids, testMembershipUID, "the live membership's grant must be revoked")
	assert.Contains(t, uids, "pm-old-stale", "the stale indexed membership's grant must also be revoked")
	assert.Equal(t, []string{"kc-1"}, grants.Deletes, "the stale entry is cleared once both pairs are revoked")
}

// failOnMembershipRemovePublisher fails only the member_remove targeting a
// specific membership UID, letting other Access calls (the live-pair revoke,
// the indexer publish) succeed — isolates one specific revoke's failure.
type failOnMembershipRemovePublisher struct {
	accessPayloadPublisher
	failMembershipUID string
}

func (p *failOnMembershipRemovePublisher) Access(ctx context.Context, subject string, msg any) error {
	if err := p.accessPayloadPublisher.Access(ctx, subject, msg); err != nil {
		return err
	}
	if fgaMsg, ok := msg.(fgatypes.GenericFGAMessage); ok && fgaMsg.Operation == "member_remove" {
		if data, ok := fgaMsg.Data.(fgatypes.GenericMemberData); ok && data.UID == p.failMembershipUID {
			return assert.AnError
		}
	}
	return nil
}

// TestKeyContactWriter_Delete_StaleIndexedPairRevokeFailure_PreservesEntry
// If the distinct stale-indexed-pair revoke fails to
// publish, the index entry must not be cleared afterward — clearing it anyway
// would erase the only remaining record that pair's grant was ever made,
// leaving it live with nothing left to revoke it by.
func TestKeyContactWriter_Delete_StaleIndexedPairRevokeFailure_PreservesEntry(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-1", MembershipUID: testMembershipUID, Email: "alice@example.com"}
	pub := &failOnMembershipRemovePublisher{failMembershipUID: "pm-old-stale"}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "pm-old-stale", Username: "alice", Revision: 4},
		},
	}

	w := newKCWriterWithGrantIndex(newSeededStorage(kc), &seededPMReader{}, pub, userReaderFunc(
		func(_ context.Context, _ string) (string, error) { return "alice", nil }), grants)

	require.NoError(t, w.Delete(context.Background(), svc.KeyContactDeleteInput{
		MembershipUID: testMembershipUID,
		UID:           "kc-1",
	}))

	assert.Empty(t, grants.Deletes,
		"the entry must be preserved when the stale indexed pair's revoke failed to publish")
}

// TestKeyContactWriter_Delete_GrantIndexReadFailure_PreservesEntry covers a
// transient index read failure (distinct from a genuine miss): the delete
// must not clear the entry on an inconclusive read, or a still-live grant's
// only address is erased with nothing left to revoke it by.
func TestKeyContactWriter_Delete_GrantIndexReadFailure_PreservesEntry(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-1", MembershipUID: testMembershipUID, Email: "alice@example.com"}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: testMembershipUID, Username: "alice", Revision: 1},
		},
		GetErr: assert.AnError,
	}

	// Live LFID lookup also comes up empty, so the only source for a revoke
	// username is the (failing) index read.
	w := newKCWriterWithGrantIndex(newSeededStorage(kc), &seededPMReader{}, pub, userReaderFunc(
		func(_ context.Context, _ string) (string, error) { return "", nil }), grants)

	require.NoError(t, w.Delete(context.Background(), svc.KeyContactDeleteInput{
		MembershipUID: testMembershipUID,
		UID:           "kc-1",
	}))

	assert.Empty(t, removeMessages(t, pub), "no username was resolvable, so nothing was revoked")
	assert.Empty(t, grants.Deletes,
		"an inconclusive read must not clear a possibly still-live grant — the entry is the only record of it")
}

func TestKeyContactWriter_Delete_ClearsGrantWithNoUsernameAnywhere(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-1", MembershipUID: testMembershipUID, Email: "alice@example.com"}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}

	w := newKCWriterWithGrantIndex(newSeededStorage(kc), &seededPMReader{}, pub, userReaderFunc(
		func(_ context.Context, _ string) (string, error) { return "", nil }), grants)

	require.NoError(t, w.Delete(context.Background(), svc.KeyContactDeleteInput{
		MembershipUID: testMembershipUID,
		UID:           "kc-1",
	}))

	assert.Empty(t, removeMessages(t, pub), "no grant is known to have been made, so there is nothing to revoke")
	assert.Equal(t, []string{"kc-1"}, grants.Deletes,
		"cleanup must not be conditional on a publish, or the entry would be orphaned forever")
}

// TestKeyContactWriter_Delete_ColdReadRevisionZero_DoesNotTombstoneConcurrentGrant
// verifies that a cold-read revision of 0 never reaches the store as an
// unconditional delete. nats.go's jetstream KV Delete only sets the expected-sequence
// header when revision != 0 (jetstream/kv.go); LastRevision(0) is otherwise a
// silent no-op on that check, not "delete only if absent". This simulates a
// grant written by another writer after Delete's own read found nothing (a
// re-invite racing this delete) — the grant must survive.
func TestKeyContactWriter_Delete_ColdReadRevisionZero_DoesNotTombstoneConcurrentGrant(t *testing.T) {
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: testMembershipUID, Username: "alice", Revision: 7},
		},
	}

	// Directly exercise the index contract Delete relies on: a caller that
	// read no entry (revision 0) must not be able to erase one that exists
	// now, regardless of how that read happened.
	err := grants.Delete(context.Background(), "kc-1", 0)

	require.NoError(t, err)
	_, found, getErr := grants.Get(context.Background(), "kc-1")
	require.NoError(t, getErr)
	assert.True(t, found, "revision-0 delete must be a no-op, not an unconditional delete")
}

// ── Cold-index Inactive revoke ────────────────────────────────────────────────

// Grants published before the index existed (or whose index write failed) have
// a live tuple but no entry. Deactivation must still revoke them from the
// record's own pair rather than no-op on the index miss.
func TestPublishKeyContactFGA_InactiveColdIndex_RevokesCurrentPair(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}
	lister := &fakeMembershipLister{siblings: []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Inactive"},
	}}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "a cold index has no entry to drive the revoke, so the record's own pair must be used")
	assert.Equal(t, "asset-1", removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	assert.Empty(t, grants.Puts, "nothing was recorded, so nothing must be written")
	assert.Empty(t, grants.Deletes, "there is no entry to clear")
}

func TestPublishKeyContactFGA_InactiveColdIndexWithLiveSibling_NoRevoke(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}
	lister := &fakeMembershipLister{siblings: []*model.KeyContact{
		{UID: "kc-2", MembershipUID: "asset-1", Email: "alice@example.com", Status: "Active"},
	}}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	assert.Empty(t, removeMessages(t, pub),
		"a live sibling still justifies the tuple even when no index entry exists")
	assert.Empty(t, grants.Puts)
	assert.Empty(t, grants.Deletes)
}

func TestPublishKeyContactFGA_InactiveColdIndexSiblingScanError_NoPublishNoMutation(t *testing.T) {
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}
	lister := &fakeMembershipLister{err: assert.AnError}

	svc.PublishKeyContactFGA(context.Background(), pub, grants, &model.KeyContact{
		UID:           "kc-1",
		MembershipUID: "asset-1",
		Email:         "alice@example.com",
		Username:      "alice",
		Status:        "Inactive",
	}, lister)

	assert.Empty(t, pub.accessMsgs,
		"an uncertain sibling scan must publish nothing, not fall back to a cold-index revoke")
	assert.Empty(t, grants.Puts)
	assert.Empty(t, grants.Deletes)
}

// ── Email change with a shared old pair ───────────────────────────────────────

func newKCWriterWithSiblingReader(
	storage svc.MemberStorageReader,
	pub svc.PublisherForKC,
	userReader svc.UserReaderForKC,
	grants port.KeyContactGrantIndex,
	siblings port.KeyContactsByMembershipReader,
) svc.KeyContactWriter {
	return svc.NewKeyContactWriter(
		svc.WithKCStorage(storage),
		svc.WithKCWriter(mock.NewMockKeyContactWriterWithOK()),
		svc.WithKCProjectMembershipReader(&seededPMReader{pm: &model.ProjectMembership{UID: testMembershipUID}}),
		svc.WithKCPublisher(pub),
		svc.WithKCUserReader(userReader),
		svc.WithKCGrantIndex(grants),
		svc.WithKCSiblingReader(siblings),
	)
}

func TestKeyContactWriter_Update_EmailChange_OldPairSharedByLiveSibling_SuppressesOldRemove(t *testing.T) {
	current := &model.KeyContact{
		UID: "kc-1", MembershipUID: testMembershipUID, Email: "old@example.com", Username: "alice",
	}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}
	// A different record still holds the old email live on the same membership,
	// so the old pair's tuple is still justified.
	siblings := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{
		{UID: "kc-2", MembershipUID: testMembershipUID, Email: "old@example.com", Status: "Active"},
	}}
	usernames := map[string]string{"old@example.com": "alice", "new@example.com": "bob"}

	w := newKCWriterWithSiblingReader(newSeededStorage(current), pub,
		userReaderFunc(func(_ context.Context, email string) (string, error) { return usernames[email], nil }),
		grants, siblings)

	newEmail := "new@example.com"
	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: "kc-1", Email: &newEmail,
	})
	require.NoError(t, err)

	assert.Empty(t, removeMessages(t, pub),
		"the old pair is still justified by a live sibling and must not be removed")
	assert.Equal(t, "bob", grants.Entries["kc-1"].Username, "the new grant must still be recorded")
}

func TestKeyContactWriter_Update_EmailChange_NoLiveSibling_StillRemovesOldPair(t *testing.T) {
	current := &model.KeyContact{
		UID: "kc-1", MembershipUID: testMembershipUID, Email: "old@example.com", Username: "alice",
	}
	pub := &accessPayloadPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}
	// Only the updated record itself carries the old email: it must not count
	// as its own sibling, so the old pair is removed exactly as before.
	siblings := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{
		{UID: "kc-1", MembershipUID: testMembershipUID, Email: "old@example.com", Status: "Active"},
	}}
	usernames := map[string]string{"old@example.com": "alice", "new@example.com": "bob"}

	w := newKCWriterWithSiblingReader(newSeededStorage(current), pub,
		userReaderFunc(func(_ context.Context, email string) (string, error) { return usernames[email], nil }),
		grants, siblings)

	newEmail := "new@example.com"
	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: "kc-1", Email: &newEmail,
	})
	require.NoError(t, err)

	removes := removeMessages(t, pub)
	require.Len(t, removes, 1, "with no live sibling the old pair must still be removed")
	assert.Equal(t, testMembershipUID, removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
}
