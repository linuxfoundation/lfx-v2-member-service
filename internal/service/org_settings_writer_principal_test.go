// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isConflict reports whether err is (or wraps) a pkgerrors.Conflict.
func isConflict(err error) bool {
	var c pkgerrors.Conflict
	return errors.As(err, &c)
}

// isValidation reports whether err is (or wraps) a pkgerrors.Validation.
func isValidation(err error) bool {
	var v pkgerrors.Validation
	return errors.As(err, &v)
}

// seedTwoAdmins seeds an org with two accepted admin writers (each with a username),
// the canonical shape we must protect: per-principal mutations must never disturb the
// other accepted member (the root cause of the prod incident was dropping username).
func seedTwoAdmins(store *mock.MockB2BOrgSettings) {
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Name: "Alice", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
			{Email: "bob@example.com", Name: "Bob", Username: "bob", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
}

func findUser(users []model.B2BOrgUser, email string) (model.B2BOrgUser, bool) {
	for _, u := range users {
		if u.Email == email {
			return u, true
		}
	}
	return model.B2BOrgUser{}, false
}

// stubB2BOrgReader is a configurable port.B2BOrgReader test double: GetB2BOrg returns org when
// set, otherwise NotFound. Used to exercise the AddPrincipal org-existence check on first add
// (the production MockB2BOrgReader always returns NotFound).
type stubB2BOrgReader struct{ org *model.B2BOrg }

func (s stubB2BOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	if s.org == nil {
		return nil, pkgerrors.NewNotFound("b2b org not found")
	}
	return s.org, nil
}

func (s stubB2BOrgReader) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s stubB2BOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// ── AddPrincipal ──────────────────────────────────────────────────────────────

func TestOrgSettingsWriter_AddPrincipal_PreservesExistingMembers(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "Carol@Example.com", InvitedAs: "auditor", Name: "Carol",
	})

	require.NoError(t, err)
	// Both original admins must remain accepted WITH their usernames (regression guard).
	alice, ok := findUser(result.Writers, "alice@example.com")
	require.True(t, ok)
	assert.Equal(t, model.InviteStatusAccepted, alice.EffectiveStatus())
	assert.Equal(t, "alice", alice.Username)
	bob, ok := findUser(result.Writers, "bob@example.com")
	require.True(t, ok)
	assert.Equal(t, "bob", bob.Username)
	// New invitee lands as a pending auditor (email lowercased, no username).
	carol, ok := findUser(result.Auditors, "carol@example.com")
	require.True(t, ok)
	assert.Equal(t, model.InviteStatusPending, carol.EffectiveStatus())
	assert.Empty(t, carol.Username)
}

func TestOrgSettingsWriter_AddPrincipal_DuplicateIsConflict(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "alice@example.com", InvitedAs: "auditor",
	})
	require.Error(t, err)
	assert.True(t, isConflict(err), "re-inviting an existing member must be a Conflict, got %T", err)
}

func TestOrgSettingsWriter_AddPrincipal_RejectsBadRole(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "x@example.com", InvitedAs: "owner",
	})
	require.Error(t, err)
}

// TestOrgSettingsWriter_AddPrincipal_EnforcesMaxPrincipals verifies the per-principal
// add rejects a write that would push a relation list past the maxPrincipals bound,
// matching the cap enforced by the bulk Update path.
func TestOrgSettingsWriter_AddPrincipal_EnforcesMaxPrincipals(t *testing.T) {
	const maxPrincipals = 700
	auditors := make([]model.B2BOrgUser, 0, maxPrincipals)
	for i := 0; i < maxPrincipals; i++ {
		auditors = append(auditors, model.B2BOrgUser{
			Email:        fmt.Sprintf("user%d@example.com", i),
			InvitedAs:    "auditor",
			InviteStatus: model.InviteStatusPending,
		})
	}
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
		Auditors: auditors,
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "overflow@example.com", InvitedAs: "auditor",
	})
	require.Error(t, err)
	assert.True(t, isValidation(err), "exceeding maxPrincipals must be a Validation error, got %T", err)
}

// TestOrgSettingsWriter_AddPrincipal_DualListLiveMatchIsConflict guards the edge case where
// the same email appears in BOTH relations (possible via the bulk PUT path): a revoked writer
// entry plus a still-accepted auditor entry. Re-inviting must be a Conflict, not a silent drop
// of the live auditor grant.
func TestOrgSettingsWriter_AddPrincipal_DualListLiveMatchIsConflict(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
			{Email: "dana@example.com", InvitedAs: "writer", InviteStatus: model.InviteStatusRevoked},
		},
		Auditors: []model.B2BOrgUser{
			{Email: "dana@example.com", InvitedAs: "auditor", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "dana@example.com", InvitedAs: "writer",
	})
	require.Error(t, err)
	assert.True(t, isConflict(err), "a live grant in either relation must be a Conflict, got %T", err)
}

// TestOrgSettingsWriter_AddPrincipal_NonexistentOrgIsNotFound verifies that a first add (no
// settings record yet) to an org that does not exist is rejected with NotFound rather than
// silently creating an orphan settings record.
func TestOrgSettingsWriter_AddPrincipal_NonexistentOrgIsNotFound(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // no settings seeded -> create path
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor",
	})
	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "adding to a nonexistent org must be NotFound, got %T", err)
}

// TestOrgSettingsWriter_AddPrincipal_FirstAddCreatesSettingsWhenOrgExists verifies the happy
// first-add path: when no settings exist yet but the parent org does, the add succeeds and
// creates the settings record.
func TestOrgSettingsWriter_AddPrincipal_FirstAddCreatesSettingsWhenOrgExists(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // no settings yet
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(stubB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID, Name: "Acme"}}),
		svc.WithOrgSettingsPublisher(mock.NewMockMemberPublisher()),
	)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor", Name: "Carol",
	})
	require.NoError(t, err)
	carol, ok := findUser(result.Auditors, "carol@example.com")
	require.True(t, ok, "carol must be added on first add")
	assert.Equal(t, model.InviteStatusPending, carol.EffectiveStatus())
}

// TestOrgSettingsWriter_AddPrincipal_IfMatchMismatch verifies the optional If-Match precondition
// is enforced on add: a stale ETag is rejected with PreconditionFailed before any write.
func TestOrgSettingsWriter_AddPrincipal_IfMatchMismatch(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor", IfMatch: "stale-etag",
	})
	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "stale If-Match must be a PreconditionFailed, got %T", err)
}

// TestOrgSettingsWriter_AddPrincipal_IfMatchOnNilSettingsReturnsPreconditionFailed verifies that
// supplying If-Match when no settings record exists yet is a PreconditionFailed (you cannot match
// against a record that does not exist). The If-Match check runs before the org-existence check.
func TestOrgSettingsWriter_AddPrincipal_IfMatchOnNilSettingsReturnsPreconditionFailed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // no settings seeded -> existing == nil
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor", IfMatch: "stale-etag",
	})
	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "If-Match against nil settings must be PreconditionFailed, got %T", err)
}

// TestOrgSettingsWriter_AddPrincipal_IfMatchMatchSucceeds verifies a matching If-Match ETag is
// accepted and the add proceeds.
func TestOrgSettingsWriter_AddPrincipal_IfMatchMatchSucceeds(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	seeded, _, err := store.GetSettings(context.Background(), testOrgUID)
	require.NoError(t, err)
	etagVal, err := etag.LFXEtag(seeded)
	require.NoError(t, err)

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor", IfMatch: etagVal,
	})
	require.NoError(t, err)
	_, ok := findUser(result.Auditors, "carol@example.com")
	assert.True(t, ok, "carol must be added when If-Match matches")
}

// ── ChangePrincipalRole ───────────────────────────────────────────────────────

func TestOrgSettingsWriter_ChangeRole_PreservesUsernameAndOtherMembers(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "auditor",
	})

	require.NoError(t, err)
	// Bob moved to auditors, still accepted, username intact.
	bob, ok := findUser(result.Auditors, "bob@example.com")
	require.True(t, ok)
	assert.Equal(t, model.InviteStatusAccepted, bob.EffectiveStatus())
	assert.Equal(t, "bob", bob.Username)
	assert.Equal(t, "auditor", bob.InvitedAs)
	// Alice untouched and still an accepted writer.
	alice, ok := findUser(result.Writers, "alice@example.com")
	require.True(t, ok)
	assert.Equal(t, "alice", alice.Username)
	_, stillWriter := findUser(result.Writers, "bob@example.com")
	assert.False(t, stillWriter, "bob must no longer be a writer")
}

func TestOrgSettingsWriter_ChangeRole_LastAdminDemotionBlocked(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "alice@example.com", InvitedAs: "auditor",
	})
	require.Error(t, err)
	assert.True(t, isConflict(err), "demoting the only Admin must be a Conflict, got %T", err)
}

func TestOrgSettingsWriter_ChangeRole_NotFound(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "ghost@example.com", InvitedAs: "auditor",
	})
	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "changing the role of a ghost principal must be NotFound, got %T", err)
}

func TestOrgSettingsWriter_ChangeRole_IfMatchMismatch(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "auditor", IfMatch: "stale-etag",
	})
	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "stale If-Match must be PreconditionFailed, got %T", err)
}

func TestOrgSettingsWriter_ChangeRole_IfMatchMatchSucceeds(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	seeded, _, err := store.GetSettings(context.Background(), testOrgUID)
	require.NoError(t, err)
	etagVal, err := etag.LFXEtag(seeded)
	require.NoError(t, err)

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	_, err = writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "auditor", IfMatch: etagVal,
	})
	assert.NoError(t, err)
}

// TestOrgSettingsWriter_ChangeRole_SameRoleIsNoOp verifies that changing a principal to the
// role it already holds short-circuits without persisting — no revision bump, no republish.
// A forced Put error proves the write path is never reached.
func TestOrgSettingsWriter_ChangeRole_SameRoleIsNoOp(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	store.SetPutError(assert.AnError) // any persist attempt would surface as an error
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "writer",
	})
	require.NoError(t, err, "same-role change must short-circuit without persisting")
	bob, ok := findUser(result.Writers, "bob@example.com")
	require.True(t, ok, "bob must remain a writer")
	assert.Equal(t, "bob", bob.Username)
}

// TestOrgSettingsWriter_ChangeRole_DualListMovesMostLiveEntry guards the duplicate-email case
// for role changes: a revoked writer + an accepted auditor for the same email. Changing the
// role to writer must promote the live (accepted, username-bearing) entry — not the revoked
// one — and collapse the duplicate, so access is preserved rather than revoked.
func TestOrgSettingsWriter_ChangeRole_DualListMovesMostLiveEntry(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
			{Email: "dana@example.com", InvitedAs: "writer", InviteStatus: model.InviteStatusRevoked},
		},
		Auditors: []model.B2BOrgUser{
			{Email: "dana@example.com", Username: "dana", InvitedAs: "auditor", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "dana@example.com", InvitedAs: "writer",
	})
	require.NoError(t, err)
	// Dana is now a single accepted writer carrying her username (access preserved/promoted).
	dana, ok := findUser(result.Writers, "dana@example.com")
	require.True(t, ok, "dana must be present as a writer")
	assert.Equal(t, model.InviteStatusAccepted, dana.EffectiveStatus())
	assert.Equal(t, "dana", dana.Username)
	// No leftover dana entry in auditors (duplicate collapsed).
	_, stillAuditor := findUser(result.Auditors, "dana@example.com")
	assert.False(t, stillAuditor, "dana's duplicate auditor entry must be collapsed")
}

// TestOrgSettingsWriter_RemovePrincipal_UsernamelessAcceptedIsNotAdmin verifies the last-Admin
// invariant counts only accepted writers with a non-empty username (the FGA-tuple condition).
// Removing the only real admin must be blocked even when a non-functional accepted writer
// (no username) would remain.
func TestOrgSettingsWriter_RemovePrincipal_UsernamelessAcceptedIsNotAdmin(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
			{Email: "ghost@example.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted}, // accepted but no username
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.RemovePrincipal(context.Background(), svc.B2BOrgSettingsRemovePrincipal{
		OrgUID: testOrgUID, Email: "alice@example.com",
	})
	require.Error(t, err)
	assert.True(t, isConflict(err), "removing the only username-bearing admin must be a Conflict, got %T", err)
}

// TestOrgSettingsWriter_RemovePrincipal_OnboardingWindowAllowed verifies the differential
// last-Admin check does not freeze the org during onboarding: when the only writer is still a
// pending invite (no username, so zero functional admins), removing an unrelated pending
// auditor is allowed rather than rejected with a spurious "must keep at least one Admin".
func TestOrgSettingsWriter_RemovePrincipal_OnboardingWindowAllowed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", InvitedAs: "writer", InviteStatus: model.InviteStatusPending}, // invited, not yet accepted
		},
		Auditors: []model.B2BOrgUser{
			{Email: "carol@example.com", InvitedAs: "auditor", InviteStatus: model.InviteStatusPending},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.RemovePrincipal(context.Background(), svc.B2BOrgSettingsRemovePrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com",
	})
	require.NoError(t, err, "removing a non-admin during onboarding must not be blocked")
	_, gone := findUser(result.Auditors, "carol@example.com")
	assert.False(t, gone, "carol must be removed")
	// The pending admin invite is left untouched.
	_, aliceStays := findUser(result.Writers, "alice@example.com")
	assert.True(t, aliceStays, "the pending admin invite must remain")
}

// TestOrgSettingsWriter_ChangeRole_OnboardingWindowAllowed verifies a benign role change is not
// frozen during the onboarding window (zero functional admins): promoting a pending auditor to
// writer succeeds.
func TestOrgSettingsWriter_ChangeRole_OnboardingWindowAllowed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", InvitedAs: "writer", InviteStatus: model.InviteStatusPending},
		},
		Auditors: []model.B2BOrgUser{
			{Email: "carol@example.com", InvitedAs: "auditor", InviteStatus: model.InviteStatusPending},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "writer",
	})
	require.NoError(t, err, "a role change during onboarding must not be blocked")
	carol, ok := findUser(result.Writers, "carol@example.com")
	require.True(t, ok, "carol must now be a writer")
	assert.Equal(t, "writer", carol.InvitedAs)
}

// TestOrgSettingsWriter_ChangeRole_EnforcesMaxPrincipalsOnTarget verifies a role move cannot
// push the destination relation past the per-list cap (a move grows the target list by one).
func TestOrgSettingsWriter_ChangeRole_EnforcesMaxPrincipalsOnTarget(t *testing.T) {
	const maxPrincipals = 700
	writers := make([]model.B2BOrgUser, 0, maxPrincipals)
	for i := 0; i < maxPrincipals; i++ {
		writers = append(writers, model.B2BOrgUser{
			Email:        fmt.Sprintf("w%d@example.com", i),
			Username:     fmt.Sprintf("w%d", i),
			InvitedAs:    "writer",
			InviteStatus: model.InviteStatusAccepted,
		})
	}
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID:     testOrgUID,
		Writers: writers, // already at the cap
		Auditors: []model.B2BOrgUser{
			{Email: "carol@example.com", InvitedAs: "auditor", InviteStatus: model.InviteStatusPending},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.ChangePrincipalRole(context.Background(), svc.B2BOrgSettingsChangeRole{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "writer",
	})
	require.Error(t, err)
	assert.True(t, isValidation(err), "moving into a full writers list must be a Validation error, got %T", err)
}

// ── RemovePrincipal ───────────────────────────────────────────────────────────

func TestOrgSettingsWriter_RemovePrincipal_DropsOnlyTarget(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
		Auditors: []model.B2BOrgUser{
			{Email: "carol@example.com", InvitedAs: "auditor", InviteStatus: model.InviteStatusPending},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	result, err := writer.RemovePrincipal(context.Background(), svc.B2BOrgSettingsRemovePrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com",
	})
	require.NoError(t, err)
	_, gone := findUser(result.Auditors, "carol@example.com")
	assert.False(t, gone, "carol must be removed")
	alice, ok := findUser(result.Writers, "alice@example.com")
	require.True(t, ok, "alice must remain")
	assert.Equal(t, "alice", alice.Username)
}

func TestOrgSettingsWriter_RemovePrincipal_LastAdminBlocked(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.RemovePrincipal(context.Background(), svc.B2BOrgSettingsRemovePrincipal{
		OrgUID: testOrgUID, Email: "alice@example.com",
	})
	require.Error(t, err)
	assert.True(t, isConflict(err), "removing the only Admin must be a Conflict, got %T", err)
}

func TestOrgSettingsWriter_RemovePrincipal_NotFound(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.RemovePrincipal(context.Background(), svc.B2BOrgSettingsRemovePrincipal{
		OrgUID: testOrgUID, Email: "ghost@example.com",
	})
	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "removing a ghost principal must be NotFound, got %T", err)
}

func TestOrgSettingsWriter_RemovePrincipal_PutConflictPropagates(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	seedTwoAdmins(store)
	store.SetPutError(assert.AnError)
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	_, err := writer.RemovePrincipal(context.Background(), svc.B2BOrgSettingsRemovePrincipal{
		OrgUID: testOrgUID, Email: "bob@example.com",
	})
	require.Error(t, err)
}
