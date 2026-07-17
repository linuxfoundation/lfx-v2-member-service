// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	inviteapi "github.com/linuxfoundation/lfx-v2-invite-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// maxPrincipals bounds the writers/auditors list length to prevent unbounded NATS KV
// value growth. Largest prod orgs carry ~300 principals; 700 gives comfortable headroom
// while remaining a practical safety bound against runaway callers.
const maxPrincipals = 700

// B2BOrgSettingsUpdate carries the validated parameters for a settings update.
// Writers/Auditors nil = keep existing; explicit empty slice = clear all.
// IfMatch == "" means first-write-wins (no ETag check).
type B2BOrgSettingsUpdate struct {
	OrgUID   string
	Writers  []model.B2BOrgUser // nil = keep, [] = clear all
	Auditors []model.B2BOrgUser // nil = keep, [] = clear all
	IfMatch  string
}

// OrgSettingsPrincipalWriter is the consumer-side port for per-principal
// org-dashboard access management. Defined at the consumer boundary (ISP):
// callers only need add/remove — not the full OrgSettingsWriter surface.
// *orgSettingsWriterOrchestrator satisfies this interface.
type OrgSettingsPrincipalWriter interface {
	AddPrincipal(ctx context.Context, in B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error)
	RemovePrincipal(ctx context.Context, in B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error)
	ChangePrincipalRole(ctx context.Context, in B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error)
}

// OrgSettingsUpdater is the consumer-side port for org-settings promotion
// (invite acceptance). Narrow interface: InviteAcceptedService only needs
// Update — not the full OrgSettingsWriter surface.
type OrgSettingsUpdater interface {
	Update(ctx context.Context, in B2BOrgSettingsUpdate) (*model.B2BOrgSettings, error)
}

// KeyContactOrgReader is the consumer-side port for listing key contacts
// scoped to a single org. Used by InviteAcceptedService to grant FGA on
// invite acceptance and by KeyContactWriter to guard org-dashboard revocation.
type KeyContactOrgReader interface {
	ListKeyContactsForOrg(ctx context.Context, orgSFID string) ([]*model.KeyContact, error)
}

// OrgSettingsWriter orchestrates the UpdateB2bOrgSettings use case.
type OrgSettingsWriter interface {
	Update(ctx context.Context, in B2BOrgSettingsUpdate) (*model.B2BOrgSettings, error)
	// AddPrincipal adds (invites) one principal, preserving every existing member.
	AddPrincipal(ctx context.Context, in B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error)
	// ChangePrincipalRole moves one principal between writers/auditors, preserving
	// its username and invite lifecycle so an accepted grant stays accepted.
	ChangePrincipalRole(ctx context.Context, in B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error)
	// RemovePrincipal removes one principal (revoke accepted grant or cancel pending invite).
	RemovePrincipal(ctx context.Context, in B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error)
}

// B2BOrgSettingsAddPrincipal carries the validated parameters for a per-principal add.
type B2BOrgSettingsAddPrincipal struct {
	OrgUID    string
	Email     string
	InvitedAs string // "writer" or "auditor"
	Name      string
	IfMatch   string // optional ETag precondition; "" = first-write-wins (no check)
	// SuppressNotification, when true, provisions the entry without sending the
	// invite (unregistered) or role-assignment email (registered). Always set by
	// CDC (passive sync) and by key-contact paths when send_invite=false.
	SuppressNotification bool
}

// B2BOrgSettingsChangeRole carries the validated parameters for a per-principal role change.
type B2BOrgSettingsChangeRole struct {
	OrgUID    string
	Email     string
	InvitedAs string // target relation: "writer" or "auditor"
	IfMatch   string
}

// B2BOrgSettingsRemovePrincipal carries the validated parameters for a per-principal remove.
type B2BOrgSettingsRemovePrincipal struct {
	OrgUID  string
	Email   string
	IfMatch string
}

type orgSettingsWriterOrchestrator struct {
	settingsReader      port.B2BOrgSettingsReader
	settingsWriter      port.B2BOrgSettingsWriter
	b2bOrgReader        port.B2BOrgReader
	publisher           port.MemberPublisher
	userReader          port.UserReader
	inviteSender        port.InviteSender
	roleNotifier        port.OrgRoleNotifier
	lfxSelfServeBaseURL string
}

// OrgSettingsWriterOption configures an orgSettingsWriterOrchestrator.
type OrgSettingsWriterOption func(*orgSettingsWriterOrchestrator)

func WithOrgSettingsReader(r port.B2BOrgSettingsReader) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.settingsReader = r }
}

func WithOrgSettingsWriter(w port.B2BOrgSettingsWriter) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.settingsWriter = w }
}

func WithOrgSettingsB2BOrgReader(r port.B2BOrgReader) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.b2bOrgReader = r }
}

func WithOrgSettingsPublisher(p port.MemberPublisher) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.publisher = p }
}

func WithOrgSettingsUserReader(r port.UserReader) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.userReader = r }
}

func WithOrgSettingsInviteSender(s port.InviteSender) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.inviteSender = s }
}

func WithOrgSettingsRoleNotifier(n port.OrgRoleNotifier) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.roleNotifier = n }
}

func WithOrgSettingsSelfServeBaseURL(u string) OrgSettingsWriterOption {
	return func(o *orgSettingsWriterOrchestrator) { o.lfxSelfServeBaseURL = u }
}

// NewOrgSettingsWriter constructs an OrgSettingsWriter.
func NewOrgSettingsWriter(opts ...OrgSettingsWriterOption) OrgSettingsWriter {
	o := &orgSettingsWriterOrchestrator{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Update applies a B2BOrgSettingsUpdate and publishes FGA sync and indexer messages.
// GetB2BOrg is called once here and shared by both publish helpers to avoid a double fetch.
// FGA is published before the indexer so access tuples land before the doc is searchable.
func (o *orgSettingsWriterOrchestrator) Update(ctx context.Context, in B2BOrgSettingsUpdate) (*model.B2BOrgSettings, error) {
	existing, revision, err := o.settingsReader.GetSettings(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}

	// Optional ETag pre-check: if caller supplied If-Match, validate before writing.
	if in.IfMatch != "" {
		if existing == nil {
			return nil, pkgerrors.NewPreconditionFailed("no settings record exists to match against — omit If-Match for first write")
		}
		currentETag, etagErr := etag.LFXEtag(existing)
		if etagErr != nil {
			return nil, pkgerrors.NewUnexpected("failed to compute etag for settings", etagErr)
		}
		if currentETag != in.IfMatch {
			return nil, pkgerrors.NewPreconditionFailed("settings have been modified since your last read — refresh and retry")
		}
	}

	// Both nil means the caller omitted both fields — semantic no-op, nothing to write or publish.
	if in.Writers == nil && in.Auditors == nil {
		if existing == nil {
			return &model.B2BOrgSettings{UID: in.OrgUID}, nil
		}
		return existing, nil
	}

	// Bound slice length to prevent unbounded NATS KV value growth.
	if len(in.Writers) > maxPrincipals || len(in.Auditors) > maxPrincipals {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("writers and auditors lists must not exceed %d entries each", maxPrincipals))
	}

	now := time.Now().UTC()
	updated := &model.B2BOrgSettings{
		UID:       in.OrgUID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing != nil {
		updated.CreatedAt = existing.CreatedAt
		updated.Writers = slices.Clone(existing.Writers)
		updated.Auditors = slices.Clone(existing.Auditors)
	}

	// Clone the caller-provided slices so the persisted record never aliases the request's
	// backing arrays. Bulk Update does not enrich avatars: a first-time PUT of a large member
	// list would otherwise issue one serial auth-service RPC per missing-avatar member inside
	// the HTTP write. Bulk first-fills are owned by the avatar backfill; the write path enriches
	// only the single just-accepted entry (see AddPrincipal).
	if in.Writers != nil {
		updated.Writers = slices.Clone(in.Writers)
	}
	if in.Auditors != nil {
		updated.Auditors = slices.Clone(in.Auditors)
	}

	if err := o.settingsWriter.UpdateSettings(ctx, updated, revision); err != nil {
		return nil, err
	}

	// Fetch org once; share across both publish helpers.
	action := indexerConstants.ActionUpdated
	if existing == nil {
		action = indexerConstants.ActionCreated
	}
	o.publishAll(ctx, in, updated, action)

	return updated, nil
}

// enrichAvatar refreshes one principal's avatar from auth-service. Best-effort: any miss/error leaves
// the existing value untouched and never fails the write; no-op for pending invites (no username yet).
func (o *orgSettingsWriterOrchestrator) enrichAvatar(ctx context.Context, u *model.B2BOrgUser) {
	if o.userReader == nil || u == nil {
		return
	}
	username := strings.TrimSpace(u.Username)
	if username == "" {
		return
	}
	meta, err := o.userReader.UserMetadataByPrincipal(ctx, username)
	if err != nil {
		if !pkgerrors.IsNotFound(err) {
			slog.WarnContext(ctx, "avatar enrichment lookup failed; leaving existing avatar",
				"username", redaction.Redact(username), "error", err)
		}
		return
	}
	u.Avatar = meta.Picture
}

// AddPrincipal adds (invites) one principal to writers/auditors. Existing members are
// preserved verbatim (full structs, incl. username/invite lifecycle). A live grant for the
// same email (accepted or pending) is a Conflict; a revoked/expired entry is replaced.
func (o *orgSettingsWriterOrchestrator) AddPrincipal(ctx context.Context, in B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error) {
	email := normalizeSettingsEmail(in.Email)
	if email == "" {
		return nil, pkgerrors.NewValidation("email is required")
	}
	if in.InvitedAs != model.B2BOrgRoleWriter && in.InvitedAs != model.B2BOrgRoleAuditor {
		return nil, pkgerrors.NewValidation("invited_as must be writer or auditor")
	}

	existing, revision, err := o.settingsReader.GetSettings(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if err := checkSettingsIfMatch(existing, in.IfMatch); err != nil {
		return nil, err
	}

	// When no settings record exists yet this add would create one. Verify the parent org
	// actually exists first so we never create an orphan settings record for a nonexistent
	// org (and so the advertised NotFound is reachable). Skipped once settings exist (the org
	// was validated at creation) and when no org reader is wired (e.g. minimal local setups).
	// guardOrgName captures the name from this fetch so notifyRoleAssigned can reuse it on
	// the first-principal path instead of issuing a second GetB2BOrg round-trip.
	var guardOrgName string
	if existing == nil && o.b2bOrgReader != nil {
		org, orgErr := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
		if orgErr != nil {
			return nil, orgErr
		}
		if org != nil {
			guardOrgName = org.Name
		}
	}

	now := time.Now().UTC()
	updated := cloneSettings(existing, in.OrgUID, now)

	// Scan every entry for this email across both relations: the same email can appear in
	// both writers and auditors (via the bulk PUT path or legacy writes). Re-invite only
	// when ALL matches are revoked/expired audit entries; if ANY match is still live
	// (accepted or pending) it's a conflict — otherwise the cleanup below would silently
	// delete that active grant while re-inviting a separate revoked one.
	cleanupRan := false
	if matches := findPrincipalsByEmail(updated, email); len(matches) > 0 {
		// Resend-in-place: the only live match is a pending entry for the same role.
		// Instead of Conflict, re-send the invite and refresh the existing entry.
		if existing != nil {
			if resent, ok := o.tryResendInPlace(ctx, in.OrgUID, in.InvitedAs, email, matches, updated, now, in.SuppressNotification); ok {
				// No bounds check needed: resend updates an existing entry in-place and never appends.
				return o.persistAndPublish(ctx, in.OrgUID, existing, resent, revision, now,
					in.InvitedAs == model.B2BOrgRoleWriter, in.InvitedAs == model.B2BOrgRoleAuditor)
			}
		}
		for _, m := range matches {
			status := m.EffectiveStatus()
			if status != model.InviteStatusRevoked && status != model.InviteStatusExpired {
				return nil, pkgerrors.NewConflict("this person already has access or a pending invite")
			}
		}
		// All matches are revoked/expired — drop them before re-inviting.
		updated.Writers = removePrincipalByEmail(updated.Writers, email)
		updated.Auditors = removePrincipalByEmail(updated.Auditors, email)
		cleanupRan = true
	}

	entry := model.B2BOrgUser{
		Email:     email,
		Name:      strings.TrimSpace(in.Name),
		InvitedAs: in.InvitedAs,
		InvitedAt: &now,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Branch on LFID lookup. If userReader is wired, check whether the email
	// already has an LFID; if so, add them as accepted immediately (no invite needed).
	// NotFound means the email has no LFID yet — fall through to the pending-invite path.
	// Any other error (transient auth-service outage, network failure, etc.) is returned
	// to the caller as a 5xx rather than silently creating a spurious pending invite.
	var username string
	if o.userReader != nil {
		var usernameErr error
		username, usernameErr = o.userReader.UsernameByEmail(ctx, email)
		if usernameErr != nil && !pkgerrors.IsNotFound(usernameErr) {
			return nil, fmt.Errorf("lookup LFID for %s: %w", redaction.RedactEmail(email), usernameErr)
		}
	}
	if username != "" {
		entry.Username = username
		entry.InviteStatus = model.InviteStatusAccepted
		entry.AcceptedAt = &now
		o.enrichAvatar(ctx, &entry)
	} else {
		entry.InviteStatus = model.InviteStatusPending
		if !in.SuppressNotification {
			entry.InviteUUID = o.sendOrgInvite(ctx, in.OrgUID, email, entry.Name, in.InvitedAs)
		}
	}

	if in.InvitedAs == model.B2BOrgRoleWriter {
		updated.Writers = append(updated.Writers, entry)
	} else {
		updated.Auditors = append(updated.Auditors, entry)
	}

	// Bound slice length to prevent unbounded NATS KV value growth (parity with Update).
	// Only the relation being appended to is checked: an add to one list must not be
	// blocked by legacy over-cap data sitting in the untouched list.
	if (in.InvitedAs == model.B2BOrgRoleWriter && len(updated.Writers) > maxPrincipals) ||
		(in.InvitedAs == model.B2BOrgRoleAuditor && len(updated.Auditors) > maxPrincipals) {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("writers and auditors lists must not exceed %d entries each", maxPrincipals))
	}

	// Only the target relation changed (plus the other one if a revoked-entry cleanup ran).
	res, err := o.persistAndPublish(ctx, in.OrgUID, existing, updated, revision, now,
		in.InvitedAs == model.B2BOrgRoleWriter || cleanupRan, in.InvitedAs == model.B2BOrgRoleAuditor || cleanupRan)
	// On the existing-LFID path, send a role-assignment notification email
	// best-effort — a transient email failure must never block the caller.
	// Skipped when SuppressNotification=true (e.g. CDC sync, key-contact silent provisioning).
	if err == nil && username != "" && !in.SuppressNotification {
		o.notifyRoleAssigned(ctx, email, in.InvitedAs, in.OrgUID, guardOrgName)
	}
	return res, err
}

// ChangePrincipalRole moves one principal between writers and auditors, preserving
// its full struct (username, invite_status, timestamps) so an accepted grant stays accepted.
func (o *orgSettingsWriterOrchestrator) ChangePrincipalRole(ctx context.Context, in B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error) {
	email := normalizeSettingsEmail(in.Email)
	if email == "" {
		return nil, pkgerrors.NewValidation("email is required")
	}
	if in.InvitedAs != model.B2BOrgRoleWriter && in.InvitedAs != model.B2BOrgRoleAuditor {
		return nil, pkgerrors.NewValidation("invited_as must be writer or auditor")
	}

	existing, revision, err := o.settingsReader.GetSettings(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, pkgerrors.NewNotFound("no settings record exists for this organization")
	}
	if err := checkSettingsIfMatch(existing, in.IfMatch); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	updated := cloneSettings(existing, in.OrgUID, now)
	matches := findPrincipalsByEmail(updated, email)
	if len(matches) == 0 {
		return nil, pkgerrors.NewNotFound("principal not found for this organization")
	}

	// The same email can appear in both relations (via the bulk PUT path). Move the entry
	// that confers the most access (accepted-with-username > accepted > pending > revoked >
	// expired) so a role change never promotes a stale duplicate while dropping a live grant.
	// The remove-from-both + single re-append below collapses any duplicates to one entry.
	moved := mostLivePrincipal(matches)
	if len(matches) == 1 && moved.InvitedAs == in.InvitedAs {
		// True no-op: a single entry already in the target role — nothing to move and no
		// duplicates to collapse. Skip the revision bump and FGA/indexer republish that
		// could turn a concurrent op's If-Match stale (spurious 409).
		return updated, nil
	}
	moved.InvitedAs = in.InvitedAs
	moved.UpdatedAt = now
	updated.Writers = removePrincipalByEmail(updated.Writers, email)
	updated.Auditors = removePrincipalByEmail(updated.Auditors, email)
	if in.InvitedAs == model.B2BOrgRoleWriter {
		updated.Writers = append(updated.Writers, moved)
	} else {
		updated.Auditors = append(updated.Auditors, moved)
	}

	// Bound the destination relation (parity with Update/AddPrincipal): a role move grows the
	// target list by one, so repeated moves must not push it past the per-list cap.
	if (in.InvitedAs == model.B2BOrgRoleWriter && len(updated.Writers) > maxPrincipals) ||
		(in.InvitedAs == model.B2BOrgRoleAuditor && len(updated.Auditors) > maxPrincipals) {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("writers and auditors lists must not exceed %d entries each", maxPrincipals))
	}

	if err := assertNotRemovingLastAdmin(existing, updated); err != nil {
		return nil, err
	}
	// A role move always changes both relations (source loses the entry, target gains it).
	return o.persistAndPublish(ctx, in.OrgUID, existing, updated, revision, now, true, true)
}

// RemovePrincipal removes one principal (revoke accepted grant or cancel pending invite),
// leaving every other member untouched. The last accepted Admin cannot be removed.
func (o *orgSettingsWriterOrchestrator) RemovePrincipal(ctx context.Context, in B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error) {
	email := normalizeSettingsEmail(in.Email)
	if email == "" {
		return nil, pkgerrors.NewValidation("email is required")
	}

	existing, revision, err := o.settingsReader.GetSettings(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, pkgerrors.NewNotFound("no settings record exists for this organization")
	}
	if err := checkSettingsIfMatch(existing, in.IfMatch); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	updated := cloneSettings(existing, in.OrgUID, now)
	// Determine which relation(s) actually contain the principal. This drives both the
	// existence check (must be in at least one) and the FGA sync flags: only a relation that
	// actually changed is reconciled, so removing an auditor never re-syncs writers. The same
	// email can appear in both lists (a bulk-PUT artifact); removePrincipalByEmail below cleans
	// up every copy regardless.
	inWriters := relationHasEmail(updated.Writers, email)
	inAuditors := relationHasEmail(updated.Auditors, email)
	if !inWriters && !inAuditors {
		return nil, pkgerrors.NewNotFound("principal not found for this organization")
	}
	updated.Writers = removePrincipalByEmail(updated.Writers, email)
	updated.Auditors = removePrincipalByEmail(updated.Auditors, email)

	if err := assertNotRemovingLastAdmin(existing, updated); err != nil {
		return nil, err
	}
	return o.persistAndPublish(ctx, in.OrgUID, existing, updated, revision, now, inWriters, inAuditors)
}

// persistAndPublish writes the merged settings (optimistic CAS via revision) and fires the
// FGA + indexer publishes. The caller's `now` is reused for the record-level UpdatedAt so it
// matches the entry-level timestamp stamped on the added/moved member (single consistent
// write time).
//
// syncWriters/syncAuditors declare which relations this operation actually changed. Only those
// are forwarded (non-nil) to the FGA full-sync so it reconciles just the touched relation(s) —
// adding new tuples and revoking removed ones. An untouched relation is passed nil and skipped,
// preserving its existing tuples and avoiding needless FGA churn (e.g. inviting an auditor must
// not re-reconcile every writer). The indexer always publishes the full settings doc regardless.
func (o *orgSettingsWriterOrchestrator) persistAndPublish(
	ctx context.Context,
	orgUID string,
	existing, updated *model.B2BOrgSettings,
	revision uint64,
	now time.Time,
	syncWriters, syncAuditors bool,
) (*model.B2BOrgSettings, error) {
	updated.UpdatedAt = now
	if err := o.settingsWriter.UpdateSettings(ctx, updated, revision); err != nil {
		return nil, err
	}

	action := indexerConstants.ActionUpdated
	if existing == nil {
		action = indexerConstants.ActionCreated
	}
	// A touched relation is coerced to a non-nil slice (empty if it was cleared) so the
	// full-sync runs and revokes removed tuples; an untouched relation stays nil and is skipped.
	in := B2BOrgSettingsUpdate{OrgUID: orgUID}
	if syncWriters {
		in.Writers = updated.Writers
		if in.Writers == nil {
			in.Writers = []model.B2BOrgUser{}
		}
	}
	if syncAuditors {
		in.Auditors = updated.Auditors
		if in.Auditors == nil {
			in.Auditors = []model.B2BOrgUser{}
		}
	}
	o.publishAll(ctx, in, updated, action)
	return updated, nil
}

// normalizeSettingsEmail lowercases and trims an email for case-insensitive matching.
func normalizeSettingsEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// cloneSettings shallow-copies the writers/auditors slices so callers can mutate the copy
// without touching the reader's cached value. A nil source yields a fresh settings record.
func cloneSettings(s *model.B2BOrgSettings, orgUID string, now time.Time) *model.B2BOrgSettings {
	if s == nil {
		return &model.B2BOrgSettings{UID: orgUID, CreatedAt: now, UpdatedAt: now}
	}
	// UID is set from the requested orgUID (the KV key), not the stored payload's UID, so a
	// corrupted or migrated record whose internal UID drifted from its key can never be
	// persisted back under the wrong key (UpdateSettings derives its key from settings.UID).
	return &model.B2BOrgSettings{
		UID:       orgUID,
		Writers:   slices.Clone(s.Writers),
		Auditors:  slices.Clone(s.Auditors),
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// relationHasEmail reports whether any entry in a single relation list matches email.
func relationHasEmail(users []model.B2BOrgUser, email string) bool {
	for _, u := range users {
		if normalizeSettingsEmail(u.Email) == email {
			return true
		}
	}
	return false
}

// findPrincipalsByEmail returns every entry matching email across both writers and auditors.
// AddPrincipal must consider all matches (the same email can appear in both relations via the
// bulk PUT path) so a live grant is never silently dropped while re-inviting a revoked one.
func findPrincipalsByEmail(s *model.B2BOrgSettings, email string) []model.B2BOrgUser {
	var out []model.B2BOrgUser
	for _, list := range [][]model.B2BOrgUser{s.Writers, s.Auditors} {
		for _, u := range list {
			if normalizeSettingsEmail(u.Email) == email {
				out = append(out, u)
			}
		}
	}
	return out
}

// mostLivePrincipal returns the entry that confers the most access among matches, used to
// resolve duplicate-email entries when moving a principal between relations. matches must
// be non-empty.
func mostLivePrincipal(matches []model.B2BOrgUser) model.B2BOrgUser {
	best := matches[0]
	for _, u := range matches[1:] {
		if principalLiveness(u) > principalLiveness(best) {
			best = u
		}
	}
	return best
}

// principalLiveness ranks an entry by how much access it confers (higher = more live).
// Accepted-with-username (the only state that emits an FGA tuple) ranks highest.
func principalLiveness(u model.B2BOrgUser) int {
	switch u.EffectiveStatus() {
	case model.InviteStatusAccepted:
		if u.Username != "" {
			return 4
		}
		return 3
	case model.InviteStatusPending:
		return 2
	case model.InviteStatusRevoked:
		return 1
	default: // expired
		return 0
	}
}

// removePrincipalByEmail returns a new slice with any entry matching email removed.
// A nil input returns nil (not an empty slice) so the nil-vs-empty contract is preserved:
// an untouched relation stays nil and the FGA full-sync skips it rather than revoking all
// of its tuples.
func removePrincipalByEmail(users []model.B2BOrgUser, email string) []model.B2BOrgUser {
	if users == nil {
		return nil
	}
	out := make([]model.B2BOrgUser, 0, len(users))
	for _, u := range users {
		if normalizeSettingsEmail(u.Email) == email {
			continue
		}
		out = append(out, u)
	}
	return out
}

// countFunctionalAdmins counts writers that confer real admin access: accepted entries with a
// non-empty username — the exact condition under which an FGA writer tuple is emitted (see
// model.activeUsernames). An accepted-but-username-less writer grants no access and is not
// counted.
func countFunctionalAdmins(s *model.B2BOrgSettings) int {
	n := 0
	for _, u := range s.Writers {
		if u.EffectiveStatus() == model.InviteStatusAccepted && u.Username != "" {
			n++
		}
	}
	return n
}

// assertNotRemovingLastAdmin rejects a mutation that drops the count of functional admins from
// >=1 to 0. It is a differential (before/after) check, not an absolute post-state one: an org
// that already has zero functional admins — e.g. during the onboarding window before the first
// invited admin accepts and gets a username — is not frozen. Only a transition that removes or
// demotes the last real admin is blocked.
func assertNotRemovingLastAdmin(before, after *model.B2BOrgSettings) error {
	if countFunctionalAdmins(before) >= 1 && countFunctionalAdmins(after) == 0 {
		return pkgerrors.NewConflict("organization must keep at least one Admin")
	}
	return nil
}

// checkIfMatch validates an optional If-Match precondition against the current ETag of
// existing. docLabel is used in error messages (e.g. "settings record", "workspace record").
//
// Returns nil when ifMatch is empty (no precondition supplied).
// Returns PreconditionFailed when existing is nil or the ETag does not match ifMatch.
// Returns an internal error when ETag computation fails (not a precondition failure).
//
// The generic *T parameter is intentional: passing existing as any would box typed-nil
// pointers into a non-nil interface, defeating the nil guard.
func checkIfMatch[T any](existing *T, ifMatch, docLabel string) error {
	if ifMatch == "" {
		return nil
	}
	if existing == nil {
		return pkgerrors.NewPreconditionFailed(
			fmt.Sprintf("no %s exists to match against — omit If-Match for first write", docLabel))
	}
	currentETag, err := etag.LFXEtag(existing)
	if err != nil {
		return pkgerrors.NewUnexpected(fmt.Sprintf("failed to compute etag for %s", docLabel), err)
	}
	if currentETag != ifMatch {
		return pkgerrors.NewPreconditionFailed(
			fmt.Sprintf("%s has been modified since your last read — refresh and retry", docLabel))
	}
	return nil
}

// checkSettingsIfMatch validates the optional If-Match precondition against the current settings ETag.
func checkSettingsIfMatch(existing *model.B2BOrgSettings, ifMatch string) error {
	return checkIfMatch(existing, ifMatch, "settings record")
}

// publishAll fetches the parent org once and drives both the FGA and indexer publishes.
// Both are fire-and-forget: errors are logged, never returned to the caller.
// FGA is published first so access tuples land before the indexer doc is searchable.
func (o *orgSettingsWriterOrchestrator) publishAll(ctx context.Context, in B2BOrgSettingsUpdate, settings *model.B2BOrgSettings, action indexerConstants.MessageAction) {
	if o.b2bOrgReader == nil || o.publisher == nil {
		return
	}
	org, err := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
	if err != nil {
		slog.WarnContext(ctx, "could not fetch org for settings publish — skipping FGA and indexer",
			"uid", in.OrgUID, "error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if org == nil {
		slog.WarnContext(ctx, "org fetch returned nil with no error — skipping FGA and indexer",
			"uid", in.OrgUID,
			"publish_failed_for_backfill_repair", true)
		return
	}

	fgaMsg := BuildB2BOrgFGAMessage(
		org,
		"",
		fgaUsernames(in.Writers, settings.ActiveWriterUsernames()),
		fgaUsernames(in.Auditors, settings.ActiveAuditorUsernames()),
		nil,
	)
	if pubErr := o.publisher.Access(ctx, constants.FGASyncUpdateAccessSubject, fgaMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "b2b org settings FGA publish failed",
			"uid", in.OrgUID, "error", pubErr,
			"publish_failed_for_backfill_repair", true)
	}

	PublishB2BOrgSettingsIndexer(ctx, o.publisher, org, settings, action)
}

// fgaUsernames maps the nil-vs-empty distinction from the input slice through to
// the FGA sync layer. nil input = caller did not touch this relation → return nil
// so BuildB2BOrgFGAMessage excludes it from the full-sync (preserving existing
// tuples). Non-nil input = caller explicitly replaced the list → return a non-nil
// slice (possibly empty) so the full-sync runs and revokes any removed tuples.
func fgaUsernames(input []model.B2BOrgUser, active []string) []string {
	if input == nil {
		return nil
	}
	if active == nil {
		return []string{} // non-nil empty: signal "replace with nothing"
	}
	return active
}

// tryResendInPlace checks whether the only live match is a pending entry for the
// same role. If so it re-sends the invite and updates that entry in-place (refreshing
// InvitedAt, UpdatedAt, InviteUUID) without changing any other field. Returns (updated,
// true) on the resend path; (nil, false) otherwise so the caller falls through to the
// normal conflict / cleanup path.
func (o *orgSettingsWriterOrchestrator) tryResendInPlace(
	ctx context.Context,
	orgUID, invitedAs, email string,
	matches []model.B2BOrgUser,
	updated *model.B2BOrgSettings,
	now time.Time,
	suppressNotification bool,
) (*model.B2BOrgSettings, bool) {
	// Collect live (non-revoked, non-expired) matches.
	var live []model.B2BOrgUser
	for _, m := range matches {
		s := m.EffectiveStatus()
		if s != model.InviteStatusRevoked && s != model.InviteStatusExpired {
			live = append(live, m)
		}
	}
	// Resend only when the single live match is pending with the same role.
	if len(live) != 1 || live[0].EffectiveStatus() != model.InviteStatusPending || live[0].InvitedAs != invitedAs {
		return nil, false
	}
	// Re-send best-effort (errors are logged and ignored) unless notification is
	// suppressed (CDC passive sync / silent key-contact provisioning must never
	// email). When suppressed, preserve the existing invite UID and only refresh
	// the entry's timestamps in place.
	inviteUID := live[0].InviteUUID
	if !suppressNotification {
		inviteUID = o.sendOrgInvite(ctx, orgUID, email, live[0].Name, invitedAs)
	}
	// Update in-place: only the list that holds the match needs refreshing.
	if invitedAs == model.B2BOrgRoleWriter {
		updated.Writers = refreshPendingEntry(updated.Writers, email, inviteUID, now)
	} else {
		updated.Auditors = refreshPendingEntry(updated.Auditors, email, inviteUID, now)
	}
	return updated, true
}

// sendOrgInvite calls the invite service and returns the invite UID (empty on error).
// Errors are logged and swallowed — invite sending is best-effort.
func (o *orgSettingsWriterOrchestrator) sendOrgInvite(
	ctx context.Context,
	orgUID, email, name, invitedAs string,
) string {
	if o.inviteSender == nil {
		return ""
	}
	role := string(inviteapi.InviteRoleManage)
	if invitedAs == model.B2BOrgRoleAuditor {
		role = string(inviteapi.InviteRoleView)
	}
	orgName := o.fetchOrgName(ctx, orgUID)
	req := inviteapi.SendInviteRequest{
		Recipient: &inviteapi.Recipient{Email: email, Name: name},
		Inviter:   &inviteapi.Inviter{Name: "An organization administrator"},
		Resource:  &inviteapi.Resource{UID: orgUID, Type: "b2b_org", Name: orgName},
		Role:      role,
		OrgName:   orgName,
		ReturnURL: returnURL(o.lfxSelfServeBaseURL),
	}
	result, err := o.inviteSender.SendInvite(ctx, req)
	if err != nil {
		slog.WarnContext(ctx, "org settings invite send failed (best-effort, entry still persisted)",
			"org_uid", orgUID, "email", redaction.RedactEmail(email), "error", err)
		return ""
	}
	return result.InviteUID
}

// fetchOrgName returns the org display name for orgUID, or "" on any error.
// Both sendOrgInvite and notifyRoleAssigned use this for best-effort template data.
func (o *orgSettingsWriterOrchestrator) fetchOrgName(ctx context.Context, orgUID string) string {
	if o.b2bOrgReader == nil {
		return ""
	}
	if org, err := o.b2bOrgReader.GetB2BOrg(ctx, orgUID); err == nil && org != nil {
		return org.Name
	}
	return ""
}

// notifyRoleAssigned sends a role-assignment notification email best-effort.
// Errors are logged and swallowed so a transient email-service outage never
// blocks the primary write. Only called on the existing-LFID path.
// orgName may be pre-resolved by the caller (e.g. from an earlier GetB2BOrg
// guard fetch); when empty, fetchOrgName is called to avoid returning a blank
// org name in the email body without adding a redundant round-trip.
func (o *orgSettingsWriterOrchestrator) notifyRoleAssigned(ctx context.Context, email, role, orgUID, orgName string) {
	if o.roleNotifier == nil {
		return
	}
	if orgName == "" {
		orgName = o.fetchOrgName(ctx, orgUID)
	}
	if err := o.roleNotifier.NotifyRoleAssigned(ctx, port.OrgRoleAssignedNotification{
		RecipientEmail: email,
		OrgName:        orgName,
		Role:           role,
	}); err != nil {
		slog.WarnContext(ctx, "role-assignment email failed (best-effort, grant still persisted)",
			"org_uid", orgUID, "email", redaction.RedactEmail(email), "role", role, "error", err)
	}
}

// returnURL builds the invite ReturnURL from the base URL. Returns an empty string
// when the base is unset so the invite service omits the return link entirely.
func returnURL(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	return base + "/org"
}

// refreshPendingEntry finds the first pending entry matching email in the slice and
// refreshes its InviteUUID, InvitedAt and UpdatedAt. All other fields are preserved.
func refreshPendingEntry(users []model.B2BOrgUser, email, inviteUID string, now time.Time) []model.B2BOrgUser {
	for i, u := range users {
		if normalizeSettingsEmail(u.Email) == email && u.EffectiveStatus() == model.InviteStatusPending {
			users[i].InviteUUID = inviteUID
			users[i].InvitedAt = &now
			users[i].UpdatedAt = now
			return users
		}
	}
	return users
}
