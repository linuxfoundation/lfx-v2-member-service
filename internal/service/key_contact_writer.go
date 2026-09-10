// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
)

// Type aliases expose port interfaces under KC-specific names for use in tests.
type (
	MemberStorageReader = port.MemberReader
	PMReader            = port.ProjectMembershipReader
	PublisherForKC      = port.MemberPublisher
	UserReaderForKC     = port.UserReader
)

// KeyContactCreateInput carries the validated, normalized fields for creating a key contact.
type KeyContactCreateInput struct {
	MembershipUID  string
	FirstName      string
	LastName       string
	Email          string
	Title          *string
	Role           string
	Status         *string
	BoardMember    *bool
	PrimaryContact *bool
	// SendInvite, when true, sends a platform invite (unregistered user) or
	// role-assignment email (registered user). Default false — access is still
	// provisioned silently for registered users.
	SendInvite bool
}

// KeyContactUpdateInput carries the validated, normalized fields for updating a key contact.
// Nil pointer = leave existing unchanged.
type KeyContactUpdateInput struct {
	MembershipUID  string
	UID            string
	FirstName      *string
	LastName       *string
	Email          *string
	Title          *string
	Role           *string
	Status         *string
	BoardMember    *bool
	PrimaryContact *bool
	IfMatch        string // ETag from request; "" = unconditional update
	// SendInvite, when true, sends a platform invite or role-assignment email if
	// the email address changes. Default false — silent provisioning only.
	SendInvite bool
}

// KeyContactDeleteInput carries the parameters for deleting a key contact.
type KeyContactDeleteInput struct {
	MembershipUID string
	UID           string
	IfMatch       string
}

// KeyContactWriter orchestrates Create/Update/Delete for key contacts.
type KeyContactWriter interface {
	Create(ctx context.Context, in KeyContactCreateInput) (*model.KeyContact, error)
	Update(ctx context.Context, in KeyContactUpdateInput) (*model.KeyContact, error)
	Delete(ctx context.Context, in KeyContactDeleteInput) error
}

type keyContactWriterOrchestrator struct {
	storage                 port.MemberReader
	keyContactWriter        port.KeyContactWriter
	projectMembershipReader port.ProjectMembershipReader
	memberPublisher         port.MemberPublisher
	userReader              port.UserReader
	orgSettings             OrgSettingsPrincipalWriter
	grantIndex              port.KeyContactGrantIndex
	// keyContactsByMembership lists sibling key contacts for the
	// Inactive-revoke check via a fresh fetch, not the stale
	// membership-group cache. Nil disables the sibling check.
	keyContactsByMembership port.KeyContactsByMembershipReader
}

// KeyContactWriterOption configures a keyContactWriterOrchestrator.
type KeyContactWriterOption func(*keyContactWriterOrchestrator)

func WithKCStorage(r port.MemberReader) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.storage = r }
}

func WithKCSiblingReader(r port.KeyContactsByMembershipReader) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.keyContactsByMembership = r }
}

func WithKCWriter(w port.KeyContactWriter) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.keyContactWriter = w }
}

func WithKCProjectMembershipReader(r port.ProjectMembershipReader) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.projectMembershipReader = r }
}

func WithKCPublisher(p port.MemberPublisher) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.memberPublisher = p }
}

func WithKCUserReader(r port.UserReader) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.userReader = r }
}

func WithKCOrgSettings(w OrgSettingsPrincipalWriter) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.orgSettings = w }
}

// WithKCGrantIndex wires the durable record of published key_contact FGA grants.
// It makes an API-published grant revocable by a later CDC delete, and gives this
// path a stored username to revoke with when live LFID lookup comes up empty.
func WithKCGrantIndex(i port.KeyContactGrantIndex) KeyContactWriterOption {
	return func(o *keyContactWriterOrchestrator) { o.grantIndex = i }
}

// kcRoleToOrgRole maps a key-contact role string to the B2BOrgSettings role.
// Representative/Voting Contact → writer; all other roles → auditor.
func kcRoleToOrgRole(kcRole string) string {
	if kcRole == constants.RoleNameRepresentativeVotingContact {
		return model.B2BOrgRoleWriter
	}
	return model.B2BOrgRoleAuditor
}

// orgDashboardReady reports whether the orchestrator has the minimum wiring to
// perform any org-dashboard operation for kc: an orgSettings writer plus a
// contact with both an org UID and an email address.
func (o *keyContactWriterOrchestrator) orgDashboardReady(kc *model.KeyContact) bool {
	return o.orgSettings != nil && kc.B2BOrgUID != "" && kc.Email != ""
}

// provisionOrgDashboardAccess grants org-dashboard access for a key contact.
// Registered users (LFID known) are always provisioned silently; the
// role-assignment email is sent only when sendInvite is true. Unregistered
// users get a pending entry + invite only when sendInvite is true; otherwise
// nothing is sent. All errors are best-effort (logged, not returned).
func (o *keyContactWriterOrchestrator) provisionOrgDashboardAccess(ctx context.Context, kc *model.KeyContact, sendInvite bool) {
	if !o.orgDashboardReady(kc) {
		return
	}
	if kc.Username == "" && !sendInvite {
		return // unregistered + no invite requested: record in SF only, no pending entry
	}
	_, err := o.orgSettings.AddPrincipal(ctx, B2BOrgSettingsAddPrincipal{
		OrgUID:               kc.B2BOrgUID,
		Email:                kc.Email,
		InvitedAs:            kcRoleToOrgRole(kc.Role),
		Name:                 kc.Name(),
		SuppressNotification: !sendInvite,
	})
	if err != nil && !pkgerrors.IsConflict(err) {
		slog.WarnContext(ctx, "key contact org-dashboard provision failed (best-effort)",
			"org_uid", kc.B2BOrgUID, "error", err)
	}
}

// remapOrgDashboardRole moves the org-dashboard principal for kc.Email to the
// role derived from kc.Role. NotFound is treated as a no-op — the contact was
// never provisioned (unregistered, send_invite=false). All other errors are
// best-effort (logged, not returned).
func (o *keyContactWriterOrchestrator) remapOrgDashboardRole(ctx context.Context, kc *model.KeyContact) {
	if !o.orgDashboardReady(kc) {
		return
	}
	_, err := o.orgSettings.ChangePrincipalRole(ctx, B2BOrgSettingsChangeRole{
		OrgUID:    kc.B2BOrgUID,
		Email:     kc.Email,
		InvitedAs: kcRoleToOrgRole(kc.Role),
	})
	if err != nil && !pkgerrors.IsNotFound(err) {
		slog.WarnContext(ctx, "key contact org-dashboard role remap failed (best-effort)",
			"org_uid", kc.B2BOrgUID, "error", err)
	}
}

// orgRoleLevel returns a numeric ordering for org-dashboard roles so the
// highest-privilege role among remaining contacts can be computed in O(n).
// writer=1 > auditor=0; any unknown value maps to 0.
func orgRoleLevel(role string) int {
	if role == model.B2BOrgRoleWriter {
		return 1
	}
	return 0
}

// revokeOrDowngradeOrgDashboardRole reconciles org-dashboard access after a
// key contact is removed (delete or email change). It scans all OTHER active
// key contacts for kc.Email in the org and takes one of three actions:
//
//   - No remaining active contacts → RemovePrincipal (full revoke).
//   - Remaining max role < departing role → ChangePrincipalRole to max remaining
//     (downgrade; e.g. Voting Contact deleted while Billing Contact stays active).
//   - Remaining max role ≥ departing role → no-op (access level unchanged).
//
// Fails safe: scan error → skip rather than revoke prematurely.
// ChangePrincipalRole NotFound → swallowed (contact never provisioned).
// ChangePrincipalRole Conflict → swallowed (assertNotRemovingLastAdmin guard; access stays elevated).
func (o *keyContactWriterOrchestrator) revokeOrDowngradeOrgDashboardRole(ctx context.Context, kc *model.KeyContact) {
	if !o.orgDashboardReady(kc) || o.storage == nil {
		return
	}
	reconcileOrgDashboardAccess(ctx, o.orgSettings, o.storage, kc)
}

// orgKeyContactLister lists an org's key contacts for org-dashboard
// reconciliation. Satisfied by port.MemberReader (API path) and any narrower
// reader wired for the same purpose on the CDC path.
type orgKeyContactLister interface {
	ListKeyContactsForOrg(ctx context.Context, orgSFID string) ([]*model.KeyContact, error)
}

// reconcileOrgDashboardAccess reconciles org-dashboard access for a key
// contact that no longer warrants its own provisioning (removed, email
// changed, or turned Inactive). It scans all OTHER live key contacts for
// kc.Email in the org (live: any status other than case-insensitive Inactive,
// matching the key-contact status gate) and takes one of three actions:
//
//   - No remaining live contacts → RemovePrincipal (full revoke).
//   - Remaining max role < departing role → ChangePrincipalRole to max remaining
//     (downgrade; e.g. Voting Contact deleted while Billing Contact stays active).
//   - Remaining max role ≥ departing role → no-op (access level unchanged).
//
// Fails safe: nil inputs or a scan error → skip rather than revoke prematurely.
// ChangePrincipalRole NotFound → swallowed (contact never provisioned).
// ChangePrincipalRole Conflict → swallowed (assertNotRemovingLastAdmin guard; access stays elevated).
// Shared by the API writer orchestrator and the CDC consumer so both paths
// reconcile identically; callers own any cursor/error-propagation semantics,
// since this function itself only logs (best-effort).
func reconcileOrgDashboardAccess(ctx context.Context, orgSettings OrgSettingsPrincipalWriter, lister orgKeyContactLister, kc *model.KeyContact) {
	if orgSettings == nil || lister == nil || kc.B2BOrgUID == "" || kc.Email == "" {
		return
	}
	contacts, err := lister.ListKeyContactsForOrg(ctx, kc.B2BOrgUID)
	if err != nil {
		slog.WarnContext(ctx, "key contact org scan failed; skipping dashboard action (best-effort)",
			"org_uid", kc.B2BOrgUID, "error", err)
		return
	}

	// Compute the highest org-dashboard role held by any OTHER live contact
	// with the same email; live means not case-insensitively Inactive, the
	// same semantics as the key-contact status gate. Empty string means no
	// remaining live contacts.
	maxRemainingRole := ""
	for _, c := range contacts {
		if c.UID == kc.UID || strings.EqualFold(c.Status, constants.RoleStatusInactive) || !strings.EqualFold(c.Email, kc.Email) {
			continue
		}
		r := kcRoleToOrgRole(c.Role)
		if maxRemainingRole == "" || orgRoleLevel(r) > orgRoleLevel(maxRemainingRole) {
			maxRemainingRole = r
		}
	}

	departingRole := kcRoleToOrgRole(kc.Role)

	switch {
	case maxRemainingRole == "":
		// No other active contacts — full revoke.
		if _, err := orgSettings.RemovePrincipal(ctx, B2BOrgSettingsRemovePrincipal{
			OrgUID: kc.B2BOrgUID, Email: kc.Email,
		}); err != nil && !pkgerrors.IsNotFound(err) && !pkgerrors.IsConflict(err) {
			slog.WarnContext(ctx, "key contact org-dashboard revoke failed (best-effort)",
				"org_uid", kc.B2BOrgUID, "error", err,
				"org_dashboard_sync_failed", true)
		}
	case orgRoleLevel(maxRemainingRole) < orgRoleLevel(departingRole):
		// Remaining contacts only warrant a lower role — downgrade.
		// NotFound: contact was never provisioned → no-op.
		// Conflict: last-writer guard fired (org must keep ≥1 admin) → swallow,
		// access stays elevated rather than stranding the org without an admin.
		if _, err := orgSettings.ChangePrincipalRole(ctx, B2BOrgSettingsChangeRole{
			OrgUID: kc.B2BOrgUID, Email: kc.Email, InvitedAs: maxRemainingRole,
		}); err != nil && !pkgerrors.IsNotFound(err) && !pkgerrors.IsConflict(err) {
			slog.WarnContext(ctx, "key contact org-dashboard role downgrade failed (best-effort)",
				"org_uid", kc.B2BOrgUID, "error", err,
				"org_dashboard_sync_failed", true)
		}
		// maxRemainingRole >= departingRole: another contact already holds equal or
		// higher access — current org-settings role is already correct, no change.
	}
}

// NewKeyContactWriter constructs a KeyContactWriter.
func NewKeyContactWriter(opts ...KeyContactWriterOption) KeyContactWriter {
	o := &keyContactWriterOrchestrator{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Create creates a new key contact. Self-heals idempotent re-creates by returning
// an existing record when the same role+email is already active for the membership.
func (o *keyContactWriterOrchestrator) Create(ctx context.Context, in KeyContactCreateInput) (*model.KeyContact, error) {
	existing, err := o.normalizeAndValidateCreate(ctx, &in)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	pm, _, err := o.projectMembershipReader.AssembleProjectMembership(ctx, in.MembershipUID)
	if err != nil {
		return nil, err
	}

	input := model.KeyContactInput{
		Email:          &in.Email,
		FirstName:      in.FirstName,
		LastName:       in.LastName,
		Title:          derefPtrStr(in.Title),
		MembershipUID:  in.MembershipUID,
		ProjectUID:     pm.ProjectUID,
		AccountSFID:    pm.B2BOrgUID,
		Role:           &in.Role,
		Status:         in.Status,
		BoardMember:    in.BoardMember,
		PrimaryContact: in.PrimaryContact,
	}

	kc, err := o.keyContactWriter.CreateKeyContact(ctx, input)
	if err != nil {
		return nil, err
	}

	// PM FGA update_access first so the parent tuple exists before the key_contact put.
	pmMsg := BuildProjectMembershipFGAMessage(pm)
	if pubErr := o.memberPublisher.Access(ctx, constants.FGASyncUpdateAccessSubject, pmMsg); pubErr != nil {
		slog.WarnContext(ctx, "project membership FGA publish failed on key contact create",
			"membership_uid", pm.UID, "error", pubErr, "publish_failed_for_backfill_repair", true)
	}

	// Resolve username, publish indexer, then FGA put.
	var definitiveMiss bool
	kc.Username, definitiveMiss = o.resolveUsernameForContact(ctx, "", kc.Email)
	PublishKeyContactIndexer(ctx, o.memberPublisher, kc, indexerConstants.ActionCreated)
	lister := siblingListerFor(o.keyContactsByMembership, o.userReader)
	PublishKeyContactFGA(ctx, o.memberPublisher, o.grantIndex, kc, lister, lister)
	if definitiveMiss {
		// The email never resolved to a registered account. There is nothing
		// to revoke on a brand-new contact — no grant was ever published for
		// it — but the index may still hold a stale entry from a prior,
		// now-superseded contact at this membership+email pair. Best-effort:
		// this new contact was created successfully regardless of this cleanup.
		// Deliberate: a failed revoke self-heals on the record's next CDC
		// touch, where processKeyContact holds the replay cursor until done.
		_ = revokeKeyContactGrantIfNoLongerLive(ctx, o.memberPublisher, o.grantIndex, lister, lister, kc.UID, kc.Username, "", reasonEmailUnregistered)
	}
	// Status: coalesce to the input value since the mock echoes "" for a
	// status it does not persist. An Inactive create must never provision.
	status := kc.Status
	if status == "" {
		status = derefPtrStr(in.Status)
	}
	if !strings.EqualFold(status, constants.RoleStatusInactive) {
		o.provisionOrgDashboardAccess(ctx, kc, in.SendInvite)
	}

	return kc, nil
}

// Update updates a key contact. Returns the current record unchanged on true no-op
// (no input fields set). Paired FGA publish: put new sub before remove old sub on email change.
func (o *keyContactWriterOrchestrator) Update(ctx context.Context, in KeyContactUpdateInput) (*model.KeyContact, error) {
	current, err := o.storage.GetKeyContact(ctx, in.UID)
	if err != nil {
		return nil, err
	}

	if in.IfMatch != "" {
		currentETag, etagErr := etag.LFXEtag(current)
		if etagErr != nil {
			return nil, pkgerrors.NewUnexpected("failed to compute etag for key contact", etagErr)
		}
		if currentETag != in.IfMatch {
			return nil, pkgerrors.NewPreconditionFailed("key contact has been modified since last read — refresh and retry")
		}
	}

	if !hasAnyKCChange(in) {
		return current, nil
	}

	if err := o.normalizeAndValidateUpdate(ctx, current, &in); err != nil {
		return nil, err
	}

	emailChanging := in.Email != nil && !strings.EqualFold(*in.Email, current.Email)

	input := model.KeyContactInput{
		Email:          in.Email,
		FirstName:      derefOrStr(in.FirstName, current.FirstName),
		LastName:       derefOrStr(in.LastName, current.LastName),
		Title:          derefPtrStr(in.Title),
		Role:           in.Role,
		Status:         in.Status,
		BoardMember:    in.BoardMember,
		PrimaryContact: in.PrimaryContact,
		MembershipUID:  in.MembershipUID,
		AccountSFID:    current.B2BOrgUID,
	}
	if in.IfMatch != "" {
		input.IfUnmodifiedSince = current.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	}

	newKC, err := o.keyContactWriter.UpdateKeyContact(ctx, in.UID, input)
	if err != nil {
		return nil, err
	}

	if emailChanging {
		lister := siblingListerFor(o.keyContactsByMembership, o.userReader)
		// Paired FGA: put new username first (avoid no-access window), then remove old.
		newKC.Username, _ = o.resolveUsernameForContact(ctx, "", newKC.Email)
		PublishKeyContactFGA(ctx, o.memberPublisher, o.grantIndex, newKC, lister, lister)
		// This revoke is intentionally unconditional on the index, even though
		// PublishKeyContactFGA's recordKeyContactGrant also revokes a superseded
		// grant when the index already holds one — a duplicate member_remove is
		// idempotent, but skipping this one is not safe. The index-driven revoke
		// only fires when idx.Get finds a stored entry for this contact; it does
		// nothing on a cold index (pre-backfill, or any prior write that failed),
		// when the new email resolves to no LFID (PublishKeyContactFGA returns
		// before ever reading the index), or when the put publish or the index
		// read itself fails. This path is the only one that resolves the old
		// grant from live data (current.Username/current.Email) rather than from
		// the index, so it is also the only one that reproduces the legacy
		// auth0|-prefix fallback in resolveUsernameForContact — it must run
		// regardless of whether the index-driven path also ran. It is still
		// skipped below when a live sibling on the same membership still
		// holds the old email, since that pair remains justified.
		oldUsername, oldMiss := o.resolveUsernameForContact(ctx, current.Username, current.Email)
		if oldUsername != newKC.Username {
			// On a definitive miss current.Email belongs to no account, so it
			// cannot serve as direct proof that a sibling justifies oldUsername.
			directEmail := current.Email
			if oldMiss {
				directEmail = ""
			}
			// Failures are logged by the choke point but not propagated: the SF
			// update already succeeded, and this path accepts unflushed loss.
			revokeKeyContactPairIfUnjustified(ctx, o.memberPublisher, lister, keyContactPairRevoke{
				membershipUID: newKC.MembershipUID,
				username:      oldUsername,
				excludeUID:    newKC.UID,
				email:         directEmail,
				reason:        "email changed",
				flush:         false,
				recheck:       lister,
			})
		}
		// Role/Status: nil means no change, coalesce to the current value since
		// the mock can't re-fetch from SF and returns "" for unchanged fields.
		newKC.Role = derefOrStr(in.Role, current.Role)
		newKC.Status = derefOrStr(in.Status, current.Status)
		if strings.EqualFold(newKC.Status, constants.RoleStatusInactive) {
			// The new email never had a principal provisioned for it, nothing
			// more to do there. The old email is reconciled below regardless.
		} else {
			o.provisionOrgDashboardAccess(ctx, newKC, in.SendInvite)
		}
		o.revokeOrDowngradeOrgDashboardRole(ctx, current)
	} else {
		// Email/Role/Status: nil input means unchanged, mock returns "" for nil fields; coalesce.
		if newKC.Email == "" {
			newKC.Email = current.Email
		}
		newKC.Role = derefOrStr(in.Role, current.Role)
		newKC.Status = derefOrStr(in.Status, current.Status)
		lister := siblingListerFor(o.keyContactsByMembership, o.userReader)
		var definitiveMiss bool
		newKC.Username, definitiveMiss = o.resolveUsernameForContact(ctx, current.Username, newKC.Email)
		if definitiveMiss {
			// The email no longer resolves to any registered account (e.g. a
			// rename or deregistration since the last successful grant).
			// newKC.Username here is only the stripped legacy auth0| fallback
			// (see resolveUsernameForContact) — publishing it would reassert FGA
			// access for an account just confirmed unregistered. Skip the put
			// and revoke any grant still recorded for this contact instead.
			// Not propagated, matching the paired-FGA revoke above: the SF
			// update already succeeded, and this path accepts unflushed loss.
			// Deliberate: a failed revoke self-heals on the record's next CDC
			// touch, where processKeyContact holds the replay cursor until done.
			_ = revokeKeyContactGrantIfNoLongerLive(ctx, o.memberPublisher, o.grantIndex, lister, lister, newKC.UID, newKC.Username, "", reasonEmailUnregistered)
		} else {
			PublishKeyContactFGA(ctx, o.memberPublisher, o.grantIndex, newKC, lister, lister)
		}

		becomingInactive := strings.EqualFold(newKC.Status, constants.RoleStatusInactive) &&
			!strings.EqualFold(current.Status, constants.RoleStatusInactive)
		becomingActive := !strings.EqualFold(newKC.Status, constants.RoleStatusInactive) &&
			strings.EqualFold(current.Status, constants.RoleStatusInactive)
		switch {
		case becomingInactive:
			// The tuple was just withdrawn; reconcile using remaining active siblings.
			o.revokeOrDowngradeOrgDashboardRole(ctx, newKC)
		case strings.EqualFold(newKC.Status, constants.RoleStatusInactive):
			// Already Inactive: no principal exists to remap.
		case becomingActive:
			// Reactivation: restore the principal removed at deactivation. On
			// Conflict (principal survived, maybe downgraded) the remap below
			// raises it back; a remap alone would swallow NotFound.
			o.provisionOrgDashboardAccess(ctx, newKC, in.SendInvite)
			o.remapOrgDashboardRole(ctx, newKC)
		case in.Role != nil && *in.Role != current.Role:
			o.remapOrgDashboardRole(ctx, newKC)
		}
	}
	PublishKeyContactIndexer(ctx, o.memberPublisher, newKC, indexerConstants.ActionUpdated)

	return newKC, nil
}

// Delete deletes a key contact. Indexer delete is swallowed; FGA remove is propagated.
//
// Missing and cross-membership paths return 404 with no publish. Without a
// fetched source record we cannot prove the requested UID is globally absent;
// tombstoning by path params could delete a real document owned elsewhere.
func (o *keyContactWriterOrchestrator) Delete(ctx context.Context, in KeyContactDeleteInput) error {
	kc, err := o.storage.GetKeyContact(ctx, in.UID)
	if err != nil {
		return err
	}

	if kc.MembershipUID != in.MembershipUID {
		// The contact exists under another membership; tombstoning by UID here
		// would delete the real indexed document. Return generic 404 only — do not
		// leak that the UID exists elsewhere.
		slog.InfoContext(ctx, "key contact membership mismatch on delete — returning 404",
			"uid", in.UID, "path_membership_uid", in.MembershipUID, "owner_membership_uid", kc.MembershipUID)
		return pkgerrors.NewNotFound("key contact not found")
	}

	if in.IfMatch != "" {
		currentETag, etagErr := etag.LFXEtag(kc)
		if etagErr != nil {
			return pkgerrors.NewUnexpected("failed to compute etag for key contact", etagErr)
		}
		if currentETag != in.IfMatch {
			return pkgerrors.NewPreconditionFailed("key contact has been modified since last read — refresh and retry")
		}
	}

	// Grant-index settlement for a pair OTHER than this record's own must run
	// before the Salesforce delete below: it can fail into a state (a failed
	// durable-address transfer) that must not be reported as done, and once
	// the Salesforce delete has run the record is gone, so a client retry of
	// this same call could never re-attempt it. Resolving the settlement here,
	// pre-delete, keeps the whole operation retriable as one idempotent unit:
	// revokes are sibling-checked and durable-address transfers/reasserts are
	// themselves idempotent, so re-running this block on retry is safe.
	resolved, _ := o.resolveUsernameForContact(ctx, kc.Username, kc.Email)
	username := resolved

	// Read the recorded grant once: it supplies a username fallback when live
	// lookup comes up empty, and its revision (when the read succeeds) is what
	// the CAS delete below is conditioned on, so a grant written concurrently
	// (e.g. a re-invite racing this delete) is never silently tombstoned.
	grant, grantFound, grantErr := o.getGrant(ctx, in.UID)
	if grantErr != nil {
		slog.WarnContext(ctx, "key contact grant index read failed on delete — will not clear the index entry",
			"uid", in.UID, "error", grantErr)
	} else if username == "" && grantFound {
		// Live lookup can come up empty for a contact that was granted access
		// earlier (auth-service unavailable, or the account since renamed or
		// removed). The grant index still holds the username the grant was made
		// to, and revoking with it beats skipping the revoke silently.
		username = grant.Username
	}

	// The index can describe a different pair than the one about to be revoked
	// below when an earlier recordKeyContactGrant Put failed (e.g. mid
	// Salesforce reparent/rename): the replacement's member_put succeeded and
	// is what kc.MembershipUID/username now describe, but the swallowed Put
	// failure left the index still pointing at the old pair, whose own
	// member_put was never revoked. Revoke that indexed pair too before the
	// index entry is cleared below; a failed publish returns early instead,
	// so the entry, the only remaining record that the old pair's grant was
	// ever made, survives for the client retry. The record still exists in
	// Salesforce at this point, but the sibling scan excludes it by UID either
	// way, so the answer is identical to running this after the delete.
	lister := siblingListerFor(o.keyContactsByMembership, o.userReader)
	if grantErr == nil && grantFound && (grant.MembershipUID != kc.MembershipUID || grant.Username != username) {
		// The stored pair's email is unknown here: justification runs by
		// resolution alone. A failed or uncertain revoke preserves the entry.
		// flush:true so delivery is confirmed independently of the main pair's
		// own flush below, which can now exit via revokeUnneeded without ever
		// flushing this one.
		staleOutcome, staleJustifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, o.memberPublisher, lister, keyContactPairRevoke{
			membershipUID: grant.MembershipUID,
			username:      grant.Username,
			excludeUID:    in.UID,
			reason:        "key contact deleted (stale indexed pair)",
			flush:         true,
			recheck:       lister,
		})
		if revokeErr != nil {
			// The stale pair is unsettled and its only address is the entry
			// keyed by this UID. Fail before the Salesforce delete so the
			// client retry re-attempts this settlement.
			return pkgerrors.NewUnexpected("failed to revoke stale indexed pair for key contact: retry the delete", revokeErr)
		} else if staleOutcome == revokeUnneeded && staleJustifiedBy != nil && o.grantIndex != nil &&
			!pairDurablyOwned(ctx, o.grantIndex, staleJustifiedBy, grant.MembershipUID, grant.Username) {
			// A live sibling justifies the stale pair, but a failed transfer
			// leaves the stale entry, keyed by this UID, as the pair's only
			// durable address: nothing will ever revisit it once the record is
			// deleted below. Fail before the Salesforce delete runs, so a
			// client retry re-attempts this same settlement rather than
			// finding the record already gone.
			slog.ErrorContext(ctx, "key contact deleted but stale indexed pair durable revoke address transfer failed",
				"uid", in.UID, "membership_uid", grant.MembershipUID, "manual_recovery_required", true)
			return pkgerrors.NewUnexpected("key contact durable revoke address transfer failed for stale indexed pair: retry the delete", nil)
		}
	}

	if err := o.keyContactWriter.DeleteKeyContact(ctx, in.UID, kc.MembershipUID); err != nil {
		return err
	}

	// Indexer delete: swallow (reindexable via /admin/reindex).
	PublishKeyContactIndexer(ctx, o.memberPublisher, kc, indexerConstants.ActionDeleted)

	// FGA remove: propagate a publication failure, since dangling permissions are not
	// auto-repairable. Publication succeeding is not revocation succeeding; it
	// only means fga-sync has been told, and it converges asynchronously.
	//
	// This revoke, for the record's OWN pair, must stay after the Salesforce
	// delete above: revoking it first and then failing the delete would strip
	// access the record still legitimately holds.

	// Org-dashboard revoke is best-effort; run it regardless of FGA outcome so a
	// failed FGA publish does not leave a stale writer/auditor entry.
	o.revokeOrDowngradeOrgDashboardRole(ctx, kc)

	// The choke point flushes here so a crash cannot discard a revocation this
	// call has already reported as done: that confirms the server received the
	// message, not that OpenFGA converged.
	// email only justifies a sibling directly when username came from live
	// resolution, i.e. it is the live identity's own pair. A username copied
	// from grant.Username after live resolution came up empty must not let a
	// same-email sibling with a different, now-unregistered LFID justify it.
	pairEmail := ""
	if resolved != "" {
		pairEmail = kc.Email
	}
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, o.memberPublisher, lister, keyContactPairRevoke{
		membershipUID: kc.MembershipUID,
		username:      username,
		excludeUID:    in.UID,
		email:         pairEmail,
		reason:        "key contact deleted",
		flush:         true,
		recheck:       lister,
	})
	switch outcome {
	case revokeFailed, revokeUncertain:
		// The Salesforce record is already gone, so the index entry recorded
		// here is the only future retry address for this pair. Record it
		// before returning the error so a later CDC delete event can still
		// address the revoke.
		if o.grantIndex != nil && username != "" {
			skipRecord := false
			if grantErr == nil && grantFound && grant.PendingRevoke != nil &&
				(grant.MembershipUID != kc.MembershipUID || grant.Username != username) {
				// recordKeyContactGrant would supersede the stale live pair into
				// the single marker slot, dropping this existing marker's pair
				// unaddressed. Drain it first so its revoke address survives.
				marker := *grant.PendingRevoke
				if drainErr := drainKeyContactPendingRevoke(ctx, o.memberPublisher, o.grantIndex, lister, lister, in.UID,
					marker, "key contact deleted (pending revoke marker before recovery record)"); drainErr != nil {
					skipRecord = true
					slog.ErrorContext(ctx, "key contact grant index record skipped: pending revoke marker could not be drained, no durable retry address for this pair",
						"uid", in.UID, "membership_uid", kc.MembershipUID, "error", drainErr, "manual_recovery_required", true)
					revokeErr = errors.Join(revokeErr, drainErr)
				} else if clearErr := clearPendingRevoke(ctx, o.grantIndex, in.UID, marker); clearErr != nil {
					// The drain was confirmed delivered; this is bookkeeping only.
					slog.WarnContext(ctx, "key contact grant index pending-revoke marker clear failed after confirmed drain",
						"uid", in.UID, "error", clearErr)
				}
			}
			if !skipRecord {
				if recordErr := recordKeyContactGrant(ctx, o.memberPublisher, o.grantIndex, lister, lister, in.UID, kc.MembershipUID, username); recordErr != nil {
					slog.ErrorContext(ctx, "key contact grant index record failed after revoke error: no durable retry address for this pair",
						"uid", in.UID, "membership_uid", kc.MembershipUID, "error", recordErr, "manual_recovery_required", true)
				}
			}
		}
		if outcome == revokeFailed {
			return pkgerrors.NewUnexpected("failed to publish FGA revocation for deleted key contact", revokeErr)
		}
		// The scan could not prove the pair unjustified. The record is already
		// deleted, so report the revoke failure rather than a false success.
		return pkgerrors.NewUnexpected("sibling scan inconclusive for deleted key contact: revocation not published", revokeErr)
	}

	// A live sibling justified the pair: this entry may be the pair's only
	// durable address. A failed transfer here means the retained entry is
	// keyed by the just-deleted UID, which nothing will ever revisit: that
	// must fail the delete, not just skip the index clear.
	if outcome == revokeUnneeded && justifiedBy != nil && o.grantIndex != nil &&
		!pairDurablyOwned(ctx, o.grantIndex, justifiedBy, kc.MembershipUID, username) {
		slog.ErrorContext(ctx, "key contact deleted but durable revoke address transfer failed",
			"uid", in.UID, "membership_uid", kc.MembershipUID, "manual_recovery_required", true)
		return pkgerrors.NewUnexpected("key contact deleted but durable revoke address transfer failed: retry the delete", nil)
	}

	// A PendingRevoke marker for a superseded pair, unrelated to the live pair
	// just settled above, must be drained before the entry is erased outright:
	// the CDC delete path drains markers the same way, but once this Delete
	// clears the whole entry there is no longer an address for the marker's
	// pair, and the later CDC delete would see a genuine miss.
	if grantErr == nil && grantFound && grant.PendingRevoke != nil {
		if drainErr := drainKeyContactPendingRevoke(ctx, o.memberPublisher, o.grantIndex, lister, lister, in.UID,
			*grant.PendingRevoke, "key contact deleted (pending revoke marker)"); drainErr != nil {
			// Preserve the entry: clear the live pair but keep the marker as
			// the only remaining address for its still-unconfirmed revoke.
			clearRevokedGrant(ctx, o.grantIndex, in.UID, grant)
			return pkgerrors.NewUnexpected("failed to drain pending revoke marker for deleted key contact", drainErr)
		}
	}

	// Clear the recorded grant now that the revoke is confirmed delivered or
	// proven unnecessary. This runs whether or not a member_remove was
	// published: when no username could be resolved from either source there is
	// nothing to revoke, and leaving the entry behind would orphan it
	// permanently — the contact is gone, so nothing will ever revisit it. It
	// also runs when a live sibling still justifies the pair, so a later delete
	// of that sibling cannot be blocked by this record's stale entry.
	// Conditioned on the revision read above, so a grant written concurrently
	// is preserved rather than deleted out from under its writer. The error
	// paths above (grant read, uncertain scan, failed live or stale revoke)
	// all deliberately return or skip before this point, keeping the entry as
	// the only record of a grant still needing follow-up. Any PendingRevoke
	// marker was already drained above, so clearing the whole entry here is
	// safe.
	if grantErr == nil && o.grantIndex != nil {
		if err := o.grantIndex.Delete(ctx, in.UID, grant.Revision); err != nil {
			slog.WarnContext(ctx, "key contact grant index cleanup failed after delete",
				"uid", in.UID, "error", err)
		}
	}

	return nil
}

const legacyAuth0UsernamePrefix = "auth0|"

// resolveUsernameForContact returns the resolved username and whether the
// lookup produced a definitive "no registered account" miss (as opposed to a
// transport-level failure, which is not evidence the email is unregistered
// and must not trigger a grant revoke).
func (o *keyContactWriterOrchestrator) resolveUsernameForContact(ctx context.Context, currentUsername, email string) (username string, definitiveMiss bool) {
	if currentUsername != "" && !strings.HasPrefix(currentUsername, legacyAuth0UsernamePrefix) {
		return currentUsername, false
	}
	if email != "" {
		resolved, err := o.userReader.UsernameByEmail(ctx, email)
		if err != nil {
			if pkgerrors.IsNotFound(err) {
				definitiveMiss = true
			} else {
				slog.WarnContext(ctx, "failed to resolve LFID username for key contact FGA",
					"email", email, "error", err)
			}
		} else if resolved != "" {
			return resolved, false
		}
	}
	// Fallback when lookup is unavailable: strip the legacy auth0| prefix from safe-slug identifiers.
	if strings.HasPrefix(currentUsername, legacyAuth0UsernamePrefix) {
		return strings.TrimPrefix(currentUsername, legacyAuth0UsernamePrefix), definitiveMiss
	}
	return "", definitiveMiss
}

// getGrant returns the grant recorded for uid. found reports whether an entry
// exists; err is non-nil only on a genuine read failure, distinct from a miss,
// so callers can tell "nothing to revoke/clear" from "we don't actually know."
// A nil grantIndex (mock mode) reports found=false, err=nil — unwired, not a
// failure.
func (o *keyContactWriterOrchestrator) getGrant(ctx context.Context, uid string) (port.KeyContactGrant, bool, error) {
	if o.grantIndex == nil {
		return port.KeyContactGrant{}, false, nil
	}
	return o.grantIndex.Get(ctx, uid)
}

func hasAnyKCChange(in KeyContactUpdateInput) bool {
	return in.Email != nil || in.Role != nil || in.Status != nil ||
		in.BoardMember != nil || in.PrimaryContact != nil ||
		in.Title != nil || in.FirstName != nil || in.LastName != nil
}

func derefPtrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefOrStr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}
