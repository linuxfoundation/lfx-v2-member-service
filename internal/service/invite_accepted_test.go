// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"testing"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	inviteapi "github.com/linuxfoundation/lfx-v2-invite-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingWriter wraps a real OrgSettingsWriter and records how many times Update is called.
// It can be configured to return a fixed error on every Update call.
type countingWriter struct {
	inner     svc.OrgSettingsWriter
	updateErr error
	calls     int
}

func (c *countingWriter) Update(ctx context.Context, in svc.B2BOrgSettingsUpdate) (*model.B2BOrgSettings, error) {
	c.calls++
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	return c.inner.Update(ctx, in)
}

func (c *countingWriter) AddPrincipal(ctx context.Context, in svc.B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error) {
	return c.inner.AddPrincipal(ctx, in)
}

func (c *countingWriter) ChangePrincipalRole(ctx context.Context, in svc.B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error) {
	return c.inner.ChangePrincipalRole(ctx, in)
}

func (c *countingWriter) RemovePrincipal(ctx context.Context, in svc.B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error) {
	return c.inner.RemovePrincipal(ctx, in)
}

func newInviteAcceptedService(store *mock.MockB2BOrgSettings, writer svc.OrgSettingsWriter) *svc.InviteAcceptedService {
	return svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(writer),
	)
}

// orgAcceptedEvent builds a b2b_org invite_accepted event. Most tests use this.
func orgAcceptedEvent(email, acceptedBy string) inviteapi.InviteServiceAcceptedEvent {
	return inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: acceptedBy,
			Recipient:  inviteapi.Recipient{Email: email},
			Resource:   inviteapi.Resource{Type: "b2b_org"},
		},
	}
}

// orgAcceptedEventWithRole adds an explicit Role field to orgAcceptedEvent.
func orgAcceptedEventWithRole(email, acceptedBy, role string) inviteapi.InviteServiceAcceptedEvent {
	ev := orgAcceptedEvent(email, acceptedBy)
	ev.Role = role
	return ev
}

// ── Handle ────────────────────────────────────────────────────────────────

func TestInviteAcceptedService_Handle_PromotesPendingWriter(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{{
			Email:        "alice@example.com",
			InviteUUID:   "invite-writer-1",
			InvitedAs:    "writer",
			InviteStatus: model.InviteStatusPending,
		}},
	}, 1)

	inner := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	invSvc := newInviteAcceptedService(store, inner)

	err := invSvc.Handle(context.Background(), orgAcceptedEvent("alice@example.com", "auth0|alice"))

	require.NoError(t, err)
	saved, _, _ := store.GetSettings(context.Background(), testOrgUID)
	require.Len(t, saved.Writers, 1)
	w := saved.Writers[0]
	assert.Equal(t, "alice", w.Username, "auth0| prefix must be stripped from AcceptedBy")
	assert.Equal(t, model.InviteStatusAccepted, w.InviteStatus)
	assert.NotNil(t, w.AcceptedAt)
	assert.Empty(t, w.InviteUUID, "InviteUUID must be cleared on acceptance")
}

func TestInviteAcceptedService_Handle_PromotesPendingAuditor(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Auditors: []model.B2BOrgUser{{
			Email:        "bob@example.com",
			InviteUUID:   "invite-auditor-1",
			InvitedAs:    "auditor",
			InviteStatus: model.InviteStatusPending,
		}},
	}, 1)

	inner := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	invSvc := newInviteAcceptedService(store, inner)

	err := invSvc.Handle(context.Background(), orgAcceptedEvent("bob@example.com", "auth0|bob"))

	require.NoError(t, err)
	saved, _, _ := store.GetSettings(context.Background(), testOrgUID)
	require.Len(t, saved.Auditors, 1)
	a := saved.Auditors[0]
	assert.Equal(t, "bob", a.Username)
	assert.Equal(t, model.InviteStatusAccepted, a.InviteStatus)
	assert.Empty(t, a.InviteUUID)
}

func TestInviteAcceptedService_Handle_NoMatch_IsNoOp(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{{
			Email:        "dave@example.com",
			InviteStatus: model.InviteStatusPending,
		}},
	}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	invSvc := newInviteAcceptedService(store, inner)

	// Different email → no match
	err := invSvc.Handle(context.Background(), orgAcceptedEvent("nobody@example.com", "auth0|nobody"))

	require.NoError(t, err)
	assert.Equal(t, 0, inner.calls, "Update must not be called when there is no matching entry")
}

func TestInviteAcceptedService_Handle_RevisionConflict_RetriesThreeTimes(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{{
			Email:        "eve@example.com",
			InviteStatus: model.InviteStatusPending,
		}},
	}, 1)

	inner := &countingWriter{
		inner:     newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher()),
		updateErr: pkgerrors.NewConflict("concurrent write"),
	}
	invSvc := newInviteAcceptedService(store, inner)

	err := invSvc.Handle(context.Background(), orgAcceptedEvent("eve@example.com", "auth0|eve"))

	require.NoError(t, err, "revision conflict must not propagate to caller")
	assert.Equal(t, 3, inner.calls, "must retry exactly 3 times before giving up")
}

func TestInviteAcceptedService_Handle_MalformedEvent_EmailEmpty_DropsEvent(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	invSvc := newInviteAcceptedService(store, inner)

	ev := orgAcceptedEvent("", "auth0|someone")
	err := invSvc.Handle(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, 0, inner.calls, "malformed event (no email) must be dropped without scanning")
}

func TestInviteAcceptedService_Handle_MalformedEvent_AcceptedByEmpty_DropsEvent(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	invSvc := newInviteAcceptedService(store, inner)

	ev := orgAcceptedEvent("user@example.com", "")
	err := invSvc.Handle(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, 0, inner.calls, "event missing AcceptedBy must be dropped")
}

func TestInviteAcceptedService_Handle_NonBizOrgType_IsNoOp(t *testing.T) {
	// committee and project acceptances arrive on the same subscription; they must be
	// dropped with zero KV access (no ListSettingsOrgUIDs call).
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{{
			Email:        "fp@example.com",
			InviteStatus: model.InviteStatusPending,
		}},
	}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	invSvc := newInviteAcceptedService(store, inner)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|fp",
			Recipient:  inviteapi.Recipient{Email: "fp@example.com"},
			Resource:   inviteapi.Resource{Type: "group"}, // committee type
		},
	}
	err := invSvc.Handle(context.Background(), ev)

	require.NoError(t, err)
	assert.Equal(t, 0, inner.calls, "non-b2b_org event must be dropped without calling Update")
}

// ── Key-contact FGA resolution on invite acceptance ──────────────────────────

// stubKCOrgReader implements keyContactOrgReader (unexported; use svc.WithInviteAcceptedKeyContactReader).
type stubKCOrgReader struct {
	contacts []*model.KeyContact
}

func (r *stubKCOrgReader) ListKeyContactsForOrg(_ context.Context, _ string) ([]*model.KeyContact, error) {
	return r.contacts, nil
}

func TestInviteAcceptedService_Handle_GrantsKeyContactFGA_OnMatch(t *testing.T) {
	// Two key contacts with the same email on two different memberships in the
	// same org → both get key_contact FGA grants (Username=AcceptedBy).
	const orgUID = "001000000000000AAA"
	store := mock.NewMockB2BOrgSettings()
	store.Seed(orgUID, &model.B2BOrgSettings{UID: orgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	pub := mock.NewMockMemberPublisher()

	kcs := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "m-1", B2BOrgUID: orgUID, Email: "alice@example.com"},
		{UID: "kc-2", MembershipUID: "m-2", B2BOrgUID: orgUID, Email: "alice@example.com"},
	}
	invSvc := svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(inner),
		svc.WithInviteAcceptedKeyContactReader(&stubKCOrgReader{contacts: kcs}),
		svc.WithInviteAcceptedPublisher(pub),
	)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: orgUID},
		},
	}
	err := invSvc.Handle(context.Background(), ev)

	require.NoError(t, err)
	var accessCount, indexerCount int
	for _, c := range pub.CallOrder {
		switch c {
		case "access":
			accessCount++
		case "indexer":
			indexerCount++
		}
	}
	assert.Equal(t, 2, accessCount, "must publish key_contact FGA grant for each matching contact")
	assert.Equal(t, 2, indexerCount, "must publish key_contact indexer update for each matching contact")
	assert.Zero(t, pub.FlushCount, "grants are publish-only; only the API deletion path confirms delivery")
}

func TestInviteAcceptedService_Handle_NoKeyContactMatch_NoFGAGrant(t *testing.T) {
	// Non-matching email → no key_contact FGA grants.
	const orgUID = "001000000000000AAA"
	store := mock.NewMockB2BOrgSettings()
	store.Seed(orgUID, &model.B2BOrgSettings{UID: orgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	pub := mock.NewMockMemberPublisher()

	kcs := []*model.KeyContact{
		{UID: "kc-1", MembershipUID: "m-1", B2BOrgUID: orgUID, Email: "other@example.com"},
	}
	invSvc := svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(inner),
		svc.WithInviteAcceptedKeyContactReader(&stubKCOrgReader{contacts: kcs}),
		svc.WithInviteAcceptedPublisher(pub),
	)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: orgUID},
		},
	}
	err := invSvc.Handle(context.Background(), ev)

	require.NoError(t, err)
	var accessCount int
	for _, c := range pub.CallOrder {
		if c == "access" {
			accessCount++
		}
	}
	assert.Equal(t, 0, accessCount, "no FGA grant when email does not match any key contact")
}

// TestInviteAcceptedService_Handle_SupersededSiblingSameEmail_NoRemove covers a
// contact that moved from membership A to B: the grant index still points at
// A, and another same-email contact remains on A in the org's contact slice.
// The accepted-email resolver must let the supersede-revoke recognize A as
// still justified, so no member_remove is published for it.
func TestInviteAcceptedService_Handle_SupersededSiblingSameEmail_NoRemove(t *testing.T) {
	const orgUID = "001000000000000AAA"
	const movedUID = "kc-moved"
	store := mock.NewMockB2BOrgSettings()
	store.Seed(orgUID, &model.B2BOrgSettings{UID: orgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			movedUID: {MembershipUID: "m-A", Username: "alice", Revision: 1},
		},
	}

	kcs := []*model.KeyContact{
		// The moved contact: same UID the index has recorded, now on m-B.
		{UID: movedUID, MembershipUID: "m-B", Email: "alice@example.com", Status: "Active"},
		// A sibling still on the old membership, same email, live.
		{UID: "kc-sibling", MembershipUID: "m-A", Email: "alice@example.com", Status: "Active"},
	}
	invSvc := svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(inner),
		svc.WithInviteAcceptedKeyContactReader(&stubKCOrgReader{contacts: kcs}),
		svc.WithInviteAcceptedPublisher(pub),
		svc.WithInviteAcceptedKeyContactGrantIndex(grants),
	)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: orgUID},
		},
	}
	err := invSvc.Handle(context.Background(), ev)
	require.NoError(t, err)

	for _, msg := range pub.accessMessages {
		removeData, ok := msg.(fgatypes.GenericMemberData)
		if !ok {
			continue
		}
		assert.NotEqual(t, "m-A", removeData.UID,
			"the old membership is still justified by the same-email sibling; it must not be revoked")
	}
}

// TestInviteAcceptedService_Handle_SupersededSiblingDifferentEmail_Inconclusive
// covers the fail-safe side: a sibling on the old membership with a different
// email is not known to the accepted-email resolver, so the scan must read
// as inconclusive and skip the revoke rather than strip a possibly-justified
// tuple.
func TestInviteAcceptedService_Handle_SupersededSiblingDifferentEmail_Inconclusive(t *testing.T) {
	const orgUID = "001000000000000AAA"
	const movedUID = "kc-moved-2"
	store := mock.NewMockB2BOrgSettings()
	store.Seed(orgUID, &model.B2BOrgSettings{UID: orgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			movedUID: {MembershipUID: "m-A2", Username: "alice", Revision: 1},
		},
	}

	kcs := []*model.KeyContact{
		{UID: movedUID, MembershipUID: "m-B2", Email: "alice@example.com", Status: "Active"},
		// A different person entirely, still on the old membership.
		{UID: "kc-other", MembershipUID: "m-A2", Email: "carol@example.com", Status: "Active"},
	}
	invSvc := svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(inner),
		svc.WithInviteAcceptedKeyContactReader(&stubKCOrgReader{contacts: kcs}),
		svc.WithInviteAcceptedPublisher(pub),
		svc.WithInviteAcceptedKeyContactGrantIndex(grants),
	)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: orgUID},
		},
	}
	err := invSvc.Handle(context.Background(), ev)
	require.NoError(t, err)

	for _, msg := range pub.accessMessages {
		removeData, ok := msg.(fgatypes.GenericMemberData)
		if !ok {
			continue
		}
		assert.NotEqual(t, "m-A2", removeData.UID,
			"an unresolvable sibling must make the scan inconclusive, not certain the pair is unjustified")
	}
	stored, found, err := grants.Get(context.Background(), movedUID)
	require.NoError(t, err)
	require.True(t, found, "the marker/entry must be retained so a later pass can retry the revoke")
	assert.NotNil(t, stored.PendingRevoke, "the superseded pair's address must survive an inconclusive scan")
}

// sequencedKCOrgReader returns first on the initial read (the handler's
// prefetch) and rest on every later one (the live post-remove recheck), so a
// grant racing the prefetched slice can be simulated.
type sequencedKCOrgReader struct {
	first []*model.KeyContact
	rest  []*model.KeyContact
	calls int
}

func (r *sequencedKCOrgReader) ListKeyContactsForOrg(_ context.Context, _ string) ([]*model.KeyContact, error) {
	r.calls++
	if r.calls == 1 {
		return r.first, nil
	}
	return r.rest, nil
}

// TestInviteAcceptedService_Handle_SupersededRacingRegrant_LiveRecheckRepairs
// covers PRRT_kwDORegyoM6g_35f: the superseded-pair revoke must recheck
// against a fresh org read, not the prefetched slice, so a same-email sibling
// granted between the prefetch and the remove gets its tuple repaired.
func TestInviteAcceptedService_Handle_SupersededRacingRegrant_LiveRecheckRepairs(t *testing.T) {
	const orgUID = "001000000000000AAA"
	const movedUID = "kc-moved-3"
	store := mock.NewMockB2BOrgSettings()
	store.Seed(orgUID, &model.B2BOrgSettings{UID: orgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			movedUID: {MembershipUID: "m-A3", Username: "alice", Revision: 1},
		},
	}

	moved := &model.KeyContact{UID: movedUID, MembershipUID: "m-B3", Email: "alice@example.com", Status: "Active"}
	// Inactive: covers m-A3 in the prefetched slice without justifying the
	// pair, so the scan says unjustified and the remove publishes.
	dead := &model.KeyContact{UID: "kc-dead", MembershipUID: "m-A3", Email: "dead@example.com", Status: constants.RoleStatusInactive}
	// A same-email grant racing the remove: visible only to the fresh read.
	racing := &model.KeyContact{UID: "kc-race", MembershipUID: "m-A3", Email: "alice@example.com", Status: "Active"}
	reader := &sequencedKCOrgReader{
		first: []*model.KeyContact{moved, dead},
		rest:  []*model.KeyContact{moved, dead, racing},
	}

	invSvc := svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(inner),
		svc.WithInviteAcceptedKeyContactReader(reader),
		svc.WithInviteAcceptedPublisher(pub),
		svc.WithInviteAcceptedKeyContactGrantIndex(grants),
	)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: orgUID},
		},
	}
	err := invSvc.Handle(context.Background(), ev)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, reader.calls, 2, "the post-remove recheck must re-read the org live, not reuse the prefetched slice")
	removeIdx, repairIdx := -1, -1
	for i, subj := range pub.access {
		switch {
		case subj == fgaconstants.GenericMemberRemoveSubject && assert.ObjectsAreEqual(pub.accessMessages[i], svc.BuildKeyContactFGARemoveMessage("m-A3", "alice")):
			removeIdx = i
		case subj == fgaconstants.GenericMemberPutSubject && assert.ObjectsAreEqual(pub.accessMessages[i], svc.BuildKeyContactFGAPutMessage("m-A3", "alice")) && removeIdx >= 0:
			repairIdx = i
		}
	}
	require.GreaterOrEqual(t, removeIdx, 0, "the superseded pair must be revoked based on the prefetched scan; access calls: %v", pub.access)
	assert.Greater(t, repairIdx, removeIdx, "the racing same-email grant must be repaired with a compensating member_put after the remove")
}

// scriptedKCOrgReader returns pages[0] on the first read, pages[1] on the
// second, and the last page on every later read, so multi-stage races
// (prefetch, recheck, post-repair verification) can each see different state.
type scriptedKCOrgReader struct {
	pages [][]*model.KeyContact
	calls int
}

func (r *scriptedKCOrgReader) ListKeyContactsForOrg(_ context.Context, _ string) ([]*model.KeyContact, error) {
	r.calls++
	i := r.calls - 1
	if i >= len(r.pages) {
		i = len(r.pages) - 1
	}
	return r.pages[i], nil
}

// TestInviteAcceptedService_Handle_RepairRacedByDeactivation_TakedownAndRetry
// covers PRRT_kwDORegyoM6hA42L: when the sibling that justified the
// compensating put deactivates before the put lands, the post-put
// verification must remove the tuple again and keep retry state.
func TestInviteAcceptedService_Handle_RepairRacedByDeactivation_TakedownAndRetry(t *testing.T) {
	const orgUID = "001000000000000AAB"
	const movedUID = "kc-moved-4"
	store := mock.NewMockB2BOrgSettings()
	store.Seed(orgUID, &model.B2BOrgSettings{UID: orgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			movedUID: {MembershipUID: "m-A4", Username: "alice", Revision: 1},
		},
	}

	moved := &model.KeyContact{UID: movedUID, MembershipUID: "m-B4", Email: "alice@example.com", Status: "Active"}
	dead := &model.KeyContact{UID: "kc-dead-4", MembershipUID: "m-A4", Email: "dead@example.com", Status: constants.RoleStatusInactive}
	racing := &model.KeyContact{UID: "kc-race-4", MembershipUID: "m-A4", Email: "alice@example.com", Status: "Active"}
	racingGone := &model.KeyContact{UID: "kc-race-4", MembershipUID: "m-A4", Email: "alice@example.com", Status: constants.RoleStatusInactive}
	reader := &scriptedKCOrgReader{pages: [][]*model.KeyContact{
		{moved, dead},             // prefetch: m-A4 covered but unjustified, remove publishes
		{moved, dead, racing},     // post-remove recheck: racing grant found, repair put publishes
		{moved, dead, racingGone}, // post-repair verification: justification gone, takedown must run
	}}

	invSvc := svc.NewInviteAcceptedService(
		svc.WithInviteAcceptedSettingsReader(store),
		svc.WithInviteAcceptedOrgSettingsWriter(inner),
		svc.WithInviteAcceptedKeyContactReader(reader),
		svc.WithInviteAcceptedPublisher(pub),
		svc.WithInviteAcceptedKeyContactGrantIndex(grants),
	)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: orgUID},
		},
	}
	err := invSvc.Handle(context.Background(), ev)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, reader.calls, 3, "the repair must be verified against a fresh org read after the put")
	removeIdx, repairIdx, takedownIdx := -1, -1, -1
	for i, subj := range pub.access {
		switch {
		case subj == fgaconstants.GenericMemberRemoveSubject && assert.ObjectsAreEqual(pub.accessMessages[i], svc.BuildKeyContactFGARemoveMessage("m-A4", "alice")) && repairIdx < 0:
			removeIdx = i
		case subj == fgaconstants.GenericMemberPutSubject && assert.ObjectsAreEqual(pub.accessMessages[i], svc.BuildKeyContactFGAPutMessage("m-A4", "alice")) && removeIdx >= 0:
			repairIdx = i
		case subj == fgaconstants.GenericMemberRemoveSubject && assert.ObjectsAreEqual(pub.accessMessages[i], svc.BuildKeyContactFGARemoveMessage("m-A4", "alice")) && repairIdx >= 0:
			takedownIdx = i
		}
	}
	require.GreaterOrEqual(t, removeIdx, 0, "the superseded pair must be revoked first; access calls: %v", pub.access)
	require.Greater(t, repairIdx, removeIdx, "the racing grant must be repaired with a compensating member_put")
	assert.Greater(t, takedownIdx, repairIdx, "the verification must take the repaired tuple back down once its justification is gone")

	stored, found, err := grants.Get(context.Background(), movedUID)
	require.NoError(t, err)
	require.True(t, found, "the entry must be retained so a later pass can settle the race")
	assert.NotNil(t, stored.PendingRevoke, "the superseded pair's address must survive an uncertain repair")
}

func TestInviteAcceptedService_Handle_NilKeyContactDeps_NoPanic(t *testing.T) {
	// Nil keyContactReader/publisher (e.g. not yet wired) must not panic.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	inner := &countingWriter{inner: newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())}
	invSvc := newInviteAcceptedService(store, inner)

	ev := inviteapi.InviteServiceAcceptedEvent{
		Invite: inviteapi.Invite{
			AcceptedBy: "auth0|alice",
			Recipient:  inviteapi.Recipient{Email: "alice@example.com"},
			Resource:   inviteapi.Resource{Type: "b2b_org", UID: testOrgUID},
		},
	}
	require.NotPanics(t, func() {
		_ = invSvc.Handle(context.Background(), ev)
	})
}
