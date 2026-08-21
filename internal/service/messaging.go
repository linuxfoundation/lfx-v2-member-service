// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// messaging.go contains pure transforms from domain types to NATS wire format,
// plus thin Publish* wrappers. Functions here take *model.X inputs and produce
// ready-to-publish messages (or invoke port.MemberPublisher). No port reads,
// no orchestration, no state. Keep this file dependency-free of orchestrator
// types so the builders stay safe to call from any layer.

package service

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	indexerTypes "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/types"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool { return &b }

// b2bOrgNonParentRelations lists relations excluded when updating only an org's
// own parent reference. Prevents the update from wiping global_org_admin,
// auditor, writer, owner, membership, or child tuples set by other code paths.
var b2bOrgNonParentRelations = []string{
	"global_org_admin", "auditor", "writer", "owner", "membership", "child",
}

// b2bOrgNonChildRelations lists relations excluded when updating only a parent
// org's child list. Mirrors b2bOrgNonParentRelations but protects the parent
// relation instead of child.
var b2bOrgNonChildRelations = []string{
	"global_org_admin", "auditor", "writer", "owner", "membership", "parent",
}

// orgNameAndAliases builds the name+domain alias slice for an org indexing config.
func orgNameAndAliases(org *model.B2BOrg) []string {
	var out []string
	if org.Name != "" {
		out = append(out, org.Name)
	}
	if org.PrimaryDomain != "" {
		out = append(out, org.PrimaryDomain)
	}
	return append(out, org.DomainAliases...)
}

// BuildB2BOrgIndexingConfig constructs an IndexingConfig for a B2BOrg document.
func BuildB2BOrgIndexingConfig(org *model.B2BOrg) *indexerTypes.IndexingConfig {
	nameAndAliases := orgNameAndAliases(org)

	var fulltext []string
	for _, s := range []string{org.Name, org.PrimaryDomain, org.Description, org.Industry, org.Sector} {
		if s != "" {
			fulltext = append(fulltext, s)
		}
	}

	var parentRefs []string
	if org.ParentUID != "" {
		parentRefs = append(parentRefs, "b2b_org:"+org.ParentUID)
	}

	return &indexerTypes.IndexingConfig{
		Public:               boolPtr(false),
		ObjectID:             org.UID,
		AccessCheckObject:    "b2b_org:" + org.UID,
		AccessCheckRelation:  fgaconstants.RelationAuditor,
		HistoryCheckObject:   "b2b_org:" + org.UID,
		HistoryCheckRelation: fgaconstants.RelationAuditor,
		SortName:             strings.ToLower(org.Name),
		NameAndAliases:       nameAndAliases,
		ParentRefs:           parentRefs,
		Fulltext:             strings.Join(fulltext, " "),
		Tags:                 org.Tags(),
	}
}

// BuildB2BOrgSettingsIndexingConfig constructs an IndexingConfig for a B2BOrgSettings document.
// ObjectID equals the parent org UID so a single point-lookup retrieves both org and settings docs
// (callers filter by object_type). Access-check resolves against the parent b2b_org — settings
// do not have a separate FGA type.
// Public is explicitly false — settings docs are never world-readable. Spelled out here so future
// readers don't adopt committee-service's &parent.Public pattern by mistake.
// HistoryCheckRelation is writer (not auditor) — history audits are a write-side concern;
// matches project-service precedent.
func BuildB2BOrgSettingsIndexingConfig(org *model.B2BOrg, settings *model.B2BOrgSettings) *indexerTypes.IndexingConfig {
	nameAndAliases := orgNameAndAliases(org)

	parentRefs := []string{"b2b_org:" + org.UID}
	if org.ParentUID != "" {
		parentRefs = append(parentRefs, "b2b_org:"+org.ParentUID)
	}

	return &indexerTypes.IndexingConfig{
		Public:               boolPtr(false),
		ObjectID:             org.UID,
		AccessCheckObject:    "b2b_org:" + org.UID,
		AccessCheckRelation:  fgaconstants.RelationAuditor,
		HistoryCheckObject:   "b2b_org:" + org.UID,
		HistoryCheckRelation: fgaconstants.RelationWriter,
		SortName:             strings.ToLower(org.Name),
		NameAndAliases:       nameAndAliases,
		ParentRefs:           parentRefs,
		Fulltext:             strings.Join(settings.FulltextTokens(), " "),
		Tags:                 settings.Tags(),
	}
}

// teamMemberRefs renders team names as fully-qualified FGA userset subjects.
// Blank and whitespace-only names are dropped rather than rendered as
// "team:#member", which OpenFGA would reject. Named for the shape it renders,
// not for one relation, because both the auditor teams and the global-admin
// team need the same subject form.
func teamMemberRefs(teams ...string) []string {
	refs := make([]string, 0, len(teams))
	for _, name := range teams {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			refs = append(refs, "team:"+trimmed+"#member")
		}
	}
	return refs
}

// B2BOrgFGARefs carries the access-control inputs for a b2b_org FGA message.
//
// This is a struct rather than positional parameters because the four slices
// have three *different* nil semantics, and transposing two of them compiles
// cleanly while silently changing what gets revoked:
//
//   - GlobalOrgAdminTeamName: set on create to grant the LF global-admin team; empty on updates.
//   - AuditorTeams: LF team names granted blanket auditor access on this org, emitted as
//     References["auditor"] entries. Independent of Auditors below: these are team
//     subjects, those are per-user LFIDs, and the two coexist in one message. Empty
//     omits the reference.
//   - Writers, Auditors: LFID usernames of accepted principals from OrgSettings.
//     nil = caller is not managing this relation → existing tuples are *preserved*.
//     Non-nil, even empty = caller explicitly replaces → the full-sync runs and revokes.
//   - MembershipUIDs: UIDs of project_memberships owned by this org. When non-empty,
//     References["membership"] is populated. When empty or nil, "membership" is added
//     to ExcludeRelations so existing membership tuples are not accidentally wiped.
//
// The zero value is the common case — no team grants, no relation managed — so
// most callers can pass B2BOrgFGARefs{} and name only the fields they mean.
type B2BOrgFGARefs struct {
	GlobalOrgAdminTeamName string
	AuditorTeams           []string
	Writers                []string
	Auditors               []string
	MembershipUIDs         []string
}

// BuildB2BOrgFGAMessage constructs a GenericFGAMessage for a B2BOrg access-control
// update.
//
// parent and child tuples are always excluded — managed by BuildB2BOrgReparentingMessages.
func BuildB2BOrgFGAMessage(org *model.B2BOrg, in B2BOrgFGARefs) fgatypes.GenericFGAMessage {
	adminTeamName := strings.TrimSpace(in.GlobalOrgAdminTeamName)

	refs := make(map[string][]string)
	if adminRefs := teamMemberRefs(adminTeamName); len(adminRefs) > 0 {
		refs["global_org_admin"] = adminRefs
	}
	if teamRefs := teamMemberRefs(in.AuditorTeams...); len(teamRefs) > 0 {
		refs["auditor"] = teamRefs
	}
	if len(in.MembershipUIDs) > 0 {
		mRefs := make([]string, len(in.MembershipUIDs))
		for i, uid := range in.MembershipUIDs {
			mRefs[i] = "project_membership:" + uid
		}
		refs["membership"] = mRefs
	}

	relations := make(map[string][]string)
	if len(in.Writers) > 0 {
		relations["writer"] = in.Writers
	}
	if len(in.Auditors) > 0 {
		relations["auditor"] = in.Auditors
	}

	excludes := []string{"parent", "child"}
	if adminTeamName == "" {
		excludes = append(excludes, "global_org_admin")
	}
	if len(in.MembershipUIDs) == 0 {
		excludes = append(excludes, "membership")
	}
	// nil = caller is not managing this relation → preserve existing tuples.
	// non-nil (even empty) = caller explicitly replaces → let full-sync run.
	if in.Writers == nil {
		excludes = append(excludes, "writer")
	}
	if in.Auditors == nil {
		// Excluding "auditor" here while refs["auditor"] carries the team
		// subjects is deliberate, not a contradiction: fga-sync applies
		// ExcludeRelations only in its delete branch, so the exclusion
		// suppresses reaping of the per-user auditor tuples this caller is not
		// managing, while the team references are still written.
		excludes = append(excludes, "auditor")
	}

	return fgatypes.GenericFGAMessage{
		ObjectType: "b2b_org",
		Operation:  "update_access",
		Data: fgatypes.GenericAccessData{
			UID:              org.UID,
			Relations:        relations,
			References:       refs,
			ExcludeRelations: excludes,
		},
	}
}

// buildProjectMembershipFGAMessage constructs a GenericFGAMessage for a
// ProjectMembership access-control update.
func buildProjectMembershipFGAMessage(pm *model.ProjectMembership, preserveMissingRefs bool) fgatypes.GenericFGAMessage {
	refs := make(map[string][]string)
	excludes := []string{"key_contact"}
	if pm.B2BOrgUID != "" {
		refs["b2b_org"] = []string{"b2b_org:" + pm.B2BOrgUID}
	} else if preserveMissingRefs {
		excludes = append(excludes, "b2b_org")
	}
	if pm.ProjectUID != "" {
		refs["project"] = []string{"project:" + pm.ProjectUID}
	} else if preserveMissingRefs {
		excludes = append(excludes, "project")
	}

	return fgatypes.GenericFGAMessage{
		ObjectType: "project_membership",
		Operation:  "update_access",
		Data: fgatypes.GenericAccessData{
			UID:              pm.UID,
			References:       refs,
			ExcludeRelations: excludes,
		},
	}
}

// BuildProjectMembershipFGAMessage constructs the authoritative project_membership
// full-sync message for update_access.
func BuildProjectMembershipFGAMessage(pm *model.ProjectMembership) fgatypes.GenericFGAMessage {
	return buildProjectMembershipFGAMessage(pm, false)
}

// BuildProjectMembershipFGAMessagePreserveMissingRefs constructs an
// update_access message that preserves missing parent refs instead of clearing
// them. Used when ProjectUID resolution failed transiently and callers still
// need to reconcile other relations without wiping existing tuples.
func BuildProjectMembershipFGAMessagePreserveMissingRefs(pm *model.ProjectMembership) fgatypes.GenericFGAMessage {
	return buildProjectMembershipFGAMessage(pm, true)
}

// buildDeleteAccessMessage constructs a GenericFGAMessage that withdraws every
// tuple fga-sync manages for the given object.
//
// Unlike update_access, this carries no relations, references, or exclusions —
// the UID alone is the whole instruction, because there is no partial delete.
// That is exactly why it must never be sent for an object that still exists:
// there is no field with which to scope it down.
func buildDeleteAccessMessage(objectType, uid string) fgatypes.GenericFGAMessage {
	return fgatypes.GenericFGAMessage{
		ObjectType: objectType,
		Operation:  "delete_access",
		Data:       fgatypes.GenericDeleteData{UID: uid},
	}
}

// BuildB2BOrgDeleteAccessMessage constructs the FGA message that withdraws a
// deleted organization's tuples. Only valid for an org genuinely deleted in
// Salesforce — see the caller in cdc_consumer.go.
func BuildB2BOrgDeleteAccessMessage(uid string) fgatypes.GenericFGAMessage {
	return buildDeleteAccessMessage("b2b_org", uid)
}

// BuildProjectMembershipDeleteAccessMessage constructs the FGA message that
// withdraws a deleted membership's tuples. Only valid for a membership
// genuinely deleted in Salesforce — see the caller in cdc_consumer.go.
func BuildProjectMembershipDeleteAccessMessage(uid string) fgatypes.GenericFGAMessage {
	return buildDeleteAccessMessage("project_membership", uid)
}

// BuildProjectMembershipIndexingConfig constructs an IndexingConfig for a
// ProjectMembership document.
func BuildProjectMembershipIndexingConfig(pm *model.ProjectMembership) *indexerTypes.IndexingConfig {
	var parentRefs []string
	if pm.B2BOrgUID != "" {
		parentRefs = append(parentRefs, "b2b_org:"+pm.B2BOrgUID)
	}
	if pm.ProjectUID != "" {
		parentRefs = append(parentRefs, "project:"+pm.ProjectUID)
	}

	nameAndAliases := []string{pm.CompanyName}
	if pm.CompanyDomain != "" {
		nameAndAliases = append(nameAndAliases, pm.CompanyDomain)
	}

	var fulltext []string
	for _, s := range []string{pm.CompanyName, pm.TierName, pm.Status, pm.Year} {
		if s != "" {
			fulltext = append(fulltext, s)
		}
	}

	return &indexerTypes.IndexingConfig{
		Public:               boolPtr(false),
		ObjectID:             pm.UID,
		AccessCheckObject:    "project_membership:" + pm.UID,
		AccessCheckRelation:  fgaconstants.RelationAuditor,
		HistoryCheckObject:   "project_membership:" + pm.UID,
		HistoryCheckRelation: fgaconstants.RelationAuditor,
		SortName:             strings.ToLower(pm.CompanyName),
		NameAndAliases:       nameAndAliases,
		ParentRefs:           parentRefs,
		Fulltext:             strings.Join(fulltext, " "),
		Tags:                 pm.Tags(),
	}
}

// BuildKeyContactIndexingConfig constructs an IndexingConfig for a KeyContact document.
func BuildKeyContactIndexingConfig(kc *model.KeyContact) *indexerTypes.IndexingConfig {
	var parentRefs []string
	if kc.B2BOrgUID != "" {
		parentRefs = append(parentRefs, "b2b_org:"+kc.B2BOrgUID)
	}
	if kc.ProjectUID != "" {
		parentRefs = append(parentRefs, "project:"+kc.ProjectUID)
	}
	if kc.MembershipUID != "" {
		parentRefs = append(parentRefs, "project_membership:"+kc.MembershipUID)
	}

	nameAndAliases := []string{kc.Name()}
	if kc.Email != "" {
		nameAndAliases = append(nameAndAliases, kc.Email)
	}

	var fulltext []string
	for _, s := range []string{kc.FirstName, kc.LastName, kc.Email, kc.Role, kc.CompanyName, kc.ProjectName} {
		if s != "" {
			fulltext = append(fulltext, s)
		}
	}

	emails := kc.Emails
	if len(emails) == 0 && kc.Email != "" {
		emails = []string{kc.Email}
	}
	contact := indexerTypes.ContactBody{
		LfxPrincipal: kc.UID,
		Name:         kc.Name(),
		Emails:       emails,
	}

	return &indexerTypes.IndexingConfig{
		Public:               boolPtr(false),
		ObjectID:             kc.UID,
		AccessCheckObject:    "project_membership:" + kc.MembershipUID,
		AccessCheckRelation:  fgaconstants.RelationAuditor,
		HistoryCheckObject:   "project_membership:" + kc.MembershipUID,
		HistoryCheckRelation: fgaconstants.RelationAuditor,
		SortName:             strings.ToLower(kc.LastName + " " + kc.FirstName),
		NameAndAliases:       nameAndAliases,
		ParentRefs:           parentRefs,
		Fulltext:             strings.Join(fulltext, " "),
		Tags:                 kc.Tags(),
		Contacts:             []indexerTypes.ContactBody{contact},
	}
}

// BuildKeyContactFGAPutMessage constructs a GenericFGAMessage that grants the
// given user (username) the key_contact relation on the parent project_membership.
func BuildKeyContactFGAPutMessage(membershipUID, username string) fgatypes.GenericFGAMessage {
	return fgatypes.GenericFGAMessage{
		ObjectType: "project_membership",
		Operation:  "member_put",
		Data: fgatypes.GenericMemberData{
			UID:       membershipUID,
			Username:  username,
			Relations: []string{"key_contact"},
		},
	}
}

// BuildKeyContactFGARemoveMessage constructs a GenericFGAMessage that revokes
// the key_contact relation for the given user (username) on the parent membership.
func BuildKeyContactFGARemoveMessage(membershipUID, username string) fgatypes.GenericFGAMessage {
	return fgatypes.GenericFGAMessage{
		ObjectType: "project_membership",
		Operation:  "member_remove",
		Data: fgatypes.GenericMemberData{
			UID:       membershipUID,
			Username:  username,
			Relations: []string{"key_contact"},
		},
	}
}

// BuildB2BOrgReparentingMessages returns FGA update_access messages when a
// b2b_org's ParentUID changes. Pass nil for current on create.
func BuildB2BOrgReparentingMessages(current, updated *model.B2BOrg, oldParentChildren, newParentChildren []string) []fgatypes.GenericFGAMessage {
	oldParent := ""
	if current != nil {
		oldParent = current.ParentUID
	}
	newParent := updated.ParentUID

	if oldParent == newParent {
		return nil
	}

	msgs := make([]fgatypes.GenericFGAMessage, 0, 3)

	parentRefs := map[string][]string{}
	if newParent != "" {
		parentRefs["parent"] = []string{"b2b_org:" + newParent}
	}
	msgs = append(msgs, fgatypes.GenericFGAMessage{
		ObjectType: "b2b_org",
		Operation:  "update_access",
		Data: fgatypes.GenericAccessData{
			UID:              updated.UID,
			References:       parentRefs,
			ExcludeRelations: b2bOrgNonParentRelations,
		},
	})

	if oldParent != "" && oldParentChildren != nil {
		msgs = append(msgs, BuildChildListMessage(oldParent, oldParentChildren))
	}

	if newParent != "" && newParentChildren != nil {
		msgs = append(msgs, BuildChildListMessage(newParent, newParentChildren))
	}

	return msgs
}

// BuildChildListMessage constructs an update_access FGA message that replaces
// a parent org's entire child list.
func BuildChildListMessage(parentUID string, children []string) fgatypes.GenericFGAMessage {
	childRefs := map[string][]string{}
	if len(children) > 0 {
		refs := make([]string, len(children))
		for i, uid := range children {
			refs[i] = "b2b_org:" + uid
		}
		childRefs["child"] = refs
	}
	return fgatypes.GenericFGAMessage{
		ObjectType: "b2b_org",
		Operation:  "update_access",
		Data: fgatypes.GenericAccessData{
			UID:              parentUID,
			References:       childRefs,
			ExcludeRelations: b2bOrgNonChildRelations,
		},
	}
}

// PublishB2BOrgTeamGrantsFGA emits the team-subject FGA tuples for a B2BOrg —
// the global_org_admin grant and the blanket auditor team grants.
// Safe to call during backfill — idempotent (fga-sync diffs before writing).
//
// It is the only FGA publisher on both /admin/reindex b2b_org paths, so the
// guard is on *either* grant being configured rather than on the global-admin
// UID alone: gating on that UID would let a blank value silently swallow the
// auditor grants on exactly the path an operator would use to repair them.
func PublishB2BOrgTeamGrantsFGA(ctx context.Context, p port.MemberPublisher, org *model.B2BOrg, globalOrgAdminTeamName string, auditorTeams []string) {
	globalOrgAdminTeamName = strings.TrimSpace(globalOrgAdminTeamName)
	if globalOrgAdminTeamName == "" && len(teamMemberRefs(auditorTeams...)) == 0 {
		return
	}
	msg := BuildB2BOrgFGAMessage(org, B2BOrgFGARefs{
		GlobalOrgAdminTeamName: globalOrgAdminTeamName,
		AuditorTeams:           auditorTeams,
	})
	if pubErr := p.Access(ctx, constants.FGASyncUpdateAccessSubject, msg); pubErr != nil {
		slog.WarnContext(ctx, "b2b org team grants FGA publish failed",
			"uid", org.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	}
}

// PublishB2BOrgParentFGA emits FGA parent/child hierarchy tuples for a B2BOrg
// that has a ParentUID. Safe to call during backfill — idempotent.
// parentChildren is the full current child-UID list for the parent org.
func PublishB2BOrgParentFGA(ctx context.Context, p port.MemberPublisher, org *model.B2BOrg, parentChildren []string) {
	if org.ParentUID == "" {
		return
	}
	// Synthesise an empty-parent "current" so BuildB2BOrgReparentingMessages emits
	// the new parent tuple without attempting to clean up a prior parent reference.
	current := &model.B2BOrg{UID: org.UID}
	for _, msg := range BuildB2BOrgReparentingMessages(current, org, nil, parentChildren) {
		if pubErr := p.Access(ctx, constants.FGASyncUpdateAccessSubject, msg); pubErr != nil {
			slog.WarnContext(ctx, "b2b org parent FGA publish failed",
				"uid", org.UID,
				"parent_uid", org.ParentUID,
				"error", pubErr,
				"publish_failed_for_backfill_repair", true)
		}
	}
}

// buildIndexerInput is the shared logic for the b2b_org, project_membership, and key_contact
// build*IndexerInput helpers: delete actions carry the UID string (indexer contract);
// create/update carry the full object. Workspace and workspace-project publishers handle
// this branching directly.
func buildIndexerInput(uid string, obj any, action indexerConstants.MessageAction) any {
	if action == indexerConstants.ActionDeleted {
		return uid
	}
	return obj
}

// buildB2BOrgIndexerInput returns the indexer message data for a b2b_org.
func buildB2BOrgIndexerInput(org *model.B2BOrg, action indexerConstants.MessageAction) any {
	return buildIndexerInput(org.UID, org, action)
}

// PublishB2BOrgIndexer builds and publishes a MemberIndexerMessage for a B2BOrg.
// Errors are swallowed and logged — /admin/reindex recovers missed records.
func PublishB2BOrgIndexer(ctx context.Context, p port.MemberPublisher, org *model.B2BOrg, action indexerConstants.MessageAction) {
	if action != indexerConstants.ActionDeleted && isScratchLogoURL(org.LogoURL) {
		slog.WarnContext(ctx, "skipping b2b org indexer publish for transient logo URL",
			"uid", org.UID,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if action != indexerConstants.ActionDeleted &&
		org.ParentDetail != nil &&
		org.ParentDetail.LogoURL != nil &&
		isScratchLogoURL(*org.ParentDetail.LogoURL) {
		safeOrg := *org
		safeParent := *org.ParentDetail
		safeParent.LogoURL = nil
		safeOrg.ParentDetail = &safeParent
		org = &safeOrg
		slog.WarnContext(ctx, "omitting transient parent logo URL from b2b org indexer publish",
			"uid", org.UID,
			"publish_failed_for_backfill_repair", true)
	}

	indexMsg := &model.MemberIndexerMessage{
		Action:         action,
		Tags:           org.Tags(),
		IndexingConfig: BuildB2BOrgIndexingConfig(org),
	}
	builtMsg, err := indexMsg.Build(ctx, buildB2BOrgIndexerInput(org, action))
	if err != nil {
		slog.WarnContext(ctx, "failed to build b2b org indexer message",
			"uid", org.UID,
			"error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if pubErr := p.Indexer(ctx, constants.IndexB2BOrgSubject, builtMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "b2b org indexer publish failed",
			"uid", org.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	} else {
		slog.DebugContext(ctx, "b2b org indexer published",
			"uid", org.UID, "subject", constants.IndexB2BOrgSubject)
	}
}

// scratchPathPattern matches the scratch key shape minted by
// logoUploaderOrchestrator: the scratch key carries no file extension, since
// Content-Type lives on the object itself.
var scratchPathPattern = regexp.MustCompile(`^org-logos-public-scratch/[^/]+/[^/]+$`)

// isScratchLogoURL reports whether rawURL addresses a transient logo-upload
// scratch object. It fails CLOSED: without a valid configured CDN host to
// compare against there is no way to tell our own scratch space from an
// unrelated site that happens to use the same path shape, and a false positive
// here permanently suppresses a legitimate record's publish on every retry. So
// an unset or unparseable CDN_URL_PREFIX means "classify nothing as transient"
// — the processes that lack that config (CDC consumer, backfill) never mint
// scratch URLs, and the upload path never publishes one.
func isScratchLogoURL(rawURL string) bool {
	cdnPrefix := os.Getenv("CDN_URL_PREFIX")
	if cdnPrefix == "" {
		return false
	}
	parsedCDN, cdnErr := url.Parse(cdnPrefix)
	if cdnErr != nil || parsedCDN.Host == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	if !strings.EqualFold(parsed.Host, parsedCDN.Host) {
		return false
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	return scratchPathPattern.MatchString(path)
}

// b2bOrgMemberView is the flat per-member wire entry in the indexer doc.
// Role is "writer" or "auditor"; writer takes precedence when a user holds both.
// invited_as and per-user created_at are omitted — role carries the role info
// and created_at is not needed downstream.
type b2bOrgMemberView struct {
	Username     string             `json:"username,omitempty"`
	Email        string             `json:"email"`
	Name         string             `json:"name,omitempty"`
	Avatar       string             `json:"avatar,omitempty"`
	Role         string             `json:"role"`
	InviteStatus model.InviteStatus `json:"invite_status"`
	UpdatedAt    string             `json:"updated_at"`
}

// b2bOrgSettingsIndexerView is the indexer doc shape for b2b_org_settings.
// Differs from model.B2BOrgSettings (the canonical KV/HTTP shape): single
// members[] with role field; no writers[]/auditors[]; no invited_as; no per-user created_at.
type b2bOrgSettingsIndexerView struct {
	UID       string             `json:"uid"`
	Members   []b2bOrgMemberView `json:"members"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
}

// buildB2BOrgSettingsIndexerView maps B2BOrgSettings to the flat indexer wire shape.
// Writers are processed first so writer role takes precedence over auditor when a
// user appears in both lists. Accepted entries are deduped by username; pending
// entries (empty username) are emitted as-is and not deduped. Revoked and expired
// entries are excluded.
func buildB2BOrgSettingsIndexerView(settings *model.B2BOrgSettings) b2bOrgSettingsIndexerView {
	view := b2bOrgSettingsIndexerView{
		UID:       settings.UID,
		Members:   []b2bOrgMemberView{},
		CreatedAt: settings.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: settings.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}

	seen := map[string]struct{}{}

	addMember := func(u model.B2BOrgUser, role string) {
		status := u.EffectiveStatus()
		if status == model.InviteStatusRevoked || status == model.InviteStatusExpired {
			return
		}
		if u.Username != "" {
			if _, exists := seen[u.Username]; exists {
				return
			}
			seen[u.Username] = struct{}{}
		}
		view.Members = append(view.Members, b2bOrgMemberView{
			Username:     u.Username,
			Email:        u.Email,
			Name:         u.Name,
			Avatar:       u.Avatar,
			Role:         role,
			InviteStatus: status,
			UpdatedAt:    u.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	for _, u := range settings.Writers {
		addMember(u, model.B2BOrgRoleWriter)
	}
	for _, u := range settings.Auditors {
		addMember(u, model.B2BOrgRoleAuditor)
	}
	return view
}

// PublishB2BOrgSettingsIndexer builds and publishes a MemberIndexerMessage for B2BOrgSettings.
// Errors are swallowed and logged — /admin/reindex recovers missed records.
func PublishB2BOrgSettingsIndexer(ctx context.Context, p port.MemberPublisher, org *model.B2BOrg, settings *model.B2BOrgSettings, action indexerConstants.MessageAction) {
	indexMsg := &model.MemberIndexerMessage{
		Action:         action,
		Tags:           settings.Tags(),
		IndexingConfig: BuildB2BOrgSettingsIndexingConfig(org, settings),
	}
	builtMsg, err := indexMsg.Build(ctx, buildB2BOrgSettingsIndexerView(settings))
	if err != nil {
		slog.WarnContext(ctx, "failed to build b2b org settings indexer message",
			"uid", org.UID,
			"error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if pubErr := p.Indexer(ctx, constants.IndexB2BOrgSettingsSubject, builtMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "b2b org settings indexer publish failed",
			"uid", org.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	}
}

// buildProjectMembershipIndexerInput returns the indexer message data for a project_membership.
func buildProjectMembershipIndexerInput(pm *model.ProjectMembership, action indexerConstants.MessageAction) any {
	return buildIndexerInput(pm.UID, pm, action)
}

// PublishProjectMembershipIndexer builds and publishes a MemberIndexerMessage for a ProjectMembership.
// Errors are swallowed and logged — /admin/reindex recovers missed records.
func PublishProjectMembershipIndexer(ctx context.Context, p port.MemberPublisher, pm *model.ProjectMembership, action indexerConstants.MessageAction) {
	if action != indexerConstants.ActionDeleted && isScratchLogoURL(pm.CompanyLogoURL) {
		safePM := *pm
		safePM.CompanyLogoURL = ""
		pm = &safePM
		slog.WarnContext(ctx, "omitting transient company logo URL from project membership indexer publish",
			"uid", pm.UID,
			"publish_failed_for_backfill_repair", true)
	}

	indexMsg := &model.MemberIndexerMessage{
		Action:         action,
		Tags:           pm.Tags(),
		IndexingConfig: BuildProjectMembershipIndexingConfig(pm),
	}
	builtMsg, err := indexMsg.Build(ctx, buildProjectMembershipIndexerInput(pm, action))
	if err != nil {
		slog.WarnContext(ctx, "failed to build project membership indexer message",
			"uid", pm.UID,
			"error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if pubErr := p.Indexer(ctx, constants.IndexProjectMembershipSubject, builtMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "project membership indexer publish failed",
			"uid", pm.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	} else {
		slog.DebugContext(ctx, "project membership indexer published",
			"uid", pm.UID, "subject", constants.IndexProjectMembershipSubject)
	}
}

// PublishProjectMembershipFGA builds and publishes a GenericFGAMessage for a ProjectMembership,
// writing the structural b2b_org and project reference tuples that enable the auditor cascade.
// Errors are swallowed and logged — /admin/reindex recovers missed records.
func PublishProjectMembershipFGA(ctx context.Context, p port.MemberPublisher, pm *model.ProjectMembership) {
	msg := BuildProjectMembershipFGAMessage(pm)
	if pubErr := p.Access(ctx, constants.FGASyncUpdateAccessSubject, msg); pubErr != nil {
		slog.WarnContext(ctx, "project membership fga publish failed",
			"uid", pm.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	} else {
		slog.DebugContext(ctx, "project membership FGA published",
			"uid", pm.UID, "subject", constants.FGASyncUpdateAccessSubject)
	}
}

// PublishProjectMembershipFGAPreservingMissingRefs emits a project_membership
// update_access message that excludes missing b2b_org/project relations so
// transiently unresolved parents are preserved.
func PublishProjectMembershipFGAPreservingMissingRefs(ctx context.Context, p port.MemberPublisher, pm *model.ProjectMembership) {
	msg := BuildProjectMembershipFGAMessagePreserveMissingRefs(pm)
	if pubErr := p.Access(ctx, constants.FGASyncUpdateAccessSubject, msg); pubErr != nil {
		slog.WarnContext(ctx, "project membership fga publish failed",
			"uid", pm.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	} else {
		slog.DebugContext(ctx, "project membership FGA published",
			"uid", pm.UID, "subject", constants.FGASyncUpdateAccessSubject)
	}
}

// buildKeyContactIndexerInput returns the indexer message data for a key_contact.
func buildKeyContactIndexerInput(kc *model.KeyContact, action indexerConstants.MessageAction) any {
	return buildIndexerInput(kc.UID, kc, action)
}

// PublishKeyContactIndexer builds and publishes a MemberIndexerMessage for a KeyContact.
// Errors are swallowed and logged — /admin/reindex recovers missed records.
func PublishKeyContactIndexer(ctx context.Context, p port.MemberPublisher, kc *model.KeyContact, action indexerConstants.MessageAction) {
	if action != indexerConstants.ActionDeleted && isScratchLogoURL(kc.CompanyLogoURL) {
		safeKC := *kc
		safeKC.CompanyLogoURL = ""
		kc = &safeKC
		slog.WarnContext(ctx, "omitting transient company logo URL from key contact indexer publish",
			"uid", kc.UID,
			"publish_failed_for_backfill_repair", true)
	}

	indexMsg := &model.MemberIndexerMessage{
		Action:         action,
		Tags:           kc.Tags(),
		IndexingConfig: BuildKeyContactIndexingConfig(kc),
	}
	builtMsg, err := indexMsg.Build(ctx, buildKeyContactIndexerInput(kc, action))
	if err != nil {
		slog.WarnContext(ctx, "failed to build key contact indexer message",
			"uid", kc.UID,
			"error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if pubErr := p.Indexer(ctx, constants.IndexKeyContactSubject, builtMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "key contact indexer publish failed",
			"uid", kc.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	} else {
		slog.DebugContext(ctx, "key contact indexer published",
			"uid", kc.UID, "subject", constants.IndexKeyContactSubject)
	}
}

// BuildWorkspaceIndexingConfig constructs an IndexingConfig for a Workspace document.
// AccessCheck resolves against the parent b2b_org (reusing the existing FGA type).
// Public is explicitly false. HistoryCheckRelation is writer (write-side concern).
// No FGA Access publish for workspaces — reuse existing b2b_org tuples.
func BuildWorkspaceIndexingConfig(org *model.B2BOrg, ws *model.Workspace) *indexerTypes.IndexingConfig {
	nameAndAliases := append(orgNameAndAliases(org), ws.Name)

	parentRefs := []string{"b2b_org:" + org.UID}
	if org.ParentUID != "" {
		parentRefs = append(parentRefs, "b2b_org:"+org.ParentUID)
	}

	return &indexerTypes.IndexingConfig{
		Public:               boolPtr(false),
		ObjectID:             ws.UID,
		AccessCheckObject:    "b2b_org:" + org.UID,
		AccessCheckRelation:  fgaconstants.RelationAuditor,
		HistoryCheckObject:   "b2b_org:" + org.UID,
		HistoryCheckRelation: fgaconstants.RelationWriter,
		SortName:             strings.ToLower(ws.Name),
		NameAndAliases:       nameAndAliases,
		ParentRefs:           parentRefs,
		Fulltext:             strings.Join(ws.FulltextTokens(), " "),
		Tags:                 ws.Tags(org.UID),
	}
}

// buildWorkspaceIndexerView returns the data payload for a workspace indexer message.
func buildWorkspaceIndexerView(ws *model.Workspace) any {
	return ws
}

// buildWorkspaceIndexerInput returns the indexer message data for a workspace.
// Delete actions must carry the UID string (indexer contract); create/update carry the struct.
func buildWorkspaceIndexerInput(ws *model.Workspace, action indexerConstants.MessageAction) any {
	if action == indexerConstants.ActionDeleted {
		return ws.UID
	}
	return buildWorkspaceIndexerView(ws)
}

// buildWorkspaceProjectIndexerInput returns the indexer message data for a workspace-project association.
func buildWorkspaceProjectIndexerInput(orgUID, workspaceUID string, wp model.WorkspaceProject, wps model.WorkspaceProjects, action indexerConstants.MessageAction) any {
	if action == indexerConstants.ActionDeleted {
		return wp.AssociationID(workspaceUID)
	}
	return buildWorkspaceProjectIndexerView(orgUID, workspaceUID, wp, wps)
}

// PublishWorkspaceIndexer builds and publishes a MemberIndexerMessage for a Workspace.
// Errors are swallowed and logged — /admin/reindex recovers missed records.
func PublishWorkspaceIndexer(ctx context.Context, p port.MemberPublisher, org *model.B2BOrg, ws *model.Workspace, action indexerConstants.MessageAction) {
	indexMsg := &model.MemberIndexerMessage{
		Action:         action,
		Tags:           ws.Tags(org.UID),
		IndexingConfig: BuildWorkspaceIndexingConfig(org, ws),
	}
	builtMsg, err := indexMsg.Build(ctx, buildWorkspaceIndexerInput(ws, action))
	if err != nil {
		slog.WarnContext(ctx, "failed to build workspace indexer message",
			"workspace_uid", ws.UID,
			"org_uid", org.UID,
			"error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if pubErr := p.Indexer(ctx, constants.IndexOrgWorkspaceSubject, builtMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "workspace indexer publish failed",
			"workspace_uid", ws.UID,
			"org_uid", org.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	}
}

// PublishWorkspaceProjectIndexer builds and publishes a MemberIndexerMessage for a
// workspace-project association. Errors are swallowed and logged — /admin/reindex
// recovers missed records.
func PublishWorkspaceProjectIndexer(ctx context.Context, p port.MemberPublisher, org *model.B2BOrg, ws *model.Workspace, wp model.WorkspaceProject, wps model.WorkspaceProjects, action indexerConstants.MessageAction) {
	indexMsg := &model.MemberIndexerMessage{
		Action:         action,
		Tags:           wp.Tags(org.UID, ws.UID),
		IndexingConfig: BuildWorkspaceProjectIndexingConfig(org, ws, wp),
	}
	builtMsg, err := indexMsg.Build(ctx, buildWorkspaceProjectIndexerInput(org.UID, ws.UID, wp, wps, action))
	if err != nil {
		slog.WarnContext(ctx, "failed to build workspace project indexer message",
			"workspace_uid", ws.UID,
			"project_uid", wp.ProjectUID,
			"org_uid", org.UID,
			"error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}
	if pubErr := p.Indexer(ctx, constants.IndexOrgWorkspaceProjectSubject, builtMsg, false); pubErr != nil {
		slog.WarnContext(ctx, "workspace project indexer publish failed",
			"workspace_uid", ws.UID,
			"project_uid", wp.ProjectUID,
			"org_uid", org.UID,
			"error", pubErr,
			"publish_failed_for_backfill_repair", true)
	}
}

// BuildWorkspaceProjectIndexingConfig constructs an IndexingConfig for a workspace-project
// association. ObjectID is "{workspaceUID}:{projectUID}" — a compound key that uniquely
// identifies the association.
func BuildWorkspaceProjectIndexingConfig(org *model.B2BOrg, ws *model.Workspace, wp model.WorkspaceProject) *indexerTypes.IndexingConfig {
	parentRefs := []string{"org_workspace:" + ws.UID, "b2b_org:" + org.UID}
	if org.ParentUID != "" {
		parentRefs = append(parentRefs, "b2b_org:"+org.ParentUID)
	}

	fulltextParts := make([]string, 0, 2)
	if wp.ProjectName != "" {
		fulltextParts = append(fulltextParts, wp.ProjectName)
	}
	if wp.ProjectSlug != "" {
		fulltextParts = append(fulltextParts, wp.ProjectSlug)
	}
	fulltext := strings.Join(fulltextParts, " ")

	return &indexerTypes.IndexingConfig{
		Public:               boolPtr(false),
		ObjectID:             wp.AssociationID(ws.UID),
		AccessCheckObject:    "b2b_org:" + org.UID,
		AccessCheckRelation:  fgaconstants.RelationAuditor,
		HistoryCheckObject:   "b2b_org:" + org.UID,
		HistoryCheckRelation: fgaconstants.RelationWriter,
		SortName:             strings.ToLower(wp.ProjectName),
		NameAndAliases:       []string{wp.ProjectName},
		ParentRefs:           parentRefs,
		Fulltext:             fulltext,
		Tags:                 wp.Tags(org.UID, ws.UID),
	}
}

// workspaceProjectIndexerView is the indexer document body for an org_workspace_project.
type workspaceProjectIndexerView struct {
	B2BOrgUID           string    `json:"b2b_org_uid"`
	B2BOrgWorkspaceUID  string    `json:"b2b_org_workspace_uid"`
	ProjectUID          string    `json:"project_uid"`
	ProjectSlug         string    `json:"project_slug,omitempty"`
	ProjectName         string    `json:"project_name,omitempty"`
	CreatedBy           string    `json:"created_by,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedBy           string    `json:"updated_by,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
	CollectionUpdatedAt time.Time `json:"collection_updated_at"`
}

// buildWorkspaceProjectIndexerView returns the data payload for an org_workspace_project
// indexer message. Includes the per-item audit quartet and the container-level UpdatedAt.
func buildWorkspaceProjectIndexerView(orgUID, workspaceUID string, wp model.WorkspaceProject, wps model.WorkspaceProjects) any {
	return workspaceProjectIndexerView{
		B2BOrgUID:           orgUID,
		B2BOrgWorkspaceUID:  workspaceUID,
		ProjectUID:          wp.ProjectUID,
		ProjectSlug:         wp.ProjectSlug,
		ProjectName:         wp.ProjectName,
		CreatedBy:           wp.CreatedBy,
		CreatedAt:           wp.CreatedAt,
		UpdatedBy:           wp.UpdatedBy,
		UpdatedAt:           wp.UpdatedAt,
		CollectionUpdatedAt: wps.UpdatedAt,
	}
}
