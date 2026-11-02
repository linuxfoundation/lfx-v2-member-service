// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
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
func (s stubB2BOrgWriterUC) UpdateWithoutPublish(_ context.Context, _ string, _ model.B2BOrgInput, _ string) (*model.B2BOrg, error) {
	return s.org, s.err
}
func (s stubB2BOrgWriterUC) ValidatePrecondition(_ context.Context, _, _ string) (*model.B2BOrg, error) {
	return s.org, s.err
}
func (s stubB2BOrgWriterUC) PublishOrgUpdated(_ context.Context, _, _ *model.B2BOrg) {}

func TestPayloadToB2BOrgInput_PreservesLogoNoOpSemantics(t *testing.T) {
	empty := ""
	input := payloadToB2BOrgInput(&membershipservice.UpdateB2bOrgPayload{LogoURL: &empty})
	assert.Nil(t, input.LogoURL, "an empty public logo_url remains a no-op")

	logoURL := "https://cdn.example.com/logo"
	input = payloadToB2BOrgInput(&membershipservice.UpdateB2bOrgPayload{LogoURL: &logoURL})
	require.NotNil(t, input.LogoURL)
	assert.Equal(t, logoURL, *input.LogoURL)
}

type stubLogoUploaderUC struct {
	org *model.B2BOrg
	err error
}

func (s stubLogoUploaderUC) UploadB2BOrgLogo(_ context.Context, _, _ string, _ io.Reader, _ string) (*model.B2BOrg, error) {
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

func (r *seededB2BOrgReader) FindByNameOrWebsite(_ context.Context, _, _ string) (*model.B2BOrg, bool, error) {
	return nil, false, nil
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
	eligibility     port.B2BOrgEligibilityReader
	pmReader        port.ProjectMembershipReader
	umReader        port.UserMembershipReader
	settingsR       port.B2BOrgSettingsReader
	b2bOrgWriter    usecaseSvc.B2BOrgWriter
	logoUploader    usecaseSvc.LogoUploader
	kcWriter        usecaseSvc.KeyContactWriter
	settingsW       usecaseSvc.OrgSettingsWriter
	workspaceWriter usecaseSvc.WorkspaceWriter
	runner          *usecaseSvc.Runner
	grantIndex      port.KeyContactGrantIndex
}

type svcOpt func(*svcBuilder)

func withStorage(r port.MemberReader) svcOpt {
	return func(b *svcBuilder) { b.storage = r }
}
func withB2BOrgReader(r port.B2BOrgReader) svcOpt {
	return func(b *svcBuilder) { b.b2bOrgReader = r }
}
func withB2BOrgEligibility(r port.B2BOrgEligibilityReader) svcOpt {
	return func(b *svcBuilder) { b.eligibility = r }
}
func withWorkspaceWriterUC(w usecaseSvc.WorkspaceWriter) svcOpt {
	return func(b *svcBuilder) { b.workspaceWriter = w }
}
func withSettingsWriterUC(w usecaseSvc.OrgSettingsWriter) svcOpt {
	return func(b *svcBuilder) { b.settingsW = w }
}
func withB2BOrgWriterUC(w usecaseSvc.B2BOrgWriter) svcOpt {
	return func(b *svcBuilder) { b.b2bOrgWriter = w }
}
func withLogoUploaderUC(u usecaseSvc.LogoUploader) svcOpt {
	return func(b *svcBuilder) { b.logoUploader = u }
}
func withKeyContactWriterUC(w usecaseSvc.KeyContactWriter) svcOpt {
	return func(b *svcBuilder) { b.kcWriter = w }
}
func withPMReader(r port.ProjectMembershipReader) svcOpt {
	return func(b *svcBuilder) { b.pmReader = r }
}
func withUserMembershipReader(r port.UserMembershipReader) svcOpt {
	return func(b *svcBuilder) { b.umReader = r }
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
func withGrantIndex(idx port.KeyContactGrantIndex) svcOpt {
	return func(b *svcBuilder) { b.grantIndex = idx }
}

func newTestSvc(opts ...svcOpt) membershipservice.Service {
	mockRepo := mock.NewMockMembershipRepository()
	b := &svcBuilder{
		auth:         &auth.MockJWTAuth{},
		storage:      mockRepo,
		b2bOrgReader: mock.NewMockB2BOrgReader(),
		pmReader:     mock.NewMockProjectMembershipReader(),
		umReader:     mock.NewMockUserMembershipReader(),
		settingsR:    mock.NewMockB2BOrgSettings(),
		b2bOrgWriter: stubB2BOrgWriterUC{org: sampleB2BOrg},
		logoUploader: stubLogoUploaderUC{org: sampleB2BOrg},
		kcWriter:     stubKeyContactWriterUC{},
		settingsW:    stubOrgSettingsWriterUC{settings: &model.B2BOrgSettings{}},
	}
	for _, o := range opts {
		o(b)
	}
	return NewMembershipService(b.auth, b.storage, b.b2bOrgReader, b.eligibility,
		b.pmReader, b.settingsR, b.b2bOrgWriter, b.logoUploader, b.kcWriter, b.settingsW, b.workspaceWriter, b.runner,
		usecaseSvc.NewMemberTiers(b.storage, b.umReader, b.grantIndex))
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

// ─── UploadB2bOrgLogo handler tests ────────────────────────────────────────────

func TestUploadB2bOrgLogo_Happy(t *testing.T) {
	svc := newTestSvc(withLogoUploaderUC(stubLogoUploaderUC{org: sampleB2BOrg}))

	body := io.NopCloser(strings.NewReader("fake-png-bytes"))
	result, err := svc.UploadB2bOrgLogo(context.Background(), &membershipservice.UploadB2bOrgLogoPayload{
		UID:         "lf-uid-001",
		ContentType: "image/png",
	}, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.B2bOrg)
	assert.Equal(t, "lf-uid-001", *result.B2bOrg.UID)
	assert.NotNil(t, result.LastModified, "Last-Modified must be set")
}

func TestUploadB2bOrgLogo_ValidationError(t *testing.T) {
	svc := newTestSvc(withLogoUploaderUC(stubLogoUploaderUC{err: pkgerrors.NewValidation("unsupported logo content type")}))

	body := io.NopCloser(strings.NewReader("fake-svg-bytes"))
	_, err := svc.UploadB2bOrgLogo(context.Background(), &membershipservice.UploadB2bOrgLogoPayload{
		UID:         "lf-uid-001",
		ContentType: "image/svg+xml",
	}, body)

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "BadRequest", serviceErr.Name)
}

func TestUploadB2bOrgLogo_EtagIgnoresWriterEnrichedIsParent(t *testing.T) {
	// The logo uploader's underlying Update call runs publishEvents, which
	// populates IsParent in place — a derived field the plain reader behind
	// GetB2bOrg/If-Match checks never sets. If the response etag hashed this
	// enriched shape as-is, a parent org's own upload response would carry an
	// etag its next request could never satisfy.
	orgWithChildren := *sampleB2BOrg
	orgWithChildren.IsParent = true
	svc := newTestSvc(withLogoUploaderUC(stubLogoUploaderUC{org: &orgWithChildren}))

	body := io.NopCloser(strings.NewReader("fake-png-bytes"))
	result, err := svc.UploadB2bOrgLogo(context.Background(), &membershipservice.UploadB2bOrgLogoPayload{
		UID:         "lf-uid-001",
		ContentType: "image/png",
	}, body)

	require.NoError(t, err)
	require.NotNil(t, result.Etag)

	unenriched := orgWithChildren
	unenriched.IsParent = false
	wantEtag, etagErr := etag.LFXEtag(&unenriched)
	require.NoError(t, etagErr)
	assert.Equal(t, wantEtag, *result.Etag, "response etag must match the shape GetB2bOrg computes (IsParent unset)")
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

// ─── GetMemberTiers handler tests ─────────────────────────────────────────────

// mapMemberReader resolves memberships from a map; unknown UIDs return
// NotFound, and a non-nil err takes precedence over the map. The embedded
// interface is left nil so any other port.MemberReader method panics if a
// test reaches it unexpectedly.
type mapMemberReader struct {
	port.MemberReader
	memberships map[string]*model.ProjectMembership
	err         error
	// keyContacts, when set, answers ListKeyContactsForMembership per
	// membership UID; contactsErr takes precedence. When both are unset,
	// every membership resolves one active key-contact record for "jdoe",
	// so tier-selection tests pass revalidation without per-test fixtures.
	keyContacts map[string][]*model.KeyContact
	contactsErr error
}

func (m *mapMemberReader) GetMembership(_ context.Context, uid string) (*model.ProjectMembership, error) {
	if m.err != nil {
		return nil, m.err
	}
	if pm, ok := m.memberships[uid]; ok {
		return pm, nil
	}
	return nil, pkgerrors.NewNotFound("membership not found")
}

func (m *mapMemberReader) ListKeyContactsForMembership(_ context.Context, membershipUID string) ([]*model.KeyContact, error) {
	if m.contactsErr != nil {
		return nil, m.contactsErr
	}
	if m.keyContacts != nil {
		return m.keyContacts[membershipUID], nil
	}
	return []*model.KeyContact{
		{UID: "kc-" + membershipUID, MembershipUID: membershipUID, Username: "jdoe", Status: "Active"},
	}, nil
}

// failingUserMembershipReader always fails, simulating an fga-sync RPC outage.
type failingUserMembershipReader struct{}

func (failingUserMembershipReader) MembershipUIDsForUser(context.Context, string) ([]string, error) {
	return nil, pkgerrors.NewServiceUnavailable("fga-sync read_tuples RPC failed")
}

func TestGetMemberTiers_BlankUsername(t *testing.T) {
	svc := newTestSvc()

	_, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "   "})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "BadRequest", serviceErr.Name)
}

func TestGetMemberTiers_UnknownUserEmpty(t *testing.T) {
	svc := newTestSvc()

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "nobody"})

	require.NoError(t, err)
	assert.Empty(t, res, "unknown users must yield an empty list, not an error")
}

func TestGetMemberTiers_Happy(t *testing.T) {
	svc := newTestSvc()

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "keycontact1"})

	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "org-1", res[0].B2bOrgUID)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", res[0].MembershipUID)
	assert.Equal(t, model.TierClassGold, res[0].Tier)
	require.NotNil(t, res[0].TierName)
	assert.Equal(t, "Gold Membership", *res[0].TierName)
}

func TestGetMemberTiers_DanglingTupleSkipped(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("keycontact1", []string{"11111111-1111-1111-1111-111111111111", "missing-uid"})
	svc := newTestSvc(withUserMembershipReader(umr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "keycontact1"})

	require.NoError(t, err)
	require.Len(t, res, 1, "the dangling tuple must be skipped, not fail the lookup")
	assert.Equal(t, "org-1", res[0].B2bOrgUID)
}

func TestGetMemberTiers_HighestTierPerOrg(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-silver", "m-platinum", "m-gold", "m-expired", "m-ended"})
	mr := &mapMemberReader{memberships: map[string]*model.ProjectMembership{
		// org-a holds silver and platinum: platinum must win.
		"m-silver":   {UID: "m-silver", B2BOrgUID: "org-a", CompanyName: "Alpha Corp", TierName: "Silver Membership", Status: "Active"},
		"m-platinum": {UID: "m-platinum", B2BOrgUID: "org-a", CompanyName: "Alpha Corp", TierName: "Platinum Membership", Status: "Active"},
		// org-b holds one active gold plus records that must be filtered out.
		"m-gold":    {UID: "m-gold", B2BOrgUID: "org-b", CompanyName: "Beta Corp", TierName: "Gold Corporate Membership", Status: "Active"},
		"m-expired": {UID: "m-expired", B2BOrgUID: "org-b", CompanyName: "Beta Corp", TierName: "Platinum Membership", Status: "Expired"},
		"m-ended":   {UID: "m-ended", B2BOrgUID: "org-b", CompanyName: "Beta Corp", TierName: "Platinum Membership", Status: "Active", EndDate: "2020-01-01"},
	}}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	require.Len(t, res, 2)
	// Ordered highest tier first: org-a's platinum outranks org-b's gold.
	assert.Equal(t, "org-a", res[0].B2bOrgUID)
	assert.Equal(t, model.TierClassPlatinum, res[0].Tier)
	assert.Equal(t, "m-platinum", res[0].MembershipUID)
	assert.Equal(t, "org-b", res[1].B2bOrgUID)
	assert.Equal(t, model.TierClassGold, res[1].Tier)
	assert.Equal(t, "m-gold", res[1].MembershipUID)
}

// Entries are ordered by tier rank descending, independent of company name, so
// a caller can read the user's highest tier across all their organizations from
// the leading entry. Organizations sharing the top tier fall back to company
// name. Here the higher tier sits on the alphabetically-later company, proving
// rank drives the order rather than name.
func TestGetMemberTiers_OrdersByHighestTierFirst(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-silver", "m-plat-z", "m-plat-m"})
	mr := &mapMemberReader{memberships: map[string]*model.ProjectMembership{
		// A lower tier on an alphabetically-earlier company still ranks last.
		"m-silver": {UID: "m-silver", B2BOrgUID: "org-silver", CompanyName: "Aardvark Corp", TierName: "Silver Membership", Status: "Active"},
		// Two orgs share the top tier; they order by company name.
		"m-plat-z": {UID: "m-plat-z", B2BOrgUID: "org-z", CompanyName: "Zebra Corp", TierName: "Platinum Membership", Status: "Active"},
		"m-plat-m": {UID: "m-plat-m", B2BOrgUID: "org-m", CompanyName: "Meerkat Corp", TierName: "Platinum Membership", Status: "Active"},
	}}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	require.Len(t, res, 3)
	assert.Equal(t, model.TierClassPlatinum, res[0].Tier)
	require.NotNil(t, res[0].CompanyName)
	assert.Equal(t, "Meerkat Corp", *res[0].CompanyName, "organizations at the same tier order by company name")
	assert.Equal(t, model.TierClassPlatinum, res[1].Tier)
	require.NotNil(t, res[1].CompanyName)
	assert.Equal(t, "Zebra Corp", *res[1].CompanyName)
	assert.Equal(t, model.TierClassSilver, res[2].Tier, "the lower tier ranks last despite its earlier company name")
	assert.Equal(t, "org-silver", res[2].B2bOrgUID)
}

// The response's tier field is the normalized class, kept alongside the raw
// product name, and per-org selection follows the full Org Lens rank order
// synced from lf-dbt, not just platinum/gold/silver. Multi-word classes are
// pinned to their snake_case wire value.
func TestGetMemberTiers_TierNormalization(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-premier", "m-gold", "m-enduser"})
	mr := &mapMemberReader{memberships: map[string]*model.ProjectMembership{
		// org-a holds gold and premier: premier outranks gold in the Org
		// Lens taxonomy (the old 4-class table wrongly demoted it to other).
		"m-premier": {UID: "m-premier", B2BOrgUID: "org-a", CompanyName: "Alpha Corp", TierName: "Premier Membership", Status: "Active"},
		"m-gold":    {UID: "m-gold", B2BOrgUID: "org-a", CompanyName: "Alpha Corp", TierName: "Gold Corporate Membership", Status: "Active"},
		"m-enduser": {UID: "m-enduser", B2BOrgUID: "org-b", CompanyName: "Beta Corp", TierName: "End User Supporter", Status: "Active"},
	}}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, model.TierClassPremier, res[0].Tier)
	assert.Equal(t, "m-premier", res[0].MembershipUID)
	require.NotNil(t, res[0].TierName)
	assert.Equal(t, "Premier Membership", *res[0].TierName, "tier_name stays the raw product text")
	assert.Equal(t, "end_user", res[1].Tier, "multi-word classes use the snake_case wire value")
}

func TestGetMemberTiers_ReverseIndexUnavailable(t *testing.T) {
	svc := newTestSvc(withUserMembershipReader(failingUserMembershipReader{}))

	_, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "ServiceUnavailable", serviceErr.Name)
}

// Only NotFound marks a dangling tuple that may be skipped; any other
// membership read failure (e.g. a Salesforce outage) must fail the whole
// lookup rather than silently shrink the result to a subset of the user's
// organizations. The Salesforce-backed reader reports outages as untyped
// wrapped errors, so those must surface as ServiceUnavailable too, not as a
// generic internal error.
func TestGetMemberTiers_MembershipReadFailureFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "typed ServiceUnavailable passes through",
			err:  pkgerrors.NewServiceUnavailable("salesforce unavailable"),
		},
		{
			name: "untyped error is coerced to ServiceUnavailable",
			err:  fmt.Errorf("getting membership record: %w", errors.New("dial tcp: i/o timeout")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			umr := mock.NewMockUserMembershipReader()
			umr.SetUserMemberships("jdoe", []string{"m-1"})
			svc := newTestSvc(withUserMembershipReader(umr), withStorage(&mapMemberReader{err: tt.err}))

			_, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

			require.Error(t, err)
			var serviceErr *goa.ServiceError
			require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
			assert.Equal(t, "ServiceUnavailable", serviceErr.Name)
		})
	}
}

// countingMemberReader wraps a MemberReader and counts GetMembership calls per UID.
type countingMemberReader struct {
	port.MemberReader
	calls map[string]int
}

func (c *countingMemberReader) GetMembership(ctx context.Context, uid string) (*model.ProjectMembership, error) {
	c.calls[uid]++
	return c.MemberReader.GetMembership(ctx, uid)
}

// A cache-missing membership read is a Salesforce round-trip, so duplicate
// tuples in the reverse index must be deduplicated before the reads, not
// merely deduplicated in the response.
func TestGetMemberTiers_DuplicateTuplesReadOnce(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-1", "m-1", "m-1"})
	mr := &countingMemberReader{
		MemberReader: &mapMemberReader{memberships: map[string]*model.ProjectMembership{
			"m-1": {UID: "m-1", B2BOrgUID: "org-1", TierName: "Gold Membership", Status: "Active"},
		}},
		calls: map[string]int{},
	}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, 1, mr.calls["m-1"], "duplicate reverse-index tuples must not multiply membership reads")
}

// A reverse index larger than any real key contact would fan out into that many
// cache-cold Salesforce reads. GetMemberTiers must refuse it (fail closed)
// before issuing a single membership read, not truncate to a wrong top tier.
func TestGetMemberTiers_CandidateCapFailsClosed(t *testing.T) {
	uids := make([]string, usecaseSvc.MaxMemberTierCandidates+1)
	for i := range uids {
		uids[i] = fmt.Sprintf("m-%d", i)
	}
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", uids)
	mr := &countingMemberReader{
		MemberReader: &mapMemberReader{memberships: map[string]*model.ProjectMembership{}},
		calls:        map[string]int{},
	}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	_, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "ServiceUnavailable", serviceErr.Name)
	assert.Empty(t, mr.calls, "candidate cap must refuse before any membership read")
}

// A candidate set exactly at the cap is within bounds and resolves normally;
// the guard trips only above it.
func TestGetMemberTiers_CandidateCapBoundaryResolves(t *testing.T) {
	uids := make([]string, usecaseSvc.MaxMemberTierCandidates)
	memberships := make(map[string]*model.ProjectMembership, len(uids))
	for i := range uids {
		id := fmt.Sprintf("m-%d", i)
		uids[i] = id
		memberships[id] = &model.ProjectMembership{UID: id, B2BOrgUID: "org-" + id, TierName: "Gold Membership", Status: "Active"}
	}
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", uids)
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(&mapMemberReader{memberships: memberships}))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	assert.Len(t, res, usecaseSvc.MaxMemberTierCandidates)
}

// The cap must apply to the post-dedup candidate count, not the raw tuple
// count: a pre-dedup count over the cap that collapses to exactly the cap
// after dedup must still resolve, not fail closed.
func TestGetMemberTiers_CandidateCapAppliesPostDedup(t *testing.T) {
	uids := make([]string, usecaseSvc.MaxMemberTierCandidates)
	memberships := make(map[string]*model.ProjectMembership, len(uids))
	for i := range uids {
		id := fmt.Sprintf("m-%d", i)
		uids[i] = id
		memberships[id] = &model.ProjectMembership{UID: id, B2BOrgUID: "org-" + id, TierName: "Gold Membership", Status: "Active"}
	}
	// Duplicate the first few UIDs so the raw tuple count exceeds the cap
	// while the unique count stays exactly at it.
	uids = append(uids, uids[0], uids[1], uids[2])
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", uids)
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(&mapMemberReader{memberships: memberships}))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	assert.Len(t, res, usecaseSvc.MaxMemberTierCandidates)
}

// A membership record without a b2b_org_uid cannot be attributed to an
// organization; it must be skipped rather than fail the lookup or surface as
// a malformed entry (b2b_org_uid is required in the response contract).
func TestGetMemberTiers_SkipsMembershipWithoutOrgUID(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-orgless"})
	mr := &mapMemberReader{memberships: map[string]*model.ProjectMembership{
		"m-orgless": {UID: "m-orgless", TierName: "Gold Membership", Status: "Active"},
	}}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	assert.Empty(t, res)
}

// A reverse-index tuple is only evidence that a key_contact grant was once
// published. The authoritative Project_Role__c record can go inactive or be
// reassigned to another user while the tuple lingers, so a candidate whose
// records no longer name the user as an active contact is omitted.
func TestGetMemberTiers_StaleGrantSkipped(t *testing.T) {
	tests := []struct {
		name     string
		contacts []*model.KeyContact
	}{
		{
			name: "contact went inactive",
			contacts: []*model.KeyContact{
				{UID: "kc-1", MembershipUID: "m-1", Username: "jdoe", Status: "Inactive"},
			},
		},
		{
			name: "contact reassigned to another user",
			contacts: []*model.KeyContact{
				{UID: "kc-1", MembershipUID: "m-1", Username: "someoneelse", Status: "Active"},
			},
		},
		{
			name:     "no contact records remain",
			contacts: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			umr := mock.NewMockUserMembershipReader()
			umr.SetUserMemberships("jdoe", []string{"m-1"})
			mr := &mapMemberReader{
				memberships: map[string]*model.ProjectMembership{
					"m-1": {UID: "m-1", B2BOrgUID: "org-1", TierName: "Gold Membership", Status: "Active"},
				},
				keyContacts: map[string][]*model.KeyContact{"m-1": tt.contacts},
			}
			svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

			res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

			require.NoError(t, err)
			assert.Empty(t, res, "a lingering FGA tuple must not report an organization the user is no longer an active key contact of")
		})
	}
}

// A failed key-contact read cannot distinguish a stale grant from a live one;
// omitting the organization would under-report and returning it unverified
// would over-report, so the lookup fails closed as ServiceUnavailable.
func TestGetMemberTiers_KeyContactReadFailureFailsClosed(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-1"})
	mr := &mapMemberReader{
		memberships: map[string]*model.ProjectMembership{
			"m-1": {UID: "m-1", B2BOrgUID: "org-1", TierName: "Gold Membership", Status: "Active"},
		},
		contactsErr: errors.New("dial tcp: i/o timeout"),
	}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	_, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "ServiceUnavailable", serviceErr.Name)
}

// Production contact records carry no Username (the SOQL projection resolves
// emails only; usernames are resolved on the write path), so revalidation
// must match them through the key-contact grant index rather than the
// never-populated Username field.
func TestGetMemberTiers_ProductionContactMatchesViaGrantIndex(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-1"})
	mr := &mapMemberReader{
		memberships: map[string]*model.ProjectMembership{
			"m-1": {UID: "m-1", B2BOrgUID: "org-1", TierName: "Gold Membership", Status: "Active"},
		},
		keyContacts: map[string][]*model.KeyContact{
			"m-1": {{UID: "kc-1", MembershipUID: "m-1", Status: "Active", Email: "jdoe@example.org"}},
		},
	}
	idx := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		"kc-1": {MembershipUID: "m-1", Username: "jdoe"},
	}}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr), withGrantIndex(idx))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	require.Len(t, res, 1, "a production-shaped contact (blank Username) must match via the grant index")
	assert.Equal(t, "org-1", res[0].B2bOrgUID)
}

func TestGetMemberTiers_ProductionContactGrantMismatchSkipped(t *testing.T) {
	tests := []struct {
		name string
		idx  port.KeyContactGrantIndex
	}{
		{
			name: "grant belongs to another user",
			idx: &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
				"kc-1": {MembershipUID: "m-1", Username: "someoneelse"},
			}},
		},
		{
			name: "grant reparented to another membership",
			idx: &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
				"kc-1": {MembershipUID: "m-2", Username: "jdoe"},
			}},
		},
		{
			name: "no grant entry for the contact",
			idx:  &mock.MockKeyContactGrantIndex{},
		},
		{
			name: "grant index not wired",
			idx:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			umr := mock.NewMockUserMembershipReader()
			umr.SetUserMemberships("jdoe", []string{"m-1"})
			mr := &mapMemberReader{
				memberships: map[string]*model.ProjectMembership{
					"m-1": {UID: "m-1", B2BOrgUID: "org-1", TierName: "Gold Membership", Status: "Active"},
				},
				keyContacts: map[string][]*model.KeyContact{
					"m-1": {{UID: "kc-1", MembershipUID: "m-1", Status: "Active", Email: "jdoe@example.org"}},
				},
			}
			svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr), withGrantIndex(tt.idx))

			res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

			require.NoError(t, err)
			assert.Empty(t, res, "a blank-Username contact must not count unless the grant index maps it to this user and membership")
		})
	}
}

// An index read failure cannot distinguish a matching grant from a stale one,
// so the lookup fails closed as ServiceUnavailable, mirroring the key-contact
// read failure above.
func TestGetMemberTiers_GrantIndexReadFailureFailsClosed(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-1"})
	mr := &mapMemberReader{
		memberships: map[string]*model.ProjectMembership{
			"m-1": {UID: "m-1", B2BOrgUID: "org-1", TierName: "Gold Membership", Status: "Active"},
		},
		keyContacts: map[string][]*model.KeyContact{
			"m-1": {{UID: "kc-1", MembershipUID: "m-1", Status: "Active", Email: "jdoe@example.org"}},
		},
	}
	idx := &mock.MockKeyContactGrantIndex{GetErr: errors.New("dial tcp: i/o timeout")}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr), withGrantIndex(idx))

	_, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.Error(t, err)
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, "ServiceUnavailable", serviceErr.Name)
}

// The response order is part of the public contract's determinism: at an equal
// tier, when company names are absent (or equal), entries must still sort
// stably, falling back to b2b_org_uid.
func TestGetMemberTiers_SortFallsBackToOrgUID(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-2", "m-1"})
	mr := &mapMemberReader{memberships: map[string]*model.ProjectMembership{
		"m-2": {UID: "m-2", B2BOrgUID: "org-b", TierName: "Gold Membership", Status: "Active"},
		"m-1": {UID: "m-1", B2BOrgUID: "org-a", TierName: "Gold Membership", Status: "Active"},
	}}
	svc := newTestSvc(withUserMembershipReader(umr), withStorage(mr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "org-a", res[0].B2bOrgUID)
	assert.Equal(t, "org-b", res[1].B2bOrgUID)
}

// The public payload omits absent optional fields entirely rather than sending
// empty strings, and always carries the normalized tier alongside the raw name.
func TestMemberOrgTierToResponse(t *testing.T) {
	t.Run("minimal membership sets only required fields", func(t *testing.T) {
		resp := memberOrgTierToResponse(&model.ProjectMembership{UID: "m-1", B2BOrgUID: "org-1"})

		assert.Equal(t, "org-1", resp.B2bOrgUID)
		assert.Equal(t, "m-1", resp.MembershipUID)
		assert.Equal(t, model.TierClassOther, resp.Tier, "an absent tier name normalizes to %q", model.TierClassOther)
		assert.Nil(t, resp.CompanyName)
		assert.Nil(t, resp.ProjectUID)
		assert.Nil(t, resp.ProjectSlug)
		assert.Nil(t, resp.TierUID)
		assert.Nil(t, resp.TierName)
		assert.Nil(t, resp.Status)
		assert.Nil(t, resp.StartDate)
		assert.Nil(t, resp.EndDate)
	})

	t.Run("full membership maps every field", func(t *testing.T) {
		resp := memberOrgTierToResponse(&model.ProjectMembership{
			UID:         "m-1",
			B2BOrgUID:   "org-1",
			CompanyName: "Acme Corp",
			ProjectUID:  "project-1",
			ProjectSlug: "linux-foundation",
			TierUID:     "tier-1",
			TierName:    "Gold Corporate Membership",
			Status:      "Active",
			StartDate:   "2025-01-01",
			EndDate:     "2099-12-31",
		})

		assert.Equal(t, "org-1", resp.B2bOrgUID)
		assert.Equal(t, "m-1", resp.MembershipUID)
		assert.Equal(t, model.TierClassGold, resp.Tier)
		require.NotNil(t, resp.CompanyName)
		assert.Equal(t, "Acme Corp", *resp.CompanyName)
		require.NotNil(t, resp.ProjectUID)
		assert.Equal(t, "project-1", *resp.ProjectUID)
		require.NotNil(t, resp.ProjectSlug)
		assert.Equal(t, "linux-foundation", *resp.ProjectSlug)
		require.NotNil(t, resp.TierUID)
		assert.Equal(t, "tier-1", *resp.TierUID)
		require.NotNil(t, resp.TierName)
		assert.Equal(t, "Gold Corporate Membership", *resp.TierName)
		require.NotNil(t, resp.Status)
		assert.Equal(t, "Active", *resp.Status)
		require.NotNil(t, resp.StartDate)
		assert.Equal(t, "2025-01-01", *resp.StartDate)
		require.NotNil(t, resp.EndDate)
		assert.Equal(t, "2099-12-31", *resp.EndDate)
	})
}

// ─── ListProjectMemberships handler tests ─────────────────────────────────────

// stubMemberReaderListErr wraps MockControllableMemberReader to inject an
// error from ListMembershipsForProject specifically.
type stubMemberReaderListErr struct {
	*mock.MockControllableMemberReader
	err error
}

func (s *stubMemberReaderListErr) ListMembershipsForProject(_ context.Context, _ string, _ model.MembershipFilters, _ int) (model.MembershipPage, error) {
	return model.MembershipPage{}, s.err
}

// stubListPage wraps MockControllableMemberReader to return a caller-supplied
// MembershipPage from ListMembershipsForProject, capturing the filters it was
// invoked with so tests can assert request-side pass-through.
type stubListPage struct {
	*mock.MockControllableMemberReader
	page             model.MembershipPage
	receivedFilter   model.MembershipFilters
	receivedPageSize int
}

func (s *stubListPage) ListMembershipsForProject(_ context.Context, _ string, filters model.MembershipFilters, pageSize int) (model.MembershipPage, error) {
	s.receivedFilter = filters
	s.receivedPageSize = pageSize
	return s.page, nil
}

// stubMultiPage wraps MockControllableMemberReader to return a scripted
// sequence of MembershipPage values from successive ListMembershipsForProject
// calls (sticking on the last page once exhausted), so tests can exercise the
// b2b_org_uid multi-page walk in ListProjectMemberships.
type stubMultiPage struct {
	*mock.MockControllableMemberReader
	pages []model.MembershipPage
	calls int // index into pages, sticks at len(pages)-1 once exhausted
	// totalCalls counts every invocation, including repeats of the last
	// page once calls has stuck — calls alone cannot distinguish "walked
	// once" from "walked repeatedly and stopped advancing".
	totalCalls int
}

func (s *stubMultiPage) ListMembershipsForProject(_ context.Context, _ string, _ model.MembershipFilters, _ int) (model.MembershipPage, error) {
	s.totalCalls++
	page := s.pages[s.calls]
	if s.calls < len(s.pages)-1 {
		s.calls++
	}
	return page, nil
}

// stubUnboundedPages always reports a fresh membership and a new, never-repeating
// NextPageToken — simulating upstream results that genuinely never exhaust, as
// opposed to stubMultiPage's scripted scenario where a token repeats verbatim.
type stubUnboundedPages struct {
	*mock.MockControllableMemberReader
	orgUID string
	calls  int
}

func (s *stubUnboundedPages) ListMembershipsForProject(_ context.Context, _ string, _ model.MembershipFilters, _ int) (model.MembershipPage, error) {
	s.calls++
	return model.MembershipPage{
		Memberships:   []*model.ProjectMembership{{UID: fmt.Sprintf("m-looping-%d", s.calls), ProjectUID: "project-uid-1", B2BOrgUID: s.orgUID}},
		NextPageToken: fmt.Sprintf("always-more-%d", s.calls),
	}, nil
}

// stubExpiringCursor returns the given first page, then a page_token expired
// validation error on every subsequent call — simulating MemberReader's own
// internal continuation cursor landing on a batch the cache evicted mid-walk
// (see member_reader.go's "cached page sequence is no longer available"
// guard), which is the genuinely-reachable failure mode on this path (as
// opposed to stubMultiPage's scripted verbatim-repeat scenario above).
type stubExpiringCursor struct {
	*mock.MockControllableMemberReader
	firstPage  model.MembershipPage
	calls      int
	totalCalls int
}

func (s *stubExpiringCursor) ListMembershipsForProject(_ context.Context, _ string, _ model.MembershipFilters, _ int) (model.MembershipPage, error) {
	s.totalCalls++
	s.calls++
	if s.calls == 1 {
		return s.firstPage, nil
	}
	return model.MembershipPage{}, pkgerrors.NewValidation(
		"page_token expired", fmt.Errorf("cached page sequence is no longer available; restart pagination without a page_token"))
}

func stringPtr(s string) *string { return &s }

// TestLogicalPageSizeInvariants pins the invariants the pageSize comment in
// ListProjectMemberships documents but cannot enforce at compile time:
// fullBatchLogicalPageSize must equal salesforce.SFQueryBatchSize exactly
// (not merely be *a* valid page size — ValidPageSizes membership alone would
// not catch, say, fullBatchLogicalPageSize=500 or SFQueryBatchSize changing
// to 2000 while this constant stayed at 1000) and defaultLogicalPageSize
// must be a valid page size that divides evenly into it, so the unfiltered
// path's intra-batch cursor math in sliceCachedBatch never leaves a
// remainder. The ValidPageSizes membership check on fullBatchLogicalPageSize
// guards the other drift direction: salesforce.NormalizePageSize (called by
// MemberReader.ListMembershipsForProject) silently caps any requested size
// above 1000, so if SFQueryBatchSize ever moved off the accepted menu the
// equality check alone would force fullBatchLogicalPageSize to follow it to
// a value NormalizePageSize then quietly clamps, breaking the "consume one
// whole cached batch per iteration" invariant without either constant
// looking wrong in isolation.
// If any of these drift, this test fails instead of silently breaking
// pagination.
func TestLogicalPageSizeInvariants(t *testing.T) {
	assert.Equal(t, salesforce.SFQueryBatchSize, fullBatchLogicalPageSize,
		"fullBatchLogicalPageSize must equal salesforce.SFQueryBatchSize exactly")
	assert.True(t, salesforce.ValidPageSizes[fullBatchLogicalPageSize],
		"fullBatchLogicalPageSize (%d) must be a Salesforce-accepted page size", fullBatchLogicalPageSize)
	assert.True(t, salesforce.ValidPageSizes[defaultLogicalPageSize],
		"defaultLogicalPageSize (%d) must be a Salesforce-accepted page size", defaultLogicalPageSize)
	assert.Equal(t, 0, fullBatchLogicalPageSize%defaultLogicalPageSize,
		"fullBatchLogicalPageSize (%d) must divide evenly by defaultLogicalPageSize (%d)",
		fullBatchLogicalPageSize, defaultLogicalPageSize)
}

func TestListProjectMemberships(t *testing.T) {
	// newService returns both the service under test and the stub it was
	// built with (nil when the case doesn't need one), so check closures
	// read case-local state rather than a variable shared across subtests —
	// safe even if a future edit adds t.Parallel() to the loop below.
	tests := []struct {
		name        string
		newService  func() (membershipservice.Service, any)
		payload     *membershipservice.ListProjectMembershipsPayload
		ctx         func() context.Context // nil defaults to context.Background()
		wantErrName string
		wantErrIs   error // checked via errors.Is when the error isn't a *goa.ServiceError
		check       func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any)
	}{
		{
			name:       "project_uid only returns the default mock membership",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload:    &membershipservice.ListProjectMembershipsPayload{ProjectUID: "project-uid-1"},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				require.Len(t, result.ProjectMemberships, 1)
				assert.Equal(t, "11111111-1111-1111-1111-111111111111", *result.ProjectMemberships[0].UID)
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 1, *result.TotalSize)
				assert.False(t, result.ResultsTruncated, "total_size matches the returned count, so no truncation")
			},
		},
		{
			name:       "unknown project_uid returns an empty list from the mock",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload:    &membershipservice.ListProjectMembershipsPayload{ProjectUID: "nonexistent-project-uid"},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				assert.Empty(t, result.ProjectMemberships)
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 0, *result.TotalSize)
			},
		},
		{
			name: "tier_uid match passes the normalized SFID through to the reader",
			newService: func() (membershipservice.Service, any) {
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships: []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1"}},
						TotalSize:   1,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				TierUID:    stringPtr("01t000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any) {
				require.Len(t, result.ProjectMemberships, 1)
				reader, ok := stub.(*stubListPage)
				require.True(t, ok)
				assert.Equal(t, "01t000000000001AAA", reader.receivedFilter.TierUID)
			},
		},
		{
			name: "tier_uid no match returns an empty list from the reader",
			newService: func() (membershipservice.Service, any) {
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page:                         model.MembershipPage{},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				TierUID:    stringPtr("01t000000000999AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any) {
				assert.Empty(t, result.ProjectMemberships)
				reader, ok := stub.(*stubListPage)
				require.True(t, ok)
				assert.Equal(t, "01t000000000999AAA", reader.receivedFilter.TierUID)
			},
		},
		{
			// Regression test for the cold-cache first-read truncation bug:
			// the unfiltered/tier_uid-only path must request a logical page
			// size (200) smaller than the underlying Salesforce batch size
			// (1000), so sliceCachedBatch can mint an intra-batch cursor
			// without depending on the background batch-link sweep.
			name: "unfiltered path requests a 200 page size, not the full Salesforce batch size",
			newService: func() (membershipservice.Service, any) {
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships: []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1"}},
						TotalSize:   1,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{ProjectUID: "project-uid-1"},
			check: func(t *testing.T, _ *membershipservice.ListProjectMembershipsResult, stub any) {
				reader, ok := stub.(*stubListPage)
				require.True(t, ok)
				assert.Equal(t, 200, reader.receivedPageSize,
					"unfiltered path must request less than the 1000-record Salesforce batch size "+
						"so a first page can mint an intra-batch cursor without the background sweep")
			},
		},
		{
			// b2b_org_uid is not client-page-size-controlled: the walk
			// re-reads the same cached batch on every internal iteration, so
			// it requests the full batch size to minimize iterations within
			// the maxB2BOrgFilterPages cap.
			name: "b2b_org_uid path requests the full 1000 Salesforce batch size",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships: []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
						TotalSize:   1,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, _ *membershipservice.ListProjectMembershipsResult, stub any) {
				reader, ok := stub.(*stubListPage)
				require.True(t, ok)
				assert.Equal(t, 1000, reader.receivedPageSize)
			},
		},
		{
			name:       "b2b_org_uid no match drops the non-matching sample membership",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"), // sample membership has no B2BOrgUID set
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				assert.Empty(t, result.ProjectMemberships)
			},
		},
		{
			name: "b2b_org_uid match filters to the matching membership",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships: []*model.ProjectMembership{
							{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID, CompanyName: "Acme"},
							{UID: "m-2", ProjectUID: "project-uid-1", B2BOrgUID: "001000000000002AAA", CompanyName: "Other"},
						},
						TotalSize: 2,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				require.Len(t, result.ProjectMemberships, 1)
				assert.Equal(t, "m-1", *result.ProjectMemberships[0].UID)
				// TotalSize reflects the filtered result, not the unfiltered page size.
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 1, *result.TotalSize)
				assert.False(t, result.ResultsTruncated)
			},
		},
		{
			name: "b2b_org_uid match spread over multiple pages accumulates and reports no next_page_token",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubMultiPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					pages: []model.MembershipPage{
						{
							Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
							NextPageToken: "cursor-page-2",
						},
						{
							Memberships:   []*model.ProjectMembership{{UID: "m-2", ProjectUID: "project-uid-1", B2BOrgUID: "001000000000002AAA"}},
							NextPageToken: "cursor-page-3",
						},
						{
							Memberships:   []*model.ProjectMembership{{UID: "m-3", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
							NextPageToken: "",
						},
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any) {
				require.Len(t, result.ProjectMemberships, 2)
				assert.Equal(t, "m-1", *result.ProjectMemberships[0].UID)
				assert.Equal(t, "m-3", *result.ProjectMemberships[1].UID)
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 2, *result.TotalSize)
				reader, ok := stub.(*stubMultiPage)
				require.True(t, ok)
				assert.Equal(t, 3, reader.totalCalls, "should have walked all 3 scripted pages")
				assert.Nil(t, result.NextPageToken, "the b2b_org_uid-filtered path never leaks a continuation token")
				assert.False(t, result.ResultsTruncated, "the walk exhausted all upstream pages normally")
			},
		},
		{
			name: "b2b_org_uid match that never exhausts stops at the page cap",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubUnboundedPages{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					orgUID:                       orgUID,
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				require.Len(t, result.ProjectMemberships, maxB2BOrgFilterPages)
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, maxB2BOrgFilterPages, *result.TotalSize)
				assert.Nil(t, result.NextPageToken)
				assert.True(t, result.ResultsTruncated, "the walk hit the page cap before exhausting results")
			},
		},
		{
			name: "b2b_org_uid match where the upstream cursor repeats verbatim mid-walk",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubMultiPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					pages: []model.MembershipPage{
						{
							Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
							NextPageToken: "cursor-page-2",
						},
						{
							// Simulates a pathological/overlapping upstream response
							// that repeats the exact same page token and record
							// rather than advancing — not an evicted-cache reset,
							// which now surfaces as a page_token expired validation
							// error (see the dedicated test case below) rather than
							// a silent restart.
							Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
							NextPageToken: "cursor-page-2",
						},
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any) {
				require.Len(t, result.ProjectMemberships, 1, "the repeated record must be deduped, not appended again")
				assert.Equal(t, "m-1", *result.ProjectMemberships[0].UID)
				reader, ok := stub.(*stubMultiPage)
				require.True(t, ok)
				assert.Equal(t, 2, reader.totalCalls, "should stop walking as soon as the repeated token is detected")
				assert.Nil(t, result.NextPageToken)
				assert.True(t, result.ResultsTruncated, "the upstream cursor repeated mid-walk")
			},
		},
		{
			name: "b2b_org_uid walk degrades gracefully when the internal continuation cursor expires mid-walk",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubExpiringCursor{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					firstPage: model.MembershipPage{
						Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
						NextPageToken: "cursor-page-2",
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any) {
				require.Len(t, result.ProjectMemberships, 1, "the page fetched before the cursor expired is kept")
				assert.Equal(t, "m-1", *result.ProjectMemberships[0].UID)
				reader, ok := stub.(*stubExpiringCursor)
				require.True(t, ok)
				assert.Equal(t, 2, reader.totalCalls, "stops after the expired-cursor error on the second call rather than erroring the whole request")
				assert.Nil(t, result.NextPageToken)
				assert.True(t, result.ResultsTruncated, "the internal continuation cursor expired before the walk reached the true end")
			},
		},
		{
			name: "b2b_org_uid walk ending on a cold-cache first page with no next_page_token reports results_truncated",
			newService: func() (membershipservice.Service, any) {
				orgUID := "001000000000001AAA"
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1", B2BOrgUID: orgUID}},
						NextPageToken: "",
						// Signature of the same race the unfiltered path
						// guards against: more records exist upstream (per
						// TotalSize) than this first page returned, with no
						// cursor to resume from.
						TotalSize: 5,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				require.Len(t, result.ProjectMemberships, 1)
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 1, *result.TotalSize, "total_size reflects only what was actually matched/returned, not the race's upstream total")
				assert.Nil(t, result.NextPageToken)
				assert.True(t, result.ResultsTruncated,
					"a cold-cache first page ending the b2b_org_uid walk with total_size exceeding the returned "+
						"record count is the same background-sweep race signature the unfiltered path detects")
			},
		},
		{
			name:       "deadline exceeded maps to ServiceUnavailable",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload:    &membershipservice.ListProjectMembershipsPayload{ProjectUID: "project-uid-1"},
			ctx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			wantErrName: "ServiceUnavailable",
		},
		{
			// A client disconnect mid-walk is routine, not a service failure,
			// but it must still be a declared/encoded error (not a raw error
			// value), so it maps to the same ServiceUnavailable name as
			// DeadlineExceeded while remaining distinguishable via
			// errors.Is(err, context.Canceled).
			name:       "cancelled context maps to ServiceUnavailable and unwraps to context.Canceled",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload:    &membershipservice.ListProjectMembershipsPayload{ProjectUID: "project-uid-1"},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErrName: "ServiceUnavailable",
			wantErrIs:   context.Canceled,
		},
		{
			name:       "invalid b2b_org_uid returns BadRequest",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("not-a-valid-sfid"),
			},
			wantErrName: "BadRequest",
		},
		{
			name:       "invalid tier_uid returns BadRequest",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				TierUID:    stringPtr("not-a-valid-sfid"),
			},
			wantErrName: "BadRequest",
		},
		{
			name:       "page_token combined with b2b_org_uid returns BadRequest",
			newService: func() (membershipservice.Service, any) { return newTestSvc(), nil },
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				B2bOrgUID:  stringPtr("001000000000001AAA"),
				PageToken:  stringPtr("some-cursor"),
			},
			wantErrName: "BadRequest",
		},
		{
			name: "resolver NotFound maps to NotFound",
			newService: func() (membershipservice.Service, any) {
				// Mirrors member_reader.go's actual wrapping of a resolver
				// error: fmt.Errorf("...%w", err) around the domain error,
				// not a bare pkgerrors.NotFound. wrapError must use
				// errors.As to see through this wrapping.
				notFound := pkgerrors.NewNotFound("project not found", fmt.Errorf("resolver error"))
				reader := &stubMemberReaderListErr{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					err:                          fmt.Errorf("resolving project SFID for UID %s: %w", "nonexistent-project-uid", notFound),
				}
				svc := newTestSvc(withStorage(reader))
				return svc, nil
			},
			payload:     &membershipservice.ListProjectMembershipsPayload{ProjectUID: "nonexistent-project-uid"},
			wantErrName: "NotFound",
		},
		{
			name: "page_token passes through to MembershipFilters and next_page_token is surfaced",
			newService: func() (membershipservice.Service, any) {
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1"}},
						NextPageToken: "opaque-cursor-abc",
						TotalSize:     1,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				PageToken:  stringPtr("prior-cursor-xyz"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, stub any) {
				reader, ok := stub.(*stubListPage)
				require.True(t, ok)
				assert.Equal(t, "prior-cursor-xyz", reader.receivedFilter.PageToken,
					"inbound page_token should pass through to MembershipFilters")
				require.NotNil(t, result.NextPageToken)
				assert.Equal(t, "opaque-cursor-abc", *result.NextPageToken)
				assert.False(t, result.ResultsTruncated, "total_size matches the returned count, so no truncation")
			},
		},
		{
			name: "cold-cache first page with total_size exceeding the returned count and no next_page_token reports results_truncated",
			newService: func() (membershipservice.Service, any) {
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1"}},
						NextPageToken: "",
						TotalSize:     5,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{ProjectUID: "project-uid-1"},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 5, *result.TotalSize)
				assert.Nil(t, result.NextPageToken)
				assert.True(t, result.ResultsTruncated,
					"total_size exceeding the returned record count with no next_page_token on a first "+
						"page is the signature of a cold-cache read racing the background batch-link sweep")
			},
		},
		{
			name: "last page of an ordinary multi-page result does not report results_truncated",
			newService: func() (membershipservice.Service, any) {
				reader := &stubListPage{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					page: model.MembershipPage{
						Memberships:   []*model.ProjectMembership{{UID: "m-1", ProjectUID: "project-uid-1"}},
						NextPageToken: "",
						TotalSize:     5,
					},
				}
				svc := newTestSvc(withStorage(reader))
				return svc, reader
			},
			payload: &membershipservice.ListProjectMembershipsPayload{
				ProjectUID: "project-uid-1",
				// An inbound page_token means this page is a continuation, not a
				// cold-cache first page, so total_size (the query-wide total)
				// legitimately exceeding this page's own record count is not
				// truncation — it's the normal shape of a final page.
				PageToken: stringPtr("prior-cursor-xyz"),
			},
			check: func(t *testing.T, result *membershipservice.ListProjectMembershipsResult, _ any) {
				require.NotNil(t, result.TotalSize)
				assert.Equal(t, 5, *result.TotalSize)
				assert.False(t, result.ResultsTruncated,
					"a later page's total_size exceeding its own record count is expected, not truncation")
			},
		},
		{
			name: "reader error maps to InternalServerError",
			newService: func() (membershipservice.Service, any) {
				reader := &stubMemberReaderListErr{
					MockControllableMemberReader: &mock.MockControllableMemberReader{},
					err:                          pkgerrors.NewUnexpected("reader failed", fmt.Errorf("salesforce error")),
				}
				svc := newTestSvc(withStorage(reader))
				return svc, nil
			},
			payload:     &membershipservice.ListProjectMembershipsPayload{ProjectUID: "project-uid-1"},
			wantErrName: "InternalServerError",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, stub := tt.newService()
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx()
			}
			result, err := svc.ListProjectMemberships(ctx, tt.payload)

			if tt.wantErrName != "" {
				require.Error(t, err)
				var serviceErr *goa.ServiceError
				require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
				assert.Equal(t, tt.wantErrName, serviceErr.Name)
				if tt.wantErrIs != nil {
					assert.True(t, errors.Is(err, tt.wantErrIs), "expected error matching %v, got %T: %v", tt.wantErrIs, err, err)
				}
				return
			}

			if tt.wantErrIs != nil {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tt.wantErrIs), "expected error matching %v, got %T: %v", tt.wantErrIs, err, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			if tt.check != nil {
				tt.check(t, result, stub)
			}
		})
	}
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
