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
	umReader        port.UserMembershipReader
	settingsR       port.B2BOrgSettingsReader
	b2bOrgWriter    usecaseSvc.B2BOrgWriter
	logoUploader    usecaseSvc.LogoUploader
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
func withLogoUploaderUC(u usecaseSvc.LogoUploader) svcOpt {
	return func(b *svcBuilder) { b.logoUploader = u }
}
func withKeyContactWriterUC(w usecaseSvc.KeyContactWriter) svcOpt {
	return func(b *svcBuilder) { b.kcWriter = w }
}
func withStorage(r port.MemberReader) svcOpt {
	return func(b *svcBuilder) { b.storage = r }
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
	return NewMembershipService(b.auth, b.storage, b.b2bOrgReader,
		b.pmReader, b.umReader, b.settingsR, b.b2bOrgWriter, b.logoUploader, b.kcWriter, b.settingsW, b.workspaceWriter, b.runner)
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
	umr.SetUserMemberships("jdoe", []string{"11111111-1111-1111-1111-111111111111", "missing-uid"})
	svc := newTestSvc(withUserMembershipReader(umr))

	res, err := svc.GetMemberTiers(context.Background(), &membershipservice.GetMemberTiersPayload{Username: "jdoe"})

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
	uids := make([]string, maxMemberTierCandidates+1)
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
	uids := make([]string, maxMemberTierCandidates)
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
	assert.Len(t, res, maxMemberTierCandidates)
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

// ─── GetMemberTiers helper tests ──────────────────────────────────────────────

func TestMembershipCountsAsActive(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name    string
		status  string
		endDate string
		want    bool
	}{
		{name: "active with no end date", status: "Active", want: true},
		// Salesforce picklist casing varies between orgs; casing must not drop members.
		{name: "status matches case-insensitively", status: "ACTIVE", want: true},
		{name: "non-active status", status: "Expired", endDate: "2099-12-31", want: false},
		{name: "empty status", status: "", want: false},
		{name: "end date in the past", status: "Active", endDate: "2026-03-14", want: false},
		// A membership stays active through its end date, not just until it.
		{name: "end date today still counts", status: "Active", endDate: "2026-03-15", want: true},
		{name: "end date in the future", status: "Active", endDate: "2027-01-01", want: true},
		{name: "RFC3339 end date in the past", status: "Active", endDate: "2020-01-01T00:00:00Z", want: false},
		// Status is the authority; a malformed date must not silently drop a member.
		{name: "unparseable end date does not deactivate", status: "Active", endDate: "03/15/2020", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &model.ProjectMembership{Status: tt.status, EndDate: tt.endDate}
			assert.Equal(t, tt.want, membershipCountsAsActive(m, now))
		})
	}
}

func TestMembershipOutranks(t *testing.T) {
	m := func(tierName, endDate string) *model.ProjectMembership {
		return &model.ProjectMembership{TierName: tierName, EndDate: endDate}
	}
	tests := []struct {
		name      string
		candidate *model.ProjectMembership
		current   *model.ProjectMembership
		want      bool
	}{
		{
			name:      "higher tier class wins regardless of end dates",
			candidate: m("Platinum Membership", "2026-01-01"),
			current:   m("Gold Membership", "2099-12-31"),
			want:      true,
		},
		{
			name:      "lower tier class never wins",
			candidate: m("Gold Membership", "2099-12-31"),
			current:   m("Platinum Membership", "2026-01-01"),
			want:      false,
		},
		{
			name:      "same class: later end date wins",
			candidate: m("Gold Membership", "2027-01-01"),
			current:   m("Gold Membership", "2026-01-01"),
			want:      true,
		},
		{
			name:      "same class: earlier end date loses",
			candidate: m("Gold Membership", "2026-01-01"),
			current:   m("Gold Membership", "2027-01-01"),
			want:      false,
		},
		{
			// An absent end date is treated as open-ended, so it outlasts any dated membership.
			name:      "same class: open-ended beats dated",
			candidate: m("Gold Membership", ""),
			current:   m("Gold Membership", "2099-01-01"),
			want:      true,
		},
		{
			// Equal on both criteria keeps the incumbent, making the winner deterministic.
			name:      "same class and end date keeps the incumbent",
			candidate: m("Gold Membership", "2026-01-01"),
			current:   m("Gold Membership", "2026-01-01"),
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, membershipOutranks(tt.candidate, tt.current))
		})
	}
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
