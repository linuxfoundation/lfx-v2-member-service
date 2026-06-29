// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
)

// ─── configurable stubs ────────────────────────────────────────────────────────

type stubB2BOrgWriterUC struct {
	org *model.B2BOrg
	err error
}

func (s stubB2BOrgWriterUC) Create(_ context.Context, _ string) (*model.B2BOrg, error) {
	return s.org, s.err
}
func (s stubB2BOrgWriterUC) Update(_ context.Context, _ string, _ model.B2BOrgInput, _ string) (*model.B2BOrg, error) {
	return s.org, s.err
}

type stubKeyContactWriterUC struct {
	kc  *model.KeyContact
	err error
}

func (s stubKeyContactWriterUC) Create(_ context.Context, _ usecaseSvc.KeyContactCreateInput) (*model.KeyContact, error) {
	return s.kc, s.err
}
func (s stubKeyContactWriterUC) Update(_ context.Context, _ usecaseSvc.KeyContactUpdateInput) (*model.KeyContact, error) {
	return s.kc, s.err
}
func (s stubKeyContactWriterUC) Delete(_ context.Context, _ usecaseSvc.KeyContactDeleteInput) error {
	return s.err
}

type stubOrgSettingsWriterUC struct {
	settings *model.B2BOrgSettings
	err      error
}

func (s stubOrgSettingsWriterUC) Update(_ context.Context, _ usecaseSvc.B2BOrgSettingsUpdate) (*model.B2BOrgSettings, error) {
	return s.settings, s.err
}

func (s stubOrgSettingsWriterUC) AddPrincipal(_ context.Context, _ usecaseSvc.B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error) {
	return s.settings, s.err
}

func (s stubOrgSettingsWriterUC) ChangePrincipalRole(_ context.Context, _ usecaseSvc.B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error) {
	return s.settings, s.err
}

func (s stubOrgSettingsWriterUC) RemovePrincipal(_ context.Context, _ usecaseSvc.B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error) {
	return s.settings, s.err
}

// ─── fixtures ─────────────────────────────────────────────────────────────────

// seededB2BOrgReader returns a fixed org for any UID.
type seededB2BOrgReader struct{ org *model.B2BOrg }

func (r *seededB2BOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	if r.org == nil {
		return nil, pkgerrors.NewNotFound("b2b org not found")
	}
	return r.org, nil
}
func (r *seededB2BOrgReader) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (r *seededB2BOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// sampleB2BOrg is the canonical test fixture returned by seeded mocks.
var sampleB2BOrg = &model.B2BOrg{
	UID:       "lf-uid-001",
	SFID:      "001000000000001AAA",
	Name:      "Linux Foundation",
	Website:   "https://linuxfoundation.org",
	Industry:  "Technology",
	Status:    "Active",
	CreatedAt: time.Date(2020, 1, 15, 10, 30, 0, 0, time.UTC),
	UpdatedAt: time.Date(2024, 6, 1, 8, 0, 0, 0, time.UTC),
}

// ─── functional-options test builder ──────────────────────────────────────────

type svcBuilder struct {
	auth            domain.Authenticator
	storage         port.MemberReader
	b2bOrgReader    port.B2BOrgReader
	pmReader        port.ProjectMembershipReader
	settingsR       port.B2BOrgSettingsReader
	b2bOrgWriter    usecaseSvc.B2BOrgWriter
	kcWriter        usecaseSvc.KeyContactWriter
	settingsW       usecaseSvc.OrgSettingsWriter
	workspaceWriter usecaseSvc.WorkspaceWriter
	runner          *usecaseSvc.Runner
}

type svcOpt func(*svcBuilder)

func withB2BOrgReader(r port.B2BOrgReader) svcOpt {
	return func(b *svcBuilder) { b.b2bOrgReader = r }
}
func withB2BOrgWriterUC(w usecaseSvc.B2BOrgWriter) svcOpt {
	return func(b *svcBuilder) { b.b2bOrgWriter = w }
}
func withKeyContactWriterUC(w usecaseSvc.KeyContactWriter) svcOpt {
	return func(b *svcBuilder) { b.kcWriter = w }
}
func withPMReader(r port.ProjectMembershipReader) svcOpt {
	return func(b *svcBuilder) { b.pmReader = r }
}
func withOrgSettingsStore(store *mock.MockB2BOrgSettings) svcOpt {
	return func(b *svcBuilder) {
		b.settingsR = store
		b.settingsW = usecaseSvc.NewOrgSettingsWriter(
			usecaseSvc.WithOrgSettingsReader(store),
			usecaseSvc.WithOrgSettingsWriter(store),
			usecaseSvc.WithOrgSettingsB2BOrgReader(&seededB2BOrgReader{org: sampleB2BOrg}),
			usecaseSvc.WithOrgSettingsPublisher(mock.NewMockMemberPublisher()),
		)
	}
}
func withBackfillRunner(r *usecaseSvc.Runner) svcOpt {
	return func(b *svcBuilder) { b.runner = r }
}

func newTestSvc(opts ...svcOpt) membershipservice.Service {
	mockRepo := mock.NewMockMembershipRepository()
	b := &svcBuilder{
		auth:         &auth.MockJWTAuth{},
		storage:      mockRepo,
		b2bOrgReader: mock.NewMockB2BOrgReader(),
		pmReader:     mock.NewMockProjectMembershipReader(),
		settingsR:    mock.NewMockB2BOrgSettings(),
		b2bOrgWriter: stubB2BOrgWriterUC{org: sampleB2BOrg},
		kcWriter:     stubKeyContactWriterUC{},
		settingsW:    stubOrgSettingsWriterUC{settings: &model.B2BOrgSettings{}},
	}
	for _, o := range opts {
		o(b)
	}
	return NewMembershipService(b.auth, b.storage, b.b2bOrgReader,
		b.pmReader, b.settingsR, b.b2bOrgWriter, b.kcWriter, b.settingsW, b.workspaceWriter, b.runner)
}

// ─── B2BOrg handler tests ──────────────────────────────────────────────────────

func TestGetB2bOrg_NotFound(t *testing.T) {
	svc := newTestSvc()

	_, err := svc.GetB2bOrg(context.Background(), &membershipservice.GetB2bOrgPayload{
		UID: "nonexistent-uid",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotFound", serviceErr.Name)
}

func TestGetB2bOrg_Happy(t *testing.T) {
	svc := newTestSvc(withB2BOrgReader(&seededB2BOrgReader{org: sampleB2BOrg}))

	result, err := svc.GetB2bOrg(context.Background(), &membershipservice.GetB2bOrgPayload{UID: "lf-uid-001"})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.B2bOrg)
	assert.Equal(t, "lf-uid-001", *result.B2bOrg.UID)
	assert.Equal(t, "Linux Foundation", *result.B2bOrg.Name)
	assert.NotNil(t, result.Etag, "ETag must be set")
	assert.NotNil(t, result.LastModified, "Last-Modified must be set")
}

func TestCreateB2bOrg_MockReturnsNotImplemented(t *testing.T) {
	svc := newTestSvc(withB2BOrgWriterUC(stubB2BOrgWriterUC{err: pkgerrors.NewNotImplemented("not implemented")}))

	_, err := svc.CreateB2bOrg(context.Background(), &membershipservice.CreateB2bOrgPayload{
		Sfid: "001000000000001AAA",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotImplemented", serviceErr.Name)
}

// ─── GetProjectMembership handler tests ───────────────────────────────────────

func TestGetProjectMembership_Happy(t *testing.T) {
	now := time.Now()
	sampleMembership := &model.ProjectMembership{
		UID:             "membership-uid-001",
		TierUID:         "tier-uid-001",
		ProjectUID:      "project-uid-001",
		ProjectSlug:     "linux-foundation",
		Status:          "Active",
		Year:            "2025",
		Tier:            "Gold",
		AutoRenew:       true,
		CompanyName:     "Acme Corp",
		CompanyLogoURL:  "https://acme.com/logo.png",
		CompanyDomain:   "https://acme.com",
		TierName:        "Gold Membership",
		TierFamily:      "Membership",
		TierProductType: "Corporate",
		CreatedAt:       now.Add(-24 * time.Hour),
		UpdatedAt:       now,
	}
	pmr := &mockProjectMembershipReader{membership: sampleMembership, lastMod: now}
	svc := newTestSvc(withPMReader(pmr))

	result, err := svc.GetProjectMembership(context.Background(), &membershipservice.GetProjectMembershipPayload{
		UID: "membership-uid-001",
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ProjectMembership)
	assert.Equal(t, "membership-uid-001", *result.ProjectMembership.UID)
	assert.Equal(t, "Acme Corp", *result.ProjectMembership.CompanyName)
	assert.Equal(t, "Gold Membership", *result.ProjectMembership.TierName)
	assert.Equal(t, "linux-foundation", *result.ProjectMembership.ProjectSlug)
	assert.NotNil(t, result.Etag, "ETag must be set")
	assert.NotNil(t, result.LastModified, "Last-Modified must be set")
}

func TestGetProjectMembership_NotFound(t *testing.T) {
	pmr := &mockProjectMembershipReader{err: pkgerrors.NewNotFound("membership not found")}
	svc := newTestSvc(withPMReader(pmr))

	_, err := svc.GetProjectMembership(context.Background(), &membershipservice.GetProjectMembershipPayload{
		UID: "nonexistent-uid",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotFound", serviceErr.Name)
}

func TestGetProjectMembership_ReaderError(t *testing.T) {
	pmr := &mockProjectMembershipReader{err: pkgerrors.NewUnexpected("reader failed", fmt.Errorf("salesforce error"))}
	svc := newTestSvc(withPMReader(pmr))

	_, err := svc.GetProjectMembership(context.Background(), &membershipservice.GetProjectMembershipPayload{
		UID: "test-uid",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "InternalServerError", serviceErr.Name)
}

// ─── GetKeyContact handler tests ──────────────────────────────────────────────

func TestGetKeyContact_Happy(t *testing.T) {
	svc := newTestSvc()

	result, err := svc.GetKeyContact(context.Background(), &membershipservice.GetKeyContactPayload{
		UID:           "contact-role-1",
		MembershipUID: "11111111-1111-1111-1111-111111111111",
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.KeyContact)
	assert.Equal(t, "contact-role-1", *result.KeyContact.UID)
	assert.Equal(t, "John", *result.KeyContact.FirstName)
	assert.Equal(t, "Doe", *result.KeyContact.LastName)
	assert.NotNil(t, result.Etag, "ETag must be set")
	assert.NotNil(t, result.LastModified, "Last-Modified must be set")
}

func TestGetKeyContact_NotFound(t *testing.T) {
	svc := newTestSvc()

	_, err := svc.GetKeyContact(context.Background(), &membershipservice.GetKeyContactPayload{
		UID: "nonexistent-uid",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotFound", serviceErr.Name)
}

// TestGetKeyContact_MembershipMismatch verifies that GetKeyContact returns 404 (not 403)
// when the contact UID exists but belongs to a different membership than the path supplies.
func TestGetKeyContact_MembershipMismatch(t *testing.T) {
	svc := newTestSvc()

	_, err := svc.GetKeyContact(context.Background(), &membershipservice.GetKeyContactPayload{
		UID:           "contact-role-1",       // exists in membership-1
		MembershipUID: "wrong-membership-uid", // mismatch → 404
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotFound", serviceErr.Name, "must return 404 (not 403) to avoid leaking existence")
}

// ─── Key contact write handler smoke tests ────────────────────────────────────

func TestCreateKeyContact_MockReturnsNotImplemented(t *testing.T) {
	svc := newTestSvc(withKeyContactWriterUC(stubKeyContactWriterUC{err: pkgerrors.NewNotImplemented("not implemented")}))

	_, err := svc.CreateKeyContact(context.Background(), &membershipservice.CreateKeyContactPayload{
		MembershipUID: "11111111-1111-1111-1111-111111111111",
		Email:         "test@example.com",
		FirstName:     "Test",
		LastName:      "User",
		Role:          "Billing Contact",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotImplemented", serviceErr.Name)
}

func TestUpdateKeyContact_MockReturnsNotImplemented(t *testing.T) {
	svc := newTestSvc(withKeyContactWriterUC(stubKeyContactWriterUC{err: pkgerrors.NewNotImplemented("not implemented")}))

	_, err := svc.UpdateKeyContact(context.Background(), &membershipservice.UpdateKeyContactPayload{
		UID:           "contact-role-1",
		MembershipUID: "11111111-1111-1111-1111-111111111111",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotImplemented", serviceErr.Name)
}

func TestDeleteKeyContact_MockReturnsNotImplemented(t *testing.T) {
	svc := newTestSvc(withKeyContactWriterUC(stubKeyContactWriterUC{err: pkgerrors.NewNotImplemented("not implemented")}))

	err := svc.DeleteKeyContact(context.Background(), &membershipservice.DeleteKeyContactPayload{
		UID:           "contact-role-1",
		MembershipUID: "11111111-1111-1111-1111-111111111111",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "NotImplemented", serviceErr.Name)
}

// ─── Membership-alignment 404 tests (cross-membership checks stay in handler) ──

// TestUpdateKeyContact_MembershipMismatch verifies that UpdateKeyContact returns 404
// when the contact UID does not belong to the supplied membership_uid.
func TestUpdateKeyContact_MembershipMismatch(t *testing.T) {
	svc := newTestSvc()

	_, err := svc.UpdateKeyContact(context.Background(), &membershipservice.UpdateKeyContactPayload{
		UID:           "contact-role-1",
		MembershipUID: "wrong-membership-uid",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr))
	assert.Equal(t, "NotFound", serviceErr.Name, "must return 404 to avoid leaking existence")
}

// TestDeleteKeyContact_MembershipMismatch verifies that DeleteKeyContact returns 404
// when the orchestrator returns NotFound for missing or wrong-parent requests.
func TestDeleteKeyContact_MembershipMismatch(t *testing.T) {
	svc := newTestSvc(withKeyContactWriterUC(stubKeyContactWriterUC{
		err: pkgerrors.NewNotFound("key contact not found in membership"),
	}))

	err := svc.DeleteKeyContact(context.Background(), &membershipservice.DeleteKeyContactPayload{
		UID:           "contact-role-1",
		MembershipUID: "wrong-membership-uid",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr))
	assert.Equal(t, "NotFound", serviceErr.Name, "orchestrator NotFound must map to 404")
}

// TestCreateKeyContact_MembershipNotFound verifies that CreateKeyContact returns 404
// when the orchestrator cannot find the membership.
func TestCreateKeyContact_MembershipNotFound(t *testing.T) {
	svc := newTestSvc(withKeyContactWriterUC(stubKeyContactWriterUC{err: pkgerrors.NewNotFound("membership not found")}))

	_, err := svc.CreateKeyContact(context.Background(), &membershipservice.CreateKeyContactPayload{
		MembershipUID: "nonexistent-membership",
		Email:         "test@example.com",
		FirstName:     "Test",
		LastName:      "User",
		Role:          "Billing Contact",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr))
	assert.Equal(t, "NotFound", serviceErr.Name)
}

// ─── OrgSettings handler tests ────────────────────────────────────────────────

func TestGetB2bOrgSettings_NoSettingsYet(t *testing.T) {
	svc := newTestSvc(withOrgSettingsStore(mock.NewMockB2BOrgSettings()))

	result, err := svc.GetB2bOrgSettings(context.Background(), &membershipservice.GetB2bOrgSettingsPayload{
		UID: "lf-uid-001",
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Settings)
	assert.Empty(t, result.Settings.Writers, "no settings stored → empty writers")
	assert.Empty(t, result.Settings.Auditors, "no settings stored → empty auditors")
}

func TestGetB2bOrgSettings_WithSettings(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := mock.NewMockB2BOrgSettings()
	store.Seed("lf-uid-001", &model.B2BOrgSettings{
		Writers: []model.B2BOrgUser{
			{
				Email:        "alice@example.com",
				Username:     "alice",
				InvitedAs:    "writer",
				InviteStatus: model.InviteStatusAccepted,
				CreatedAt:    now,
				UpdatedAt:    now,
			},
		},
		Auditors: []model.B2BOrgUser{
			{
				Email:        "bob@example.com",
				InvitedAs:    "auditor",
				InviteStatus: model.InviteStatusPending,
				CreatedAt:    now,
				UpdatedAt:    now,
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}, 5)

	svc := newTestSvc(withOrgSettingsStore(store))
	result, err := svc.GetB2bOrgSettings(context.Background(), &membershipservice.GetB2bOrgSettingsPayload{
		UID: "lf-uid-001",
	})

	require.NoError(t, err)
	require.NotNil(t, result.Settings)
	require.Len(t, result.Settings.Writers, 1, "must have one writer")
	assert.Equal(t, "alice@example.com", result.Settings.Writers[0].Email)
	assert.Equal(t, "alice", *result.Settings.Writers[0].Username)
	require.NotNil(t, result.Settings.Writers[0].InviteStatus)
	assert.Equal(t, "accepted", *result.Settings.Writers[0].InviteStatus)

	require.Len(t, result.Settings.Auditors, 1, "must have one auditor")
	assert.Equal(t, "bob@example.com", result.Settings.Auditors[0].Email)
	require.NotNil(t, result.Settings.Auditors[0].InviteStatus)
	assert.Equal(t, "pending", *result.Settings.Auditors[0].InviteStatus)
}

// TestUpdateB2bOrgSettings_Create verifies that when no prior settings exist a
// new record is created and returned, and that ETag/Last-Modified headers are set.
func TestUpdateB2bOrgSettings_Create(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	svc := newTestSvc(withOrgSettingsStore(store))

	username := "alice"
	result, err := svc.UpdateB2bOrgSettings(context.Background(), &membershipservice.UpdateB2bOrgSettingsPayload{
		UID: "lf-uid-001",
		Writers: []*membershipservice.OrgUser{
			{Email: "alice@example.com", InvitedAs: "writer", Username: &username},
		},
		Auditors: []*membershipservice.OrgUser{},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Settings)
	require.Len(t, result.Settings.Writers, 1)
	assert.Equal(t, "alice@example.com", result.Settings.Writers[0].Email)
	assert.Equal(t, "alice", *result.Settings.Writers[0].Username)
	require.NotNil(t, result.Settings.Writers[0].InviteStatus)
	assert.Equal(t, "accepted", *result.Settings.Writers[0].InviteStatus)
	assert.Empty(t, result.Settings.Auditors)
}

// TestUpdateB2bOrgSettings_Conflict verifies that a stale revision returns a
// Goa Conflict error.
func TestUpdateB2bOrgSettings_Conflict(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.SetPutError(pkgerrors.NewConflict("stale revision"))

	svc := newTestSvc(withOrgSettingsStore(store))
	_, err := svc.UpdateB2bOrgSettings(context.Background(), &membershipservice.UpdateB2bOrgSettingsPayload{
		UID:     "lf-uid-001",
		Writers: []*membershipservice.OrgUser{},
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr))
	assert.Equal(t, "Conflict", serviceErr.Name)
}

// TestAddB2bOrgSettingsUser_PreservesExistingMembers verifies a per-principal add
// keeps existing accepted members (with usernames) intact and lands the invitee as pending.
func TestAddB2bOrgSettingsUser_PreservesExistingMembers(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed("lf-uid-001", &model.B2BOrgSettings{
		UID: "lf-uid-001",
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	svc := newTestSvc(withOrgSettingsStore(store))

	result, err := svc.AddB2bOrgSettingsUser(context.Background(), &membershipservice.AddB2bOrgSettingsUserPayload{
		UID: "lf-uid-001", Email: "carol@example.com", InvitedAs: "auditor",
	})

	require.NoError(t, err)
	require.NotNil(t, result.Settings)
	require.Len(t, result.Settings.Writers, 1)
	assert.Equal(t, "alice", *result.Settings.Writers[0].Username, "existing admin username must survive")
	assert.Equal(t, "accepted", *result.Settings.Writers[0].InviteStatus)
	require.Len(t, result.Settings.Auditors, 1)
	assert.Equal(t, "carol@example.com", result.Settings.Auditors[0].Email)
	assert.Equal(t, "pending", *result.Settings.Auditors[0].InviteStatus)
	assert.NotNil(t, result.Etag)
}

// TestUpdateB2bOrgSettingsUserRole_LastAdminBlocked verifies the last-Admin invariant
// surfaces as a Goa Conflict from the role-change handler.
func TestUpdateB2bOrgSettingsUserRole_LastAdminBlocked(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed("lf-uid-001", &model.B2BOrgSettings{
		UID: "lf-uid-001",
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	svc := newTestSvc(withOrgSettingsStore(store))

	_, err := svc.UpdateB2bOrgSettingsUserRole(context.Background(), &membershipservice.UpdateB2bOrgSettingsUserRolePayload{
		UID: "lf-uid-001", Email: "alice@example.com", InvitedAs: "auditor",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr))
	assert.Equal(t, "Conflict", serviceErr.Name)
}

// TestDeleteB2bOrgSettingsUser_NotFound verifies removing an absent principal returns NotFound.
func TestDeleteB2bOrgSettingsUser_NotFound(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed("lf-uid-001", &model.B2BOrgSettings{
		UID: "lf-uid-001",
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	svc := newTestSvc(withOrgSettingsStore(store))

	_, err := svc.DeleteB2bOrgSettingsUser(context.Background(), &membershipservice.DeleteB2bOrgSettingsUserPayload{
		UID: "lf-uid-001", Email: "ghost@example.com",
	})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr))
	assert.Equal(t, "NotFound", serviceErr.Name)
}

// ─── mockProjectMembershipReader ──────────────────────────────────────────────

type mockProjectMembershipReader struct {
	membership *model.ProjectMembership
	lastMod    time.Time
	err        error
}

func (m *mockProjectMembershipReader) AssembleProjectMembership(_ context.Context, _ string) (*model.ProjectMembership, time.Time, error) {
	return m.membership, m.lastMod, m.err
}

// ─── compile-time interface checks ────────────────────────────────────────────

var (
	_ port.B2BOrgReader            = (*mock.MockB2BOrgReader)(nil)
	_ port.B2BOrgReader            = (*seededB2BOrgReader)(nil)
	_ port.B2BOrgWriter            = (*mock.MockB2BOrgWriter)(nil)
	_ port.MemberReader            = (*mock.MockMembershipRepository)(nil)
	_ port.ProjectMembershipReader = (*mockProjectMembershipReader)(nil)
	_ port.MemberPublisher         = (*mock.MockMemberPublisher)(nil)
	_ port.B2BOrgSettingsReader    = (*mock.MockB2BOrgSettings)(nil)
	_ port.B2BOrgSettingsWriter    = (*mock.MockB2BOrgSettings)(nil)
	_ usecaseSvc.B2BOrgWriter      = stubB2BOrgWriterUC{}
	_ usecaseSvc.KeyContactWriter  = stubKeyContactWriterUC{}
	_ usecaseSvc.OrgSettingsWriter = stubOrgSettingsWriterUC{}
)
