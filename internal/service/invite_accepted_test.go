// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"testing"

	inviteapi "github.com/linuxfoundation/lfx-v2-invite-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
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
