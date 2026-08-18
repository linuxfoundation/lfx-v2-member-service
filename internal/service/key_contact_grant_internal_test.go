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

// TestRevokeKeyContactGrantIfUnregistered_ConcurrentReplacement_LeavesNewPairUntouched
// covers a concurrent writer that already replaced this pair (e.g. the email
// was corrected and a new grant published) between this call's read and its
// confirmed revoke: the replacement must not be discarded — the
// CAS-conditional clear must be a no-op against the new value.
func TestRevokeKeyContactGrantIfUnregistered_ConcurrentReplacement_LeavesNewPairUntouched(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1},
		},
	}
	// Simulate a concurrent writer replacing the pair between this call's
	// initial Get and its post-revoke clear attempt: the clear's own Get sees
	// a different pair than the one just revoked.
	reads := 0
	grants.GetFn = func(_ context.Context, uid string) (port.KeyContactGrant, bool, error) {
		reads++
		if reads == 1 {
			return port.KeyContactGrant{MembershipUID: internalTestMembershipUID, Username: "alice", Revision: 1}, true, nil
		}
		return port.KeyContactGrant{MembershipUID: "asset-new", Username: "bob", Revision: 2}, true, nil
	}

	revokeKeyContactGrantIfUnregistered(context.Background(), pub, grants, "kc-1")

	assert.Empty(t, grants.Deletes, "a pair that no longer matches what was revoked must not be cleared")
}
