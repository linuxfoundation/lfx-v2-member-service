// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
	"goa.design/goa/v3/security"
)

// membershipServicesrvc implements the generated membershipservice.Service interface.
type membershipServicesrvc struct {
	storage                 port.MemberReader
	auth                    domain.Authenticator
	b2bOrgReader            port.B2BOrgReader
	projectMembershipReader port.ProjectMembershipReader
	b2bOrgSettingsReader    port.B2BOrgSettingsReader
	b2bOrgWriter            usecaseSvc.B2BOrgWriter
	keyContactWriter        usecaseSvc.KeyContactWriter
	orgSettingsWriter       usecaseSvc.OrgSettingsWriter
	workspaceWriter         usecaseSvc.WorkspaceWriter
	backfillRunner          *usecaseSvc.Runner
}

// JWTAuth implements the authorization logic for service "membership-service".
func (s *membershipServicesrvc) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	principal, err := s.auth.ParsePrincipal(ctx, token, slog.Default())
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, constants.PrincipalContextID, principal), nil
}

// ── Health probes ─────────────────────────────────────────────────────────────

// Readyz checks if the service is ready to take inbound requests.
func (s *membershipServicesrvc) Readyz(ctx context.Context) ([]byte, error) {
	if err := s.storage.IsReady(ctx); err != nil {
		slog.ErrorContext(ctx, "service not ready", "error", err)
		return nil, err
	}
	return []byte("OK\n"), nil
}

// Livez checks if the service is alive.
func (s *membershipServicesrvc) Livez(_ context.Context) ([]byte, error) {
	return []byte("OK\n"), nil
}

// DebugVars returns the expvar debug variables as a JSON object.
func (s *membershipServicesrvc) DebugVars(_ context.Context) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	first := true
	expvar.Do(func(kv expvar.KeyValue) {
		if !first {
			buf.WriteString(",\n")
		}
		first = false
		key, _ := json.Marshal(kv.Key)
		fmt.Fprintf(&buf, "%s: %s", key, kv.Value.String())
	})
	buf.WriteString("\n}\n")
	return buf.Bytes(), nil
}

// ── B2B Organizations ─────────────────────────────────────────────────────────

// GetB2bOrg retrieves a single B2B organization by UID.
func (s *membershipServicesrvc) GetB2bOrg(ctx context.Context, p *membershipservice.GetB2bOrgPayload) (*membershipservice.GetB2bOrgResult, error) {
	p.UID = normalizeSFID(p.UID)
	org, err := s.b2bOrgReader.GetB2BOrg(ctx, p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(org)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for b2b org", "uid", p.UID, "error", etagErr)
	}

	lastMod := org.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.GetB2bOrgResult{
		B2bOrg:       b2bOrgToResponse(org),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// CreateB2bOrg creates a new B2B organization record from an existing Salesforce Account.
func (s *membershipServicesrvc) CreateB2bOrg(ctx context.Context, p *membershipservice.CreateB2bOrgPayload) (*membershipservice.CreateB2bOrgResult, error) {
	org, err := s.b2bOrgWriter.Create(ctx, p.Sfid)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(org)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for b2b org", "uid", org.UID, "error", etagErr)
	}

	lastMod := org.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.CreateB2bOrgResult{
		B2bOrg:       b2bOrgToResponse(org),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// UpdateB2bOrg updates a B2B organization.
//
// ETag validation and no-op detection are handled by the orchestrator. When
// If-Match is absent the update is unconditional; when present and stale, 412
// is returned. A no-op (no payload changes) returns the current record as-is.
func (s *membershipServicesrvc) UpdateB2bOrg(ctx context.Context, p *membershipservice.UpdateB2bOrgPayload) (*membershipservice.UpdateB2bOrgResult, error) {
	p.UID = normalizeSFID(p.UID)
	input := payloadToB2BOrgInput(p)
	ifMatch := ""
	if p.IfMatch != nil {
		ifMatch = *p.IfMatch
	}
	org, err := s.b2bOrgWriter.Update(ctx, p.UID, input, ifMatch)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(org)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for b2b org", "uid", p.UID, "error", etagErr)
	}

	lastMod := org.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.UpdateB2bOrgResult{
		B2bOrg:       b2bOrgToResponse(org),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// ── Project Memberships ──────────────────────────────────────────────────────

// GetProjectMembership retrieves a single membership by UID and assembles the
// fully denormalised record from its constituent Salesforce objects.
func (s *membershipServicesrvc) GetProjectMembership(ctx context.Context, p *membershipservice.GetProjectMembershipPayload) (*membershipservice.GetProjectMembershipResult, error) {
	p.UID = normalizeSFID(p.UID)
	membership, lastMod, err := s.projectMembershipReader.AssembleProjectMembership(ctx, p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(membership)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for project membership", "uid", p.UID, "error", etagErr)
	}

	lastModStr := lastMod.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.GetProjectMembershipResult{
		ProjectMembership: projectMembershipToResponse(membership),
		LastModified:      &lastModStr,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// ── Key Contacts ─────────────────────────────────────────────────────────────

// GetKeyContact retrieves a single key contact by UID.
func (s *membershipServicesrvc) GetKeyContact(ctx context.Context, p *membershipservice.GetKeyContactPayload) (*membershipservice.GetKeyContactResult, error) {
	p.UID = normalizeSFID(p.UID)
	p.MembershipUID = normalizeSFID(p.MembershipUID)

	kc, err := s.storage.GetKeyContact(ctx, p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	// 404 (not 403) to avoid leaking existence of contacts in other memberships.
	if kc.MembershipUID != p.MembershipUID {
		return nil, wrapError(ctx, pkgerrors.NewNotFound(
			fmt.Sprintf("key contact %s not found in membership %s", p.UID, p.MembershipUID)))
	}

	etagVal, etagErr := etag.LFXEtag(kc)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for key contact", "uid", p.UID, "error", etagErr)
	}

	lastMod := kc.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.GetKeyContactResult{
		KeyContact:   keyContactToResponse(kc),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// CreateKeyContact creates a new key contact.
func (s *membershipServicesrvc) CreateKeyContact(ctx context.Context, p *membershipservice.CreateKeyContactPayload) (*membershipservice.CreateKeyContactResult, error) {
	p.MembershipUID = normalizeSFID(p.MembershipUID)
	in := usecaseSvc.KeyContactCreateInput{
		MembershipUID:  p.MembershipUID,
		FirstName:      p.FirstName,
		LastName:       p.LastName,
		Email:          p.Email,
		Title:          p.Title,
		Role:           p.Role,
		Status:         p.Status,
		BoardMember:    p.BoardMember,
		PrimaryContact: p.PrimaryContact,
		SendInvite:     p.SendInvite,
	}
	kc, err := s.keyContactWriter.Create(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(kc)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for key contact", "uid", kc.UID, "error", etagErr)
	}

	lastMod := kc.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.CreateKeyContactResult{
		KeyContact:   keyContactToResponse(kc),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// UpdateKeyContact updates a key contact.
//
// Cross-membership 404 check is performed here before delegating to the
// orchestrator — avoids leaking record existence across membership boundaries.
func (s *membershipServicesrvc) UpdateKeyContact(ctx context.Context, p *membershipservice.UpdateKeyContactPayload) (*membershipservice.UpdateKeyContactResult, error) {
	p.UID = normalizeSFID(p.UID)
	p.MembershipUID = normalizeSFID(p.MembershipUID)

	// 404 (not 403) to avoid leaking existence of contacts in other memberships.
	current, err := s.storage.GetKeyContact(ctx, p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	if current.MembershipUID != p.MembershipUID {
		return nil, wrapError(ctx, pkgerrors.NewNotFound(
			fmt.Sprintf("key contact %s not found in membership %s", p.UID, p.MembershipUID)))
	}

	in := usecaseSvc.KeyContactUpdateInput{
		MembershipUID:  p.MembershipUID,
		UID:            p.UID,
		Email:          p.Email,
		Title:          p.Title,
		Role:           p.Role,
		Status:         p.Status,
		BoardMember:    p.BoardMember,
		PrimaryContact: p.PrimaryContact,
		IfMatch:        derefStr(p.IfMatch),
		SendInvite:     p.SendInvite,
	}
	kc, err := s.keyContactWriter.Update(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(kc)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for key contact", "uid", p.UID, "error", etagErr)
	}

	lastMod := kc.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.UpdateKeyContactResult{
		KeyContact:   keyContactToResponse(kc),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}

// DeleteKeyContact deletes a key contact.
func (s *membershipServicesrvc) DeleteKeyContact(ctx context.Context, p *membershipservice.DeleteKeyContactPayload) error {
	p.UID = normalizeSFID(p.UID)
	p.MembershipUID = normalizeSFID(p.MembershipUID)

	in := usecaseSvc.KeyContactDeleteInput{
		MembershipUID: p.MembershipUID,
		UID:           p.UID,
		IfMatch:       derefStr(p.IfMatch),
	}

	if err := s.keyContactWriter.Delete(ctx, in); err != nil {
		return wrapError(ctx, err)
	}
	return nil
}

// ── Admin ─────────────────────────────────────────────────────────────────────

// AdminReindex validates the request, spawns an async backfill goroutine, and
// returns 202 Accepted with a run_id for log correlation.
func (s *membershipServicesrvc) AdminReindex(ctx context.Context, p *membershipservice.AdminReindexPayload) (*membershipservice.AdminReindexResult, error) {
	req, err := usecaseSvc.ValidateAndBuildRequest(p)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	runID := uuid.New().String()
	req.RunID = runID

	slog.InfoContext(ctx, "admin reindex accepted — search logs for run_id to track progress",
		"run_id", runID,
		"mode", string(usecaseSvc.ClassifyMode(req)),
		"dry_run", req.DryRun)

	if s.backfillRunner == nil {
		slog.WarnContext(ctx, "backfill runner not initialised — reindex skipped", "run_id", runID)
		return &membershipservice.AdminReindexResult{RunID: runID}, nil
	}

	// cdc_repair does its quota gate and page selection synchronously so the 202
	// can carry selected_count and the caller sees ServiceUnavailable when the
	// quota is unreadable or at/above the admin threshold.
	if req.CDCRepair {
		markers, prepErr := s.backfillRunner.PrepareRepair(ctx, req)
		if prepErr != nil {
			return nil, wrapError(ctx, prepErr)
		}
		selected := len(markers)
		// Fire-and-forget drain; concurrent drains are safe (idempotent reindex
		// + revision-conditional delete). No distributed lock.
		go s.backfillRunner.RunRepair(context.WithoutCancel(ctx), req, markers)
		return &membershipservice.AdminReindexResult{RunID: runID, SelectedCount: &selected}, nil
	}

	// Synchronous quota gate for full/filtered runs (targeted is exempt — bounded
	// surgical tool). Returns 503 before launching the async run so the operator
	// gets immediate feedback instead of discovering a refused run in the logs.
	if gateErr := s.backfillRunner.GateBackfillStart(ctx, req); gateErr != nil {
		return nil, wrapError(ctx, gateErr)
	}

	// Fire-and-forget: the backfill runs independently of the HTTP request lifetime.
	// context.WithoutCancel prevents HTTP cancellation from killing a running page, but
	// the goroutine is not registered on the server's shutdown WaitGroup — a SIGTERM
	// during a large reindex will interrupt the run mid-flight (partial index, no error
	// logged). Accepted trade-off: /admin/reindex is a manual recovery tool and the
	// backfill can be re-triggered; graceful-shutdown integration is tracked as a
	// follow-up.
	go s.backfillRunner.Run(context.WithoutCancel(ctx), req) //nolint:errcheck // fire-and-forget; error is intentionally discarded (see comment above)

	return &membershipservice.AdminReindexResult{RunID: runID}, nil
}

// ── Response converters ───────────────────────────────────────────────────────

// b2bOrgToResponse converts a domain B2BOrg to the generated response type.
func b2bOrgToResponse(org *model.B2BOrg) *membershipservice.B2bOrgResponse {
	resp := &membershipservice.B2bOrgResponse{
		UID:  &org.UID,
		Name: &org.Name,
	}
	if org.Description != "" {
		resp.Description = &org.Description
	}
	if org.Phone != "" {
		resp.Phone = &org.Phone
	}
	if org.Website != "" {
		resp.Website = &org.Website
	}
	if org.PrimaryDomain != "" {
		resp.PrimaryDomain = &org.PrimaryDomain
	}
	if len(org.DomainAliases) > 0 {
		resp.DomainAliases = org.DomainAliases
	}
	if org.LogoURL != "" {
		resp.LogoURL = &org.LogoURL
	}
	if org.Industry != "" {
		resp.Industry = &org.Industry
	}
	if org.Sector != "" {
		resp.Sector = &org.Sector
	}
	if org.CrunchBaseURL != nil {
		resp.CrunchBaseURL = org.CrunchBaseURL
	}
	if org.NumberOfEmployees != nil {
		n := int(*org.NumberOfEmployees)
		resp.NumberOfEmployees = &n
	}
	if org.Status != "" {
		resp.Status = &org.Status
	}
	resp.IsMember = &org.IsMember
	if org.Slug != "" {
		resp.Slug = &org.Slug
	}
	if org.ParentUID != "" {
		resp.ParentUID = &org.ParentUID
	}
	createdAt := org.CreatedAt.UTC().Format(time.RFC3339)
	resp.CreatedAt = &createdAt
	updatedAt := org.UpdatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedAt = &updatedAt
	return resp
}

// projectMembershipToResponse converts a domain ProjectMembership to the
// generated response type.
func projectMembershipToResponse(m *model.ProjectMembership) *membershipservice.ProjectMembershipResponse {
	resp := &membershipservice.ProjectMembershipResponse{
		UID: &m.UID,
	}

	if m.TierUID != "" {
		resp.TierUID = &m.TierUID
	}
	if m.ProjectUID != "" {
		resp.ProjectUID = &m.ProjectUID
	}
	if m.ProjectSFID != "" {
		resp.ProjectSfid = &m.ProjectSFID
	}
	if m.ProjectSlug != "" {
		resp.ProjectSlug = &m.ProjectSlug
	}
	if m.B2BOrgUID != "" {
		resp.B2bOrgUID = &m.B2BOrgUID
	}
	if m.Status != "" {
		resp.Status = &m.Status
	}
	if m.Year != "" {
		resp.Year = &m.Year
	}
	if m.Tier != "" {
		resp.Tier = &m.Tier
	}
	if m.AutoRenew {
		resp.AutoRenew = &m.AutoRenew
	}
	if m.RenewalType != "" {
		resp.RenewalType = &m.RenewalType
	}
	if m.Price != 0 {
		resp.Price = &m.Price
	}
	if m.AnnualFullPrice != 0 {
		resp.AnnualFullPrice = &m.AnnualFullPrice
	}
	if m.PaymentFrequency != "" {
		resp.PaymentFrequency = &m.PaymentFrequency
	}
	if m.PaymentTerms != "" {
		resp.PaymentTerms = &m.PaymentTerms
	}
	if m.AgreementDate != "" {
		resp.AgreementDate = &m.AgreementDate
	}
	if m.PurchaseDate != "" {
		resp.PurchaseDate = &m.PurchaseDate
	}
	if m.StartDate != "" {
		resp.StartDate = &m.StartDate
	}
	if m.EndDate != "" {
		resp.EndDate = &m.EndDate
	}
	if m.CompanyName != "" {
		resp.CompanyName = &m.CompanyName
	}
	if m.CompanyLogoURL != "" {
		resp.CompanyLogoURL = &m.CompanyLogoURL
	}
	if m.CompanyDomain != "" {
		resp.CompanyDomain = &m.CompanyDomain
	}
	if m.TierName != "" {
		resp.TierName = &m.TierName
	}
	if m.TierFamily != "" {
		resp.TierFamily = &m.TierFamily
	}
	if m.TierProductType != "" {
		resp.TierProductType = &m.TierProductType
	}

	createdAt := m.CreatedAt.UTC().Format(time.RFC3339)
	resp.CreatedAt = &createdAt
	updatedAt := m.UpdatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedAt = &updatedAt

	return resp
}

// keyContactToResponse converts a domain KeyContact to the generated response type.
func keyContactToResponse(kc *model.KeyContact) *membershipservice.ProjectKeyContactResponse {
	resp := &membershipservice.ProjectKeyContactResponse{
		UID: &kc.UID,
	}

	if kc.MembershipUID != "" {
		resp.MembershipUID = &kc.MembershipUID
	}
	if kc.TierUID != "" {
		resp.TierUID = &kc.TierUID
	}
	if kc.ProjectUID != "" {
		resp.ProjectUID = &kc.ProjectUID
	}
	if kc.ProjectSFID != "" {
		resp.ProjectSfid = &kc.ProjectSFID
	}
	if kc.B2BOrgUID != "" {
		resp.B2bOrgUID = &kc.B2BOrgUID
	}
	if kc.Role != "" {
		resp.Role = &kc.Role
	}
	if kc.Status != "" {
		resp.Status = &kc.Status
	}
	if kc.BoardMember {
		resp.BoardMember = &kc.BoardMember
	}
	if kc.PrimaryContact {
		resp.PrimaryContact = &kc.PrimaryContact
	}
	if kc.FirstName != "" {
		resp.FirstName = &kc.FirstName
	}
	if kc.LastName != "" {
		resp.LastName = &kc.LastName
	}
	if kc.Title != "" {
		resp.Title = &kc.Title
	}
	if kc.Email != "" {
		resp.Email = &kc.Email
	}
	if kc.CompanyName != "" {
		resp.CompanyName = &kc.CompanyName
	}
	if kc.CompanyLogoURL != "" {
		resp.CompanyLogoURL = &kc.CompanyLogoURL
	}
	if kc.CompanyDomain != "" {
		resp.CompanyDomain = &kc.CompanyDomain
	}

	createdAt := kc.CreatedAt.UTC().Format(time.RFC3339)
	resp.CreatedAt = &createdAt
	updatedAt := kc.UpdatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedAt = &updatedAt

	return resp
}

// derefStr dereferences a *string, returning "" when nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// normalizeSFID normalizes s to its canonical 18-char Salesforce ID form.
// If s is not a valid SFID (e.g. a UUID from tests or an arbitrary string),
// it is returned unchanged so comparisons against mock data still work.
func normalizeSFID(s string) string {
	if normalized, err := sfuuid.Normalize18(s); err == nil {
		return normalized
	}
	return s
}

// payloadToB2BOrgInput maps an UpdateB2bOrgPayload to a model.B2BOrgInput.
func payloadToB2BOrgInput(p *membershipservice.UpdateB2bOrgPayload) model.B2BOrgInput {
	input := model.B2BOrgInput{}
	if p.Name != nil {
		input.Name = *p.Name
	}
	if p.Description != nil {
		input.Description = *p.Description
	}
	if p.Phone != nil {
		input.Phone = *p.Phone
	}
	if p.Website != nil {
		input.Website = *p.Website
	}
	if p.PrimaryDomain != nil {
		input.PrimaryDomain = *p.PrimaryDomain
	}
	if p.LogoURL != nil {
		input.LogoURL = *p.LogoURL
	}
	if p.Industry != nil {
		input.Industry = *p.Industry
	}
	if p.Sector != nil {
		input.Sector = *p.Sector
	}
	if p.CrunchBaseURL != nil {
		input.CrunchBaseURL = p.CrunchBaseURL
	}
	if p.NumberOfEmployees != nil {
		n := int64(*p.NumberOfEmployees)
		input.NumberOfEmployees = &n
	}
	return input
}

// ── Org settings handlers ─────────────────────────────────────────────────────

// GetB2bOrgSettings returns the current access-control settings for a b2b_org.
// When no settings record exists yet it returns empty arrays — not a 404.
func (s *membershipServicesrvc) GetB2bOrgSettings(ctx context.Context, p *membershipservice.GetB2bOrgSettingsPayload) (*membershipservice.GetB2bOrgSettingsResult, error) {
	p.UID = normalizeSFID(p.UID)
	settings, _, err := s.b2bOrgSettingsReader.GetSettings(ctx, p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	result := &membershipservice.GetB2bOrgSettingsResult{
		Settings: orgSettingsToResponse(settings),
	}
	etagVal, etagErr := etag.LFXEtag(settings)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for b2b org settings", "uid", p.UID, "error", etagErr)
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	if settings != nil {
		lastMod := settings.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
		result.LastModified = &lastMod
	}
	return result, nil
}

// UpdateB2bOrgSettings fully replaces the writers and/or auditors for a b2b_org.
// Nil writers/auditors = leave existing unchanged; explicit empty slice = clear.
func (s *membershipServicesrvc) UpdateB2bOrgSettings(ctx context.Context, p *membershipservice.UpdateB2bOrgSettingsPayload) (*membershipservice.UpdateB2bOrgSettingsResult, error) {
	p.UID = normalizeSFID(p.UID)
	now := time.Now().UTC()
	in := usecaseSvc.B2BOrgSettingsUpdate{
		OrgUID:  p.UID,
		IfMatch: derefStr(p.IfMatch),
	}
	if p.Writers != nil {
		in.Writers = orgUsersFromPayload(p.Writers, now)
	}
	if p.Auditors != nil {
		in.Auditors = orgUsersFromPayload(p.Auditors, now)
	}

	updated, err := s.orgSettingsWriter.Update(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	result := &membershipservice.UpdateB2bOrgSettingsResult{
		Settings: orgSettingsToResponse(updated),
	}
	etagVal, etagErr := etag.LFXEtag(updated)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for b2b org settings", "uid", p.UID, "error", etagErr)
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	lastMod := updated.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result.LastModified = &lastMod
	return result, nil
}

// AddB2bOrgSettingsUser adds (invites) a single principal to a b2b_org's writers/auditors.
// Per-principal merge: existing members are preserved; the new entry lands as a pending invite.
func (s *membershipServicesrvc) AddB2bOrgSettingsUser(ctx context.Context, p *membershipservice.AddB2bOrgSettingsUserPayload) (*membershipservice.AddB2bOrgSettingsUserResult, error) {
	p.UID = normalizeSFID(p.UID)
	in := usecaseSvc.B2BOrgSettingsAddPrincipal{
		OrgUID:    p.UID,
		Email:     p.Email,
		InvitedAs: p.InvitedAs,
		IfMatch:   derefStr(p.IfMatch),
	}
	if p.Name != nil {
		in.Name = *p.Name
	}
	updated, err := s.orgSettingsWriter.AddPrincipal(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.AddB2bOrgSettingsUserResult{
		Settings:     orgSettingsToResponse(updated),
		Etag:         settingsETagHeader(ctx, updated, p.UID),
		LastModified: settingsLastModifiedHeader(updated),
	}, nil
}

// UpdateB2bOrgSettingsUserRole changes one principal's role (writer⇄auditor), preserving
// its username and invite lifecycle and leaving all other members untouched.
func (s *membershipServicesrvc) UpdateB2bOrgSettingsUserRole(ctx context.Context, p *membershipservice.UpdateB2bOrgSettingsUserRolePayload) (*membershipservice.UpdateB2bOrgSettingsUserRoleResult, error) {
	p.UID = normalizeSFID(p.UID)
	in := usecaseSvc.B2BOrgSettingsChangeRole{
		OrgUID:    p.UID,
		Email:     p.Email,
		InvitedAs: p.InvitedAs,
		IfMatch:   derefStr(p.IfMatch),
	}
	updated, err := s.orgSettingsWriter.ChangePrincipalRole(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.UpdateB2bOrgSettingsUserRoleResult{
		Settings:     orgSettingsToResponse(updated),
		Etag:         settingsETagHeader(ctx, updated, p.UID),
		LastModified: settingsLastModifiedHeader(updated),
	}, nil
}

// DeleteB2bOrgSettingsUser removes one principal (revoke accepted grant or cancel pending invite),
// leaving all other members untouched. The last accepted Admin cannot be removed.
func (s *membershipServicesrvc) DeleteB2bOrgSettingsUser(ctx context.Context, p *membershipservice.DeleteB2bOrgSettingsUserPayload) (*membershipservice.DeleteB2bOrgSettingsUserResult, error) {
	p.UID = normalizeSFID(p.UID)
	in := usecaseSvc.B2BOrgSettingsRemovePrincipal{
		OrgUID:  p.UID,
		Email:   p.Email,
		IfMatch: derefStr(p.IfMatch),
	}
	updated, err := s.orgSettingsWriter.RemovePrincipal(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.DeleteB2bOrgSettingsUserResult{
		Settings:     orgSettingsToResponse(updated),
		Etag:         settingsETagHeader(ctx, updated, p.UID),
		LastModified: settingsLastModifiedHeader(updated),
	}, nil
}

// ── Workspace handlers ────────────────────────────────────────────────────────

// CreateB2bOrgWorkspace creates a new workspace in a b2b_org.
func (s *membershipServicesrvc) CreateB2bOrgWorkspace(ctx context.Context, p *membershipservice.CreateB2bOrgWorkspacePayload) (*membershipservice.CreateB2bOrgWorkspaceResult, error) {
	p.UID = normalizeSFID(p.UID)
	principal, _ := ctx.Value(constants.PrincipalContextID).(string)
	in := usecaseSvc.WorkspaceCreate{
		OrgUID:    p.UID,
		Name:      p.Name,
		CreatedBy: principal,
		IfMatch:   derefStr(p.IfMatch),
	}
	result, err := s.workspaceWriter.CreateWorkspace(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.CreateB2bOrgWorkspaceResult{
		Workspace:    workspaceToResponse(result.Workspace),
		Etag:         workspaceRegistryETagHeader(ctx, result.Registry),
		LastModified: workspaceRegistryLastModifiedHeader(result.Registry),
	}, nil
}

// UpdateB2bOrgWorkspace renames an existing workspace.
func (s *membershipServicesrvc) UpdateB2bOrgWorkspace(ctx context.Context, p *membershipservice.UpdateB2bOrgWorkspacePayload) (*membershipservice.UpdateB2bOrgWorkspaceResult, error) {
	p.UID = normalizeSFID(p.UID)
	principal, _ := ctx.Value(constants.PrincipalContextID).(string)
	in := usecaseSvc.WorkspaceUpdate{
		OrgUID:       p.UID,
		WorkspaceUID: p.WorkspaceUID,
		Name:         p.Name,
		UpdatedBy:    principal,
		IfMatch:      derefStr(p.IfMatch),
	}
	result, err := s.workspaceWriter.UpdateWorkspace(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.UpdateB2bOrgWorkspaceResult{
		Workspace:    workspaceToResponse(result.Workspace),
		Etag:         workspaceRegistryETagHeader(ctx, result.Registry),
		LastModified: workspaceRegistryLastModifiedHeader(result.Registry),
	}, nil
}

// DeleteB2bOrgWorkspace deletes a workspace and all its project associations.
func (s *membershipServicesrvc) DeleteB2bOrgWorkspace(ctx context.Context, p *membershipservice.DeleteB2bOrgWorkspacePayload) error {
	p.UID = normalizeSFID(p.UID)
	in := usecaseSvc.WorkspaceDelete{
		OrgUID:       p.UID,
		WorkspaceUID: p.WorkspaceUID,
		IfMatch:      derefStr(p.IfMatch),
	}
	if err := s.workspaceWriter.DeleteWorkspace(ctx, in); err != nil {
		return wrapError(ctx, err)
	}
	return nil
}

// AddB2bOrgWorkspaceProject adds a single project to a workspace.
func (s *membershipServicesrvc) AddB2bOrgWorkspaceProject(ctx context.Context, p *membershipservice.AddB2bOrgWorkspaceProjectPayload) (*membershipservice.AddB2bOrgWorkspaceProjectResult, error) {
	p.UID = normalizeSFID(p.UID)
	principal, _ := ctx.Value(constants.PrincipalContextID).(string)
	in := usecaseSvc.WorkspaceProjectAdd{
		OrgUID:       p.UID,
		WorkspaceUID: p.WorkspaceUID,
		ProjectSlug:  p.ProjectSlug,
		ProjectName:  derefStr(p.ProjectName),
		CreatedBy:    principal,
		IfMatch:      derefStr(p.IfMatch),
	}
	result, err := s.workspaceWriter.AddProject(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.AddB2bOrgWorkspaceProjectResult{
		Workspace:    workspaceWithProjectsToResponse(result.Workspace, result.Projects),
		Etag:         workspaceProjectsETagHeader(ctx, result.Projects),
		LastModified: workspaceProjectsLastModifiedHeader(result.Projects),
	}, nil
}

// BulkAddB2bOrgWorkspaceProjects adds multiple projects to a workspace in one operation.
func (s *membershipServicesrvc) BulkAddB2bOrgWorkspaceProjects(ctx context.Context, p *membershipservice.BulkAddB2bOrgWorkspaceProjectsPayload) (*membershipservice.WorkspaceBulkResponse, error) {
	p.UID = normalizeSFID(p.UID)
	principal, _ := ctx.Value(constants.PrincipalContextID).(string)
	items := make([]usecaseSvc.WorkspaceProjectItem, 0, len(p.Projects))
	for _, it := range p.Projects {
		// Keep index alignment: a nil array element becomes a blank-slug item
		// so it surfaces as a per-item validation failure rather than being
		// silently dropped from both succeeded and failed.
		var item usecaseSvc.WorkspaceProjectItem
		if it != nil {
			item.Slug = it.ProjectSlug
			item.Name = derefStr(it.ProjectName)
		}
		items = append(items, item)
	}
	in := usecaseSvc.WorkspaceProjectsBulkAdd{
		OrgUID:       p.UID,
		WorkspaceUID: p.WorkspaceUID,
		Projects:     items,
		CreatedBy:    principal,
		IfMatch:      derefStr(p.IfMatch),
	}
	result, err := s.workspaceWriter.AddProjectsBulk(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	succeeded := make([]string, 0, len(result.Succeeded))
	for _, info := range result.Succeeded {
		succeeded = append(succeeded, info.Slug)
	}
	failed := make([]*membershipservice.WorkspaceBulkAddItemError, 0, len(items))
	for i, ferr := range result.Failed {
		if ferr != nil {
			slug := ""
			if i < len(items) {
				slug = items[i].Slug
			}
			failed = append(failed, &membershipservice.WorkspaceBulkAddItemError{
				ProjectSlug: slug,
				Error:       ferr.Error(),
			})
		}
	}
	return &membershipservice.WorkspaceBulkResponse{
		Workspace:    workspaceWithProjectsToResponse(result.Workspace, result.Projects),
		Succeeded:    succeeded,
		Failed:       failed,
		Etag:         workspaceProjectsETagHeader(ctx, result.Projects),
		LastModified: workspaceProjectsLastModifiedHeader(result.Projects),
	}, nil
}

// RemoveB2bOrgWorkspaceProject removes a project association from a workspace.
func (s *membershipServicesrvc) RemoveB2bOrgWorkspaceProject(ctx context.Context, p *membershipservice.RemoveB2bOrgWorkspaceProjectPayload) (*membershipservice.RemoveB2bOrgWorkspaceProjectResult, error) {
	p.UID = normalizeSFID(p.UID)
	in := usecaseSvc.WorkspaceProjectRemove{
		OrgUID:       p.UID,
		WorkspaceUID: p.WorkspaceUID,
		ProjectUID:   p.ProjectUID,
		IfMatch:      derefStr(p.IfMatch),
	}
	result, err := s.workspaceWriter.RemoveProject(ctx, in)
	if err != nil {
		return nil, wrapError(ctx, err)
	}
	return &membershipservice.RemoveB2bOrgWorkspaceProjectResult{
		Workspace:    workspaceWithProjectsToResponse(result.Workspace, result.Projects),
		Etag:         workspaceProjectsETagHeader(ctx, result.Projects),
		LastModified: workspaceProjectsLastModifiedHeader(result.Projects),
	}, nil
}

// ── Workspace response converters ─────────────────────────────────────────────

// workspaceToResponse maps a domain Workspace to the generated API response type.
// Note: projects are stored in a separate WorkspaceProjects document; this function
// only maps workspace metadata.
func workspaceToResponse(ws *model.Workspace) *membershipservice.WorkspaceResponse {
	if ws == nil {
		return nil
	}
	resp := &membershipservice.WorkspaceResponse{
		UID:  ws.UID,
		Name: ws.Name,
	}
	if ws.CreatedBy != "" {
		resp.CreatedBy = &ws.CreatedBy
	}
	if ws.UpdatedBy != "" {
		resp.UpdatedBy = &ws.UpdatedBy
	}
	createdAt := ws.CreatedAt.UTC().Format(time.RFC3339)
	resp.CreatedAt = &createdAt
	updatedAt := ws.UpdatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedAt = &updatedAt
	return resp
}

// workspaceProjectToResponse maps a domain WorkspaceProject to the generated type.
func workspaceProjectToResponse(p model.WorkspaceProject) *membershipservice.WorkspaceProjectResponse {
	out := &membershipservice.WorkspaceProjectResponse{
		ProjectUID:  p.ProjectUID,
		ProjectSlug: p.ProjectSlug,
	}
	if p.ProjectName != "" {
		out.ProjectName = &p.ProjectName
	}
	if p.CreatedBy != "" {
		out.CreatedBy = &p.CreatedBy
	}
	if p.UpdatedBy != "" {
		out.UpdatedBy = &p.UpdatedBy
	}
	createdAt := p.CreatedAt.UTC().Format(time.RFC3339)
	out.CreatedAt = &createdAt
	updatedAt := p.UpdatedAt.UTC().Format(time.RFC3339)
	out.UpdatedAt = &updatedAt
	return out
}

// computeETag computes an ETag header value for any serialisable object, logging and
// returning nil on failure (the response is still valid without the optional header).
// logUID is included in the warning log to identify which record failed.
func computeETag(ctx context.Context, obj any, logUID string) *string {
	etagVal, err := etag.LFXEtag(obj)
	if err != nil {
		slog.WarnContext(ctx, "failed to compute etag", "uid", logUID, "error", err)
		return nil
	}
	if etagVal == "" {
		return nil
	}
	return &etagVal
}

// formatLastModified returns the HTTP-date formatted Last-Modified header value.
func formatLastModified(t time.Time) *string {
	s := t.UTC().Format(constants.HTTPDateFormat)
	return &s
}

// workspaceRegistryETagHeader computes the ETag header value for workspace
// create/update responses. Hashes *model.OrgWorkspaces — the same type the
// orchestrator validates If-Match against (OrgSettings pattern invariant).
func workspaceRegistryETagHeader(ctx context.Context, registry *model.OrgWorkspaces) *string {
	if registry == nil {
		return nil
	}
	return computeETag(ctx, registry, registry.OrgUID)
}

// workspaceRegistryLastModifiedHeader formats the Last-Modified header for
// workspace create/update responses, using the registry document's UpdatedAt.
func workspaceRegistryLastModifiedHeader(registry *model.OrgWorkspaces) *string {
	if registry == nil {
		return nil
	}
	return formatLastModified(registry.UpdatedAt)
}

// workspaceProjectsETagHeader computes the ETag header value for a workspace projects result.
// Hashes the WorkspaceProjects doc so the ETag reflects only the projects aggregate.
func workspaceProjectsETagHeader(ctx context.Context, wps *model.WorkspaceProjects) *string {
	if wps == nil {
		return nil
	}
	return computeETag(ctx, wps, wps.WorkspaceUID)
}

// workspaceProjectsLastModifiedHeader formats the Last-Modified header for a projects result.
func workspaceProjectsLastModifiedHeader(wps *model.WorkspaceProjects) *string {
	if wps == nil {
		return nil
	}
	return formatLastModified(wps.UpdatedAt)
}

// workspaceWithProjectsToResponse builds a WorkspaceResponse that includes the
// projects list from the separate WorkspaceProjects aggregate.
func workspaceWithProjectsToResponse(ws *model.Workspace, wps *model.WorkspaceProjects) *membershipservice.WorkspaceResponse {
	resp := workspaceToResponse(ws)
	if resp == nil {
		return nil
	}
	if wps != nil {
		projects := make([]*membershipservice.WorkspaceProjectResponse, 0, len(wps.Projects))
		for _, p := range wps.Projects {
			projects = append(projects, workspaceProjectToResponse(p))
		}
		resp.Projects = projects
	}
	return resp
}

// settingsETagHeader computes the ETag header value for a settings result.
func settingsETagHeader(ctx context.Context, updated *model.B2BOrgSettings, uid string) *string {
	if updated == nil {
		return nil
	}
	return computeETag(ctx, updated, uid)
}

// settingsLastModifiedHeader formats the Last-Modified header value for a settings result.
func settingsLastModifiedHeader(updated *model.B2BOrgSettings) *string {
	if updated == nil {
		return nil
	}
	return formatLastModified(updated.UpdatedAt)
}

// orgSettingsToResponse maps model.B2BOrgSettings to the generated response type.
// A nil settings pointer is treated as empty (no settings stored yet).
func orgSettingsToResponse(s *model.B2BOrgSettings) *membershipservice.B2bOrgSettingsResponse {
	resp := &membershipservice.B2bOrgSettingsResponse{
		Writers:  []*membershipservice.OrgUser{},
		Auditors: []*membershipservice.OrgUser{},
	}
	if s == nil {
		return resp
	}
	for _, u := range s.Writers {
		resp.Writers = append(resp.Writers, orgUserToResponse(u))
	}
	for _, u := range s.Auditors {
		resp.Auditors = append(resp.Auditors, orgUserToResponse(u))
	}
	createdAt := s.CreatedAt.UTC().Format(time.RFC3339)
	resp.CreatedAt = &createdAt
	updatedAt := s.UpdatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedAt = &updatedAt
	return resp
}

// orgUserToResponse maps a domain B2BOrgUser to the generated API type.
func orgUserToResponse(u model.B2BOrgUser) *membershipservice.OrgUser {
	out := &membershipservice.OrgUser{
		Email:     u.Email,
		InvitedAs: u.InvitedAs,
	}
	if u.Avatar != "" {
		out.Avatar = &u.Avatar
	}
	if u.Name != "" {
		out.Name = &u.Name
	}
	if u.Username != "" {
		out.Username = &u.Username
	}
	status := string(u.EffectiveStatus())
	out.InviteStatus = &status
	return out
}

// orgUsersFromPayload maps the API payload slice to domain B2BOrgUser slice, deriving
// InviteStatus: accepted when Username is set, pending otherwise.
func orgUsersFromPayload(users []*membershipservice.OrgUser, now time.Time) []model.B2BOrgUser {
	out := make([]model.B2BOrgUser, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		du := model.B2BOrgUser{
			Email:        u.Email,
			InvitedAs:    u.InvitedAs,
			InviteStatus: model.InviteStatusPending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if u.Avatar != nil {
			du.Avatar = *u.Avatar
		}
		if u.Name != nil {
			du.Name = *u.Name
		}
		if u.Username != nil && *u.Username != "" {
			du.Username = *u.Username
			du.InviteStatus = model.InviteStatusAccepted
		}
		out = append(out, du)
	}
	return out
}

// ── Constructor ───────────────────────────────────────────────────────────────

// NewMembershipService returns the membership-service implementation with
// injected dependencies.
func NewMembershipService(
	auth domain.Authenticator,
	storage port.MemberReader,
	b2bOrgReader port.B2BOrgReader,
	projectMshipR port.ProjectMembershipReader,
	b2bOrgSettingsReader port.B2BOrgSettingsReader,
	b2bOrgWriter usecaseSvc.B2BOrgWriter,
	keyContactWriter usecaseSvc.KeyContactWriter,
	orgSettingsWriter usecaseSvc.OrgSettingsWriter,
	workspaceWriter usecaseSvc.WorkspaceWriter,
	backfillRunner *usecaseSvc.Runner,
) membershipservice.Service {
	return &membershipServicesrvc{
		storage:                 storage,
		auth:                    auth,
		b2bOrgReader:            b2bOrgReader,
		projectMembershipReader: projectMshipR,
		b2bOrgSettingsReader:    b2bOrgSettingsReader,
		b2bOrgWriter:            b2bOrgWriter,
		keyContactWriter:        keyContactWriter,
		orgSettingsWriter:       orgSettingsWriter,
		workspaceWriter:         workspaceWriter,
		backfillRunner:          backfillRunner,
	}
}
