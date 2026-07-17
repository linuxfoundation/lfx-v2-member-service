// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	natspkg "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// BackfillIterator provides paged SOQL iterators for full and since-filtered
// backfill modes. Each method calls fn once per page of converted records.
type BackfillIterator interface {
	IterB2BOrgs(ctx context.Context, since *time.Time, fn func([]*model.B2BOrg) error) error
	IterProjectMemberships(ctx context.Context, since *time.Time, fn func([]*model.ProjectMembership) error) error
	IterKeyContacts(ctx context.Context, since *time.Time, fn func([]*model.KeyContact) error) error
}

// KeyContactSObjectReader fetches a single KeyContact by UID via the live sObject path.
// Defined here to avoid a direct service→salesforce dependency while keeping the
// port package free of infrastructure concerns.
type KeyContactSObjectReader interface {
	AssembleKeyContact(ctx context.Context, uid string) (*model.KeyContact, time.Time, error)
}

const (
	entityTypeB2BOrg            = "b2b_org"
	entityTypeProjectMembership = "project_membership"
	entityTypeKeyContact        = "key_contact"
	entityTypeB2BOrgSettings    = "b2b_org_settings"
)

// allBackfillTypes is the canonical ordered list of types the backfill supports.
var allBackfillTypes = []string{entityTypeB2BOrg, entityTypeProjectMembership, entityTypeKeyContact, entityTypeB2BOrgSettings}

// Runner orchestrates a reindex run. It is safe to call Run concurrently
// from multiple goroutines (each run is independent). Full-mode runs acquire a
// per-type NATS KV lock so the same type is not reindexed simultaneously across
// pods.
type Runner struct {
	iter                  BackfillIterator
	b2bReader             port.B2BOrgReader
	pmReader              port.ProjectMembershipReader
	kcReader              KeyContactSObjectReader
	settingsReader        port.B2BOrgSettingsReader
	settingsWriter        port.B2BOrgSettingsWriter
	userReader            port.UserReader
	publisher             port.MemberPublisher
	natsClient            *natspkg.NATSClient
	globalOrgAdminTeamUID string
	resolver              port.ProjectResolver
}

// RunnerOption configures optional Runner collaborators. The avatar-enrichment path (the
// b2b_org_settings type with BackfillRequest.EnrichAvatars) needs the settings writer + user reader;
// callers that never enrich avatars can omit them.
type RunnerOption func(*Runner)

// WithSettingsWriter wires the b2b_org_settings writer used to persist enriched avatars.
func WithSettingsWriter(w port.B2BOrgSettingsWriter) RunnerOption {
	return func(r *Runner) { r.settingsWriter = w }
}

// WithUserReader wires the auth-service reader used to resolve avatar pictures.
func WithUserReader(u port.UserReader) RunnerOption {
	return func(r *Runner) { r.userReader = u }
}

// NewRunner constructs a Runner.
func NewRunner(
	iter BackfillIterator,
	b2bReader port.B2BOrgReader,
	pmReader port.ProjectMembershipReader,
	kcReader KeyContactSObjectReader,
	settingsReader port.B2BOrgSettingsReader,
	publisher port.MemberPublisher,
	natsClient *natspkg.NATSClient,
	globalOrgAdminTeamUID string,
	resolver port.ProjectResolver,
	opts ...RunnerOption,
) *Runner {
	r := &Runner{
		iter:                  iter,
		b2bReader:             b2bReader,
		pmReader:              pmReader,
		kcReader:              kcReader,
		settingsReader:        settingsReader,
		publisher:             publisher,
		natsClient:            natsClient,
		globalOrgAdminTeamUID: globalOrgAdminTeamUID,
		resolver:              resolver,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// maxToleratedAvatarFailures bounds how many per-principal auth-service lookup failures an avatar
// enrichment pass tolerates before the type is reported failed. A handful of transient misses must
// not fail the whole Job; a systemic auth-service outage should.
const maxToleratedAvatarFailures = 50

type runMode string

const (
	modeTargeted runMode = "targeted"
	modeFiltered runMode = "filtered"
	modeFull     runMode = "full"
)

// ClassifyMode returns the run mode for the given request.
func ClassifyMode(req BackfillRequest) runMode {
	if len(req.Items) > 0 {
		return modeTargeted
	}
	if req.Since != nil {
		return modeFiltered
	}
	return modeFull
}

func effectiveTypes(requested []string) []string {
	if len(requested) == 0 {
		return allBackfillTypes
	}
	return requested
}

// Run executes the backfill. The async /admin/reindex path calls it in a goroutine and ignores the
// return; the avatar-backfill Job uses the error to set its exit code. Intended to be called with
// context.WithoutCancel so it outlives the HTTP request.
func (r *Runner) Run(ctx context.Context, req BackfillRequest) error {
	mode := ClassifyMode(req)
	log := slog.With(
		"run_id", req.RunID,
		"component", "backfill",
		"mode", string(mode),
		"dry_run", req.DryRun,
	)
	log.InfoContext(ctx, "backfill started")
	defer log.InfoContext(ctx, "backfill complete")

	if mode == modeTargeted {
		r.runTargeted(ctx, log, req)
		return nil
	}

	if mode == modeFull {
		log.WarnContext(ctx, "full reindex started", "full_reindex_started", true)
	}

	succeeded := make([]string, 0, len(allBackfillTypes))
	var failed, skipped []string

	for _, t := range effectiveTypes(req.Types) {
		var err error
		if mode == modeFull && r.natsClient != nil {
			release, lockErr := natspkg.AcquireFullRunLock(ctx, r.natsClient, req.RunID, t)
			if lockErr != nil {
				log.WarnContext(ctx, "full reindex skipped — lock held",
					"type", t,
					"full_reindex_rejected_lock_held", true,
					"error", lockErr)
				skipped = append(skipped, t)
				continue
			}
			err = r.runType(ctx, log, req, t)
			release()
		} else {
			err = r.runType(ctx, log, req, t)
		}

		if err != nil {
			log.ErrorContext(ctx, "backfill type failed",
				"type", t,
				"error", err)
			failed = append(failed, t)
		} else {
			succeeded = append(succeeded, t)
		}
	}

	log.InfoContext(ctx, "backfill summary",
		"succeeded", succeeded,
		"failed", failed,
		"skipped_locked", skipped)

	if len(failed) > 0 {
		return fmt.Errorf("backfill completed with failed type(s): %v", failed)
	}
	return nil
}

func (r *Runner) runType(ctx context.Context, log *slog.Logger, req BackfillRequest, sfType string) error {
	var total, published int

	logPage := func(pageLen int) {
		log.InfoContext(ctx, "backfill page processed",
			"type", sfType, "page_size", pageLen,
			"total_so_far", total, "published_so_far", published)
	}

	switch sfType {
	case entityTypeB2BOrg:
		return r.iter.IterB2BOrgs(ctx, req.Since, func(orgs []*model.B2BOrg) error {
			// Pre-fetch children for every unique org and parent in this page so we
			// issue one SOQL query per unique UID rather than per org.
			orgChildrenCache := map[string][]string{}
			if !req.DryRun {
				seen := map[string]struct{}{}
				for _, org := range orgs {
					if org.UID != "" {
						seen[org.UID] = struct{}{}
					}
					if org.ParentUID != "" {
						seen[org.ParentUID] = struct{}{}
					}
				}
				seenUIDs := make([]string, 0, len(seen))
				for uid := range seen {
					seenUIDs = append(seenUIDs, uid)
				}
				batchedChildren, batchErr := r.b2bReader.FetchChildUIDsByParentUIDs(ctx, seenUIDs)
				if batchErr != nil {
					log.WarnContext(ctx, "failed to bulk-fetch child UIDs for backfill page",
						"error", batchErr, "publish_failed_for_backfill_repair", true)
					if batchedChildren == nil {
						batchedChildren = map[string][]string{}
					}
				}
				orgChildrenCache = batchedChildren
			}

			for _, org := range orgs {
				total++
				if !req.DryRun {
					// Populate children from cache for the indexer document.
					if children, ok := orgChildrenCache[org.UID]; ok {
						org.IsParent = len(children) > 0
					}
					PublishB2BOrgIndexer(ctx, r.publisher, org, indexerConstants.ActionUpdated)
					PublishB2BOrgGlobalAdminFGA(ctx, r.publisher, org, r.globalOrgAdminTeamUID)
					if org.ParentUID != "" {
						if children, ok := orgChildrenCache[org.ParentUID]; ok {
							PublishB2BOrgParentFGA(ctx, r.publisher, org, children)
						}
					}
					published++
				}
			}
			logPage(len(orgs))
			return nil
		})
	case entityTypeProjectMembership:
		return r.iter.IterProjectMemberships(ctx, req.Since, func(pms []*model.ProjectMembership) error {
			for _, pm := range pms {
				total++
				if !req.DryRun {
					uid, ok := resolveProjectUID(ctx, r.resolver, pm.ProjectSlug, pm.ProjectUID)
					if ok {
						pm.ProjectUID = uid
						PublishProjectMembershipIndexer(ctx, r.publisher, pm, indexerConstants.ActionUpdated)
						PublishProjectMembershipFGA(ctx, r.publisher, pm)
					} else {
						log.ErrorContext(ctx, "skipping project_membership indexer publish; project_uid unresolved — publishing OpenFGA only",
							"uid", pm.UID, "slug", pm.ProjectSlug, "publish_failed_for_backfill_repair", true)
						PublishProjectMembershipFGAPreservingMissingRefs(ctx, r.publisher, pm)
					}
					published++
				}
			}
			logPage(len(pms))
			return nil
		})
	case entityTypeKeyContact:
		if req.Since != nil {
			log.WarnContext(ctx, "since filter on key_contact only checks Project_Role__c.LastModifiedDate; Contact/Asset field changes are not captured",
				"since_filter_misses_joined_fields", true)
		}
		return r.iter.IterKeyContacts(ctx, req.Since, func(kcs []*model.KeyContact) error {
			for _, kc := range kcs {
				total++
				if !req.DryRun {
					uid, ok := resolveProjectUID(ctx, r.resolver, kc.ProjectSlug, kc.ProjectUID)
					if ok {
						kc.ProjectUID = uid
						PublishKeyContactIndexer(ctx, r.publisher, kc, indexerConstants.ActionUpdated)
					} else {
						log.ErrorContext(ctx, "skipping key_contact indexer publish; project_uid unresolved — publishing OpenFGA only",
							"uid", kc.UID, "slug", kc.ProjectSlug, "publish_failed_for_backfill_repair", true)
					}
					PublishKeyContactFGA(ctx, r.publisher, kc)
					published++
				}
			}
			logPage(len(kcs))
			return nil
		})
	case entityTypeB2BOrgSettings:
		if r.settingsReader == nil {
			return fmt.Errorf("b2b_org_settings backfill requires a settingsReader — pass it as the settingsReader argument to NewRunner")
		}
		if req.EnrichAvatars && (r.userReader == nil || r.settingsWriter == nil) {
			return fmt.Errorf("b2b_org_settings avatar enrichment requires a userReader and settingsWriter — pass WithUserReader/WithSettingsWriter to NewRunner")
		}
		orgUIDs, listErr := r.settingsReader.ListSettingsOrgUIDs(ctx)
		if listErr != nil {
			return fmt.Errorf("listing org-settings keys: %w", listErr)
		}
		var avatarFailures int
		for _, uid := range orgUIDs {
			total++
			// Non-enrich dry-run keeps the original "count only, no reads" behavior.
			if req.DryRun && !req.EnrichAvatars {
				continue
			}

			settings, revision, settingsErr := r.settingsReader.GetSettings(ctx, uid)
			if settingsErr != nil {
				log.WarnContext(ctx, "failed to fetch settings for backfill",
					"uid", uid, "error", settingsErr,
					"publish_failed_for_backfill_repair", true)
				continue
			}
			if settings == nil {
				log.DebugContext(ctx, "settings absent for org — skipping (race between list and get)",
					"uid", uid)
				continue
			}

			if req.EnrichAvatars {
				changed, failures := r.enrichSettingsAvatars(ctx, log, req, uid, settings)
				avatarFailures += failures
				if !changed {
					// Idempotent: no avatar drift → nothing to persist or republish (also the recurring-refresh no-op).
					continue
				}
			}

			if req.DryRun {
				published++
				continue
			}

			// Persist enriched avatars before republishing so the indexer doc reflects them.
			if req.EnrichAvatars {
				if wErr := r.settingsWriter.UpdateSettings(ctx, settings, revision); wErr != nil {
					if errs.IsConflict(wErr) {
						log.InfoContext(ctx, "settings changed concurrently; skipping (will retry next run)", "uid", uid)
						continue
					}
					log.WarnContext(ctx, "failed to persist enriched avatars",
						"uid", uid, "error", wErr, "publish_failed_for_backfill_repair", true)
					continue
				}
			}

			org, orgErr := r.b2bReader.GetB2BOrg(ctx, uid)
			if orgErr != nil {
				if errs.IsNotFound(orgErr) {
					log.WarnContext(ctx, "org not found for settings backfill — skipping",
						"uid", uid, "not_found", true)
				} else {
					log.WarnContext(ctx, "failed to fetch org for settings backfill",
						"uid", uid, "error", orgErr,
						"publish_failed_for_backfill_repair", true)
				}
				continue
			}
			PublishB2BOrgSettingsIndexer(ctx, r.publisher, org, settings, indexerConstants.ActionUpdated)
			published++
		}
		logPage(len(orgUIDs))
		if req.EnrichAvatars && avatarFailures > maxToleratedAvatarFailures {
			return fmt.Errorf("avatar enrichment exceeded failure tolerance: %d lookup(s) failed (limit %d)", avatarFailures, maxToleratedAvatarFailures)
		}
		return nil

	default:
		return fmt.Errorf("unhandled backfill type: %q", sfType)
	}
}

// enrichSettingsAvatars refreshes each accepted writer/auditor avatar in `settings` (in place) from
// the auth-service and reports whether any value changed plus the count of lookup failures. The
// backfill keeps the projection in sync with the source (sync-and-clear), which is deliberately
// asymmetric with the write path (which only ever fills empty avatars): a NotFound miss leaves the
// existing value untouched, but a successful lookup whose picture is empty clears a previously-stored
// avatar (a user who removed their Auth0 photo gets their indexed avatar blanked on the next pass). A
// transport/app error is counted and isolated so a single transient failure never aborts the run (see
// maxToleratedAvatarFailures); a context cancellation aborts the pass without counting as a failure.
// Rate-limited by req.AvatarSleep; honours MissingOnly. Caller persists the change.
func (r *Runner) enrichSettingsAvatars(ctx context.Context, log *slog.Logger, req BackfillRequest, uid string, settings *model.B2BOrgSettings) (changed bool, failures int) {
	// enrichList returns true when the pass was aborted by context cancellation, so the caller
	// stops before the next relation rather than counting clean-shutdown errors as failures.
	enrichList := func(users []model.B2BOrgUser) (aborted bool) {
		for i := range users {
			if ctx.Err() != nil {
				return true
			}
			u := &users[i]
			status := u.EffectiveStatus()
			if status == model.InviteStatusRevoked || status == model.InviteStatusExpired {
				continue
			}
			username := strings.TrimSpace(u.Username)
			if username == "" {
				continue
			}
			if req.AvatarMissingOnly && u.Avatar != "" {
				continue
			}

			meta, err := r.userReader.UserMetadataByPrincipal(ctx, username)
			if err != nil {
				// A context cancellation/deadline is a clean shutdown, not a lookup failure:
				// abort the pass rather than counting it toward maxToleratedAvatarFailures.
				if ctx.Err() != nil {
					return true
				}
				if !errs.IsNotFound(err) {
					failures++
					log.WarnContext(ctx, "avatar enrichment lookup failed",
						"uid", uid, "username", redaction.Redact(username), "error", err)
				}
			} else if u.Avatar != meta.Picture {
				// Apply the fetched value before the rate-limit sleep so a mid-run cancel doesn't drop it.
				u.Avatar = meta.Picture
				changed = true
			}

			if req.AvatarSleep > 0 {
				if sleepErr := sleepWithContext(ctx, req.AvatarSleep); sleepErr != nil {
					return true
				}
			}
		}
		return false
	}
	if enrichList(settings.Writers) {
		return changed, failures
	}
	enrichList(settings.Auditors)
	return changed, failures
}

// sleepWithContext waits for d or until ctx is cancelled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Runner) runTargeted(ctx context.Context, log *slog.Logger, req BackfillRequest) {
	var notFound, published int
	// childUIDsCache memoises FetchChildUIDsByParentUID within this request so
	// sibling orgs sharing the same parent don't each trigger a separate SOQL call.
	childUIDsCache := map[string][]string{}
	fetchChildUIDs := func(uid string) ([]string, error) {
		if v, ok := childUIDsCache[uid]; ok {
			return v, nil
		}
		uids, err := r.b2bReader.FetchChildUIDsByParentUID(ctx, uid)
		if err == nil {
			childUIDsCache[uid] = uids
		}
		return uids, err
	}

	for _, item := range req.Items {
		if item.Type == entityTypeB2BOrgSettings && r.settingsReader == nil {
			log.ErrorContext(ctx, "b2b_org_settings targeted backfill requires settingsReader — wiring error",
				"uid", item.UID, "publish_failed_for_backfill_repair", true)
			continue
		}
		switch item.Type {
		case entityTypeB2BOrg:
			org, err := r.b2bReader.GetB2BOrg(ctx, item.UID)
			if err != nil {
				if errs.IsNotFound(err) {
					log.WarnContext(ctx, "targeted item not found", "type", item.Type, "uid", item.UID, "not_found", true)
					notFound++
				} else {
					log.WarnContext(ctx, "targeted item fetch error", "type", item.Type, "uid", item.UID, "error", err,
						"publish_failed_for_backfill_repair", true)
				}
				continue
			}
			if !req.DryRun {
				// Fetch direct children for the indexer document.
				childUIDs, childErr := fetchChildUIDs(org.UID)
				if childErr != nil {
					log.WarnContext(ctx, "failed to fetch child UIDs for indexer",
						"uid", org.UID, "error", childErr,
						"publish_failed_for_backfill_repair", true)
				} else {
					org.IsParent = len(childUIDs) > 0
				}
				PublishB2BOrgIndexer(ctx, r.publisher, org, indexerConstants.ActionUpdated)
				PublishB2BOrgGlobalAdminFGA(ctx, r.publisher, org, r.globalOrgAdminTeamUID)
				if org.ParentUID != "" {
					children, childErr := fetchChildUIDs(org.ParentUID)
					if childErr != nil {
						log.WarnContext(ctx, "failed to fetch parent children for FGA backfill",
							"uid", org.UID, "parent_uid", org.ParentUID, "error", childErr,
							"publish_failed_for_backfill_repair", true)
					} else {
						PublishB2BOrgParentFGA(ctx, r.publisher, org, children)
					}
				}
				published++
			}

		case entityTypeProjectMembership:
			pm, _, err := r.pmReader.AssembleProjectMembership(ctx, item.UID)
			if err != nil {
				if errs.IsNotFound(err) {
					log.WarnContext(ctx, "targeted item not found", "type", item.Type, "uid", item.UID, "not_found", true)
					notFound++
				} else {
					log.WarnContext(ctx, "targeted item fetch error", "type", item.Type, "uid", item.UID, "error", err,
						"publish_failed_for_backfill_repair", true)
				}
				continue
			}
			if !req.DryRun {
				uid, ok := resolveProjectUID(ctx, r.resolver, pm.ProjectSlug, pm.ProjectUID)
				if ok {
					pm.ProjectUID = uid
					PublishProjectMembershipIndexer(ctx, r.publisher, pm, indexerConstants.ActionUpdated)
					PublishProjectMembershipFGA(ctx, r.publisher, pm)
				} else {
					log.ErrorContext(ctx, "skipping project_membership indexer publish; project_uid unresolved — publishing OpenFGA only",
						"uid", pm.UID, "slug", pm.ProjectSlug, "publish_failed_for_backfill_repair", true)
					PublishProjectMembershipFGAPreservingMissingRefs(ctx, r.publisher, pm)
				}
				published++
			}

		case entityTypeKeyContact:
			kc, _, err := r.kcReader.AssembleKeyContact(ctx, item.UID)
			if err != nil {
				if errs.IsNotFound(err) {
					log.WarnContext(ctx, "targeted item not found", "type", item.Type, "uid", item.UID, "not_found", true)
					notFound++
				} else {
					log.WarnContext(ctx, "targeted item fetch error", "type", item.Type, "uid", item.UID, "error", err,
						"publish_failed_for_backfill_repair", true)
				}
				continue
			}
			if !req.DryRun {
				uid, ok := resolveProjectUID(ctx, r.resolver, kc.ProjectSlug, kc.ProjectUID)
				if ok {
					kc.ProjectUID = uid
					PublishKeyContactIndexer(ctx, r.publisher, kc, indexerConstants.ActionUpdated)
				} else {
					log.ErrorContext(ctx, "skipping key_contact indexer publish; project_uid unresolved — publishing OpenFGA only",
						"uid", kc.UID, "slug", kc.ProjectSlug, "publish_failed_for_backfill_repair", true)
				}
				PublishKeyContactFGA(ctx, r.publisher, kc)
				published++
			}

		case entityTypeB2BOrgSettings:
			org, orgErr := r.b2bReader.GetB2BOrg(ctx, item.UID)
			if orgErr != nil {
				if errs.IsNotFound(orgErr) {
					log.WarnContext(ctx, "targeted item not found", "type", item.Type, "uid", item.UID, "not_found", true)
					notFound++
				} else {
					log.WarnContext(ctx, "targeted item fetch error", "type", item.Type, "uid", item.UID, "error", orgErr,
						"publish_failed_for_backfill_repair", true)
				}
				continue
			}
			settings, revision, settingsErr := r.settingsReader.GetSettings(ctx, item.UID)
			if settingsErr != nil {
				log.WarnContext(ctx, "targeted item fetch error", "type", item.Type, "uid", item.UID, "error", settingsErr,
					"publish_failed_for_backfill_repair", true)
				continue
			}
			if settings == nil {
				log.WarnContext(ctx, "targeted item not found", "type", item.Type, "uid", item.UID, "not_found", true)
				notFound++
				continue
			}
			if req.EnrichAvatars && r.userReader != nil {
				r.enrichSettingsAvatars(ctx, log, req, item.UID, settings)
			}
			if !req.DryRun {
				if req.EnrichAvatars && r.settingsWriter != nil {
					if wErr := r.settingsWriter.UpdateSettings(ctx, settings, revision); wErr != nil {
						// Skip publish on any persist failure (parity with full mode) so the indexer
						// only ever sees avatars that were actually persisted — a benign revision
						// conflict must not project the unpersisted enriched values.
						if errs.IsConflict(wErr) {
							log.InfoContext(ctx, "settings changed concurrently; skipping (will retry next run)", "uid", item.UID)
						} else {
							log.WarnContext(ctx, "failed to persist enriched avatars (targeted)", "uid", item.UID, "error", wErr,
								"publish_failed_for_backfill_repair", true)
						}
						continue
					}
				}
				PublishB2BOrgSettingsIndexer(ctx, r.publisher, org, settings, indexerConstants.ActionUpdated)
				published++
			}
		}
	}

	log.InfoContext(ctx, "targeted backfill complete",
		"total_items", len(req.Items),
		"published", published,
		"not_found", notFound,
		"would_publish_count", len(req.Items)-notFound)
}
