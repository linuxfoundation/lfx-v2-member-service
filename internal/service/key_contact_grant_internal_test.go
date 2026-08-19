// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
)

const internalTestMembershipUID = "00000000-0000-0000-0000-000000000010"

// White-box tests for revokeKeyContactGrantIfUnregistered / clearRevokedGrant
// (unexported — the definitive-miss revoke path added for LFXV2-2999). Kept
// in a package-internal test file since these helpers are wired directly
// into the writer/CDC/backfill call sites and are not part of the package's
// exported surface (unlike PublishKeyContactFGA).

// internalRemoveMessages returns the member_remove payloads captured by a
// mock.MockMemberPublisher, in publication order.
func internalRemoveMessages(t *testing.T, msgs []any) []fgatypes.GenericMemberData {
	t.Helper()
	var out []fgatypes.GenericMemberData
	for _, msg := range msgs {
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

func TestRevokeKeyContactGrantIfUnregistered_NoRecordedGrant_NoPublish(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	assert.Nil(t, pub.LastAccessData, "no grant was ever recorded, so there is nothing to revoke")
	assert.Empty(t, grants.Deletes, "and no index entry to clear")
}

func TestRevokeKeyContactGrantIfUnregistered_RecordedGrant_RevokesAndClears(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1},
		},
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	removes := internalRemoveMessages(t, []any{pub.LastAccessData})
	require.Len(t, removes, 1, "the recorded grant must be revoked")
	assert.Equal(t, internalTestMembershipUID, removes[0].UID)
	assert.Equal(t, "alice", removes[0].Username)
	assert.Equal(t, 1, pub.FlushCount, "delivery must be confirmed via Flush before the index entry is cleared")
	assert.Equal(t, []string{"kc-1"}, grants.Deletes, "the confirmed-revoked entry must be cleared")
}

// TestRevokeKeyContactGrantIfUnregistered_PublishFailure_RetainsEntry covers
// an unconfirmed revoke: it must not be treated as done — the entry must
// survive so a later reactive trigger (or backfill) can retry it.
func TestRevokeKeyContactGrantIfUnregistered_PublishFailure_RetainsEntry(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pub.SetAccessError(assert.AnError)
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1},
		},
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	assert.Empty(t, grants.Deletes, "an unconfirmed publish must leave the entry in place for retry")
	assert.Equal(t, internalTestMembershipUID, grants.Entries["kc-1"].MembershipUID, "the grant must still be addressable")
}

// TestRevokeKeyContactGrantIfUnregistered_FlushFailure_RetainsEntry covers the
// other half: Access succeeding only means the message reached the local NATS
// connection, not the broker — an unconfirmed Flush must not clear the entry.
func TestRevokeKeyContactGrantIfUnregistered_FlushFailure_RetainsEntry(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pub.SetFlushError(assert.AnError)
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1},
		},
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	removes := internalRemoveMessages(t, []any{pub.LastAccessData})
	require.NotEmpty(t, removes, "the revoke was handed to NATS even though delivery was never confirmed")
	assert.Empty(t, grants.Deletes, "an unconfirmed flush must leave the entry in place for retry")
}

// TestRevokeKeyContactGrantIfUnregistered_PreservesUnrelatedPendingRevoke
// covers an unrelated, still-outstanding PendingRevoke marker on the entry
// (from a concurrent supersede): it must survive — it addresses a different
// tuple than the one this call just confirmed revoked, and is only ever
// cleared by its own confirmed revoke.
func TestRevokeKeyContactGrantIfUnregistered_PreservesUnrelatedPendingRevoke(t *testing.T) {
	pending := port.KeyContactGrantRef{MembershipUID: "asset-superseded", Username: "carol"}
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1, PendingRevoke: &pending},
		},
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	removes := internalRemoveMessages(t, []any{pub.LastAccessData})
	require.Len(t, removes, 1, "only the live pair is revoked by this call")
	assert.Equal(t, "alice", removes[0].Username)
	assert.Empty(t, grants.Deletes, "the entry is rewritten (live grant cleared), not deleted, so the marker survives")
	entry, found := grants.Entries["kc-1"]
	require.True(t, found, "the entry must survive to carry the unrelated pending-revoke marker")
	assert.Empty(t, entry.MembershipUID, "the just-revoked live pair must be cleared")
	assert.Empty(t, entry.Username)
	require.NotNil(t, entry.PendingRevoke, "the unrelated marker must not be discarded")
	assert.Equal(t, pending, *entry.PendingRevoke)
}

// TestRevokeKeyContactGrantIfUnregistered_ConcurrentReplacement_AbortsBeforePublish
// covers a concurrent writer that already replaced this pair (e.g. the email
// was corrected and a new grant published) between this call's read and its
// claim: the claim's revision-conditional rewrite must fail as a conflict, so
// the function aborts before ever publishing a revoke for the pair it read —
// the replacement must not be discarded, and no stale member_remove must be
// sent for a pair that no longer describes what is live.
func TestRevokeKeyContactGrantIfUnregistered_ConcurrentReplacement_AbortsBeforePublish(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	original := port.KeyContactGrant{MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{"kc-1": original},
	}
	// Simulate a concurrent writer replacing the pair immediately after this
	// call's initial Get (revision 1): by the time this call's claim
	// (Put(uid, original) conditional on revision 1) runs, the stored entry
	// is already at revision 2 with a different pair.
	grants.GetFn = func(_ context.Context, uid string) (port.KeyContactGrant, bool, error) {
		grants.Entries[uid] = port.KeyContactGrant{MembershipUID: "asset-new", Username: "bob", Revision: 2}
		return original, true, nil
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	assert.Nil(t, pub.LastAccessData, "the claim must fail before any revoke is published for the stale read")
	assert.Equal(t, "asset-new", grants.Entries["kc-1"].MembershipUID, "the newer pair must not be discarded")
	assert.Empty(t, grants.Deletes, "the claim conflict must abort before reaching the clear step")
}

// TestRevokeKeyContactGrantIfUnregistered_ConcurrentReconfirmation_AbortsBeforePublish
// covers the exact race this claim step closes: a concurrent writer
// reconfirms the *same* pair (recordKeyContactGrant's unchanged-pair branch,
// which now always touches the revision) between this call's read and its
// claim. Without the touch, the claim would see the same stale revision and
// could not distinguish this from no concurrent activity at all, letting a
// stale revoke through for a pair another writer just reasserted as live.
func TestRevokeKeyContactGrantIfUnregistered_ConcurrentReconfirmation_AbortsBeforePublish(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	original := port.KeyContactGrant{MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{"kc-1": original},
	}
	// Simulate recordKeyContactGrant's own touch on an unchanged pair,
	// landing between this call's Get and its claim: same pair, revision
	// advanced from 1 to 2.
	grants.GetFn = func(_ context.Context, uid string) (port.KeyContactGrant, bool, error) {
		grants.Entries[uid] = port.KeyContactGrant{MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 2}
		return original, true, nil
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	assert.Nil(t, pub.LastAccessData, "a concurrent reconfirmation of the same pair must abort the revoke, not just the later clear")
	assert.Equal(t, uint64(2), grants.Entries["kc-1"].Revision, "the reconfirmed entry must be left exactly as the concurrent writer left it")
}

// TestRevokeKeyContactGrantIfUnregistered_ClaimSucceeds_ClearUsesAdvancedRevision
// covers the happy path once the claim is in place: the claim's own Put
// advances the revision, so the final clear must use that advanced revision
// (re-read after Flush, since Put does not return it) rather than the
// revision originally observed by Get — using the stale revision there would
// make every clear fail as a spurious conflict against its own claim.
func TestRevokeKeyContactGrantIfUnregistered_ClaimSucceeds_ClearUsesAdvancedRevision(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1},
		},
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	removes := internalRemoveMessages(t, []any{pub.LastAccessData})
	require.Len(t, removes, 1, "the claim must not block the legitimate revoke it protects")
	require.Len(t, grants.Puts, 1, "the claim itself is a Put")
	assert.Equal(t, uint64(1), grants.Puts[0].Revision, "the claim is conditioned on the originally observed revision")
	require.Equal(t, []string{"kc-1"}, grants.Deletes, "the clear must still run and succeed")
	_, found := grants.Entries["kc-1"]
	assert.False(t, found, "the entry must be fully cleared once the clear's CAS uses the claim's advanced revision")
}

// TestClearPendingRevoke_MarkerOnlyEntry_Deletes covers a marker-only entry
// (its live pair was already revoked and cleared by clearRevokedGrant,
// leaving only a PendingRevoke for a different, unrelated pair) whose own
// revoke now confirms delivered. Clearing the marker would otherwise leave an
// entry with neither a live pair nor a PendingRevoke — which Put rejects as
// addressing nothing — so this must delete the entry outright instead.
func TestClearPendingRevoke_MarkerOnlyEntry_Deletes(t *testing.T) {
	superseded := port.KeyContactGrantRef{MembershipUID: "asset-superseded", Username: "carol"}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {PendingRevoke: &superseded, Revision: 5},
		},
	}

	err := clearPendingRevoke(context.Background(), grants, "kc-1", superseded)

	require.NoError(t, err)
	assert.Equal(t, []string{"kc-1"}, grants.Deletes, "a marker-only entry must be deleted, not rewritten as empty")
	_, found := grants.Entries["kc-1"]
	assert.False(t, found, "the entry must be gone once its only marker is cleared")
}

// TestRecordKeyContactGrant_MarkerOnlyEntry_CarriesPendingRevokeForward
// covers a new grant arriving for a contact whose entry is currently
// marker-only. There is no live pair here for the new grant to supersede;
// treating the empty pair as "superseded" would overwrite the real,
// still-outstanding PendingRevoke with an empty reference that serialization
// then drops, stranding the only address for that other revoke. The marker
// must instead be carried forward untouched alongside the new live pair, and
// no supersede-revoke fires since nothing live was actually replaced.
func TestRecordKeyContactGrant_MarkerOnlyEntry_CarriesPendingRevokeForward(t *testing.T) {
	pending := port.KeyContactGrantRef{MembershipUID: "asset-superseded", Username: "carol"}
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {PendingRevoke: &pending, Revision: 5},
		},
	}

	err := recordKeyContactGrant(context.Background(), pub, grants, "kc-1", "asset-new", "bob")

	require.NoError(t, err)
	assert.Nil(t, pub.LastAccessData, "nothing live existed to supersede, so no revoke must fire")
	entry, found := grants.Entries["kc-1"]
	require.True(t, found)
	assert.Equal(t, "asset-new", entry.MembershipUID)
	assert.Equal(t, "bob", entry.Username)
	require.NotNil(t, entry.PendingRevoke, "the real marker must survive, not be overwritten by an empty superseded ref")
	assert.Equal(t, pending, *entry.PendingRevoke)
}
