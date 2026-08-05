// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	natspkg "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// BackfillIterator provides paged SOQL iterators for full and since-filtered
// backfill modes. Each method calls fn once per page of converted records. The
// optional since/until bounds apply an inclusive LastModifiedDate window
// (>= since AND <= until); either may be nil for an open bound.
type BackfillIterator interface {
	IterB2BOrgs(ctx context.Context, since, until *time.Time, fn func([]*model.B2BOrg) error) error
	IterProjectMemberships(ctx context.Context, since, until *time.Time, fn func([]*model.ProjectMembership) error) error
	IterKeyContacts(ctx context.Context, since, until *time.Time, fn func([]*model.KeyContact) error) error
}

// KeyContactSObjectReader fetches a single KeyContact by UID via the live sObject path.
// Defined here to avoid a direct service→salesforce dependency while keeping the
// port package free of infrastructure concerns.
type KeyContactSObjectReader interface {
	AssembleKeyContact(ctx context.Context, uid string) (*model.KeyContact, time.Time, error)
}

const (
	entityTypeB2BOrg            = constants.ReindexTypeB2BOrg
	entityTypeProjectMembership = constants.ReindexTypeProjectMembership
	entityTypeKeyContact        = constants.ReindexTypeKeyContact
	entityTypeB2BOrgSettings    = constants.ReindexTypeB2BOrgSettings
)

// allBackfillTypes is the canonical ordered list of types the backfill supports.
var allBackfillTypes = []string{entityTypeB2BOrg, entityTypeProjectMembership, entityTypeKeyContact, entityTypeB2BOrgSettings}

// validBackfillTypes is the set form of allBackfillTypes, precomputed once for
// membership lookups in ValidateAndBuildRequest.
var validBackfillTypes = func() map[string]bool {
	m := make(map[string]bool, len(allBackfillTypes))
	for _, t := range allBackfillTypes {
		m[t] = true
	}
	return m
}()

// defaultAdminReindexQuotaThreshold is the fraction of the daily Salesforce REST
// API quota at/above which a cdc_repair drain refuses to start (and stops
// mid-page). Configurable via ADMIN_REINDEX_QUOTA_THRESHOLD. Default: 0.80 —
// deliberately below the consumer's 0.95 skip threshold so a drain never
// competes with live traffic for the last slice of quota.
const defaultAdminReindexQuotaThreshold = 0.80

// repairPageCap bounds how many pending markers a single cdc_repair run selects
// and drains. Operators re-run until selected_count is zero.
const repairPageCap = 100

// settingsQuotaCheckInterval is how often the b2b_org_settings flat loop (which
// has no SOQL pages) re-checks the passive quota gauge so a long key list still
// stops mid-run.
const settingsQuotaCheckInterval = 100

// errQuotaStop is returned from a full/filtered page callback (or the settings
// flat loop) when the passive quota gauge reaches the admin threshold. runType
// propagates it and Run treats it as a clean stop (stopped_early), not a
// failure. The operator re-runs the request (optionally a tighter since/until
// window) once quota recovers — reindex is idempotent, so there is no watermark.
var errQuotaStop = errors.New("salesforce quota threshold reached; backfill stopped")

// Runner orchestrates a reindex run. It is safe to call Run concurrently
// from multiple goroutines (each run is independent). Full-mode runs acquire a
// per-type NATS KV lock so the same type is not reindexed simultaneously across
// pods. cdc_repair drains take no distributed lock — concurrent drains are safe
// because targeted reindex publishes only idempotent projections and the
// revision-conditional marker delete is the sole race guard.
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
	grantIndex            port.KeyContactGrantIndex

	// cdc_repair collaborators (optional; only the repair path uses them).
	repairStore          port.CDCRepairStore
	quotaGauge           port.SalesforceQuotaGauge
	repairQuotaThreshold float64

	// Batch readers for targeted (items) reindex of the two prod volume drivers.
	// When wired, runTargeted fetches project_membership / key_contact in one
	// SOQL batch instead of per-item Assemble*. When nil, targeted falls back to
	// the per-item reindexItem path (preserving pre-batch behavior).
	membershipBatch port.MembershipBatchReader
	keyContactBatch port.KeyContactBatchReader
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

// WithRepairStore wires the durable CDC quota-repair queue drained by cdc_repair
// runs.
func WithRepairStore(s port.CDCRepairStore) RunnerOption {
	return func(r *Runner) { r.repairStore = s }
}

// WithKeyContactGrantIndex wires the durable record of published key_contact FGA
// grants, so a key_contact reindex populates it for contacts whose grant predates
// the index.
func WithKeyContactGrantIndex(i port.KeyContactGrantIndex) RunnerOption {
	return func(r *Runner) { r.grantIndex = i }
}

// WithQuotaGauge wires the Salesforce quota gauge used to gate cdc_repair drains.
func WithQuotaGauge(g port.SalesforceQuotaGauge) RunnerOption {
	return func(r *Runner) { r.quotaGauge = g }
}

// WithMembershipBatchReader wires the batch SOQL reader used to fetch
// project_membership records in targeted (items) reindex.
func WithMembershipBatchReader(b port.MembershipBatchReader) RunnerOption {
	return func(r *Runner) { r.membershipBatch = b }
}

// WithKeyContactBatchReader wires the batch SOQL reader used to fetch
// key_contact records in targeted (items) reindex.
func WithKeyContactBatchReader(b port.KeyContactBatchReader) RunnerOption {
	return func(r *Runner) { r.keyContactBatch = b }
}

// WithRepairQuotaThreshold overrides the cdc_repair quota gate threshold
// (fraction 0–1). Values outside (0,1] are ignored.
func WithRepairQuotaThreshold(threshold float64) RunnerOption {
	return func(r *Runner) {
		if threshold > 0 && threshold <= 1 {
			r.repairQuotaThreshold = threshold
		}
	}
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
		repairQuotaThreshold:  defaultAdminReindexQuotaThreshold,
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
	modeRepair   runMode = "repair"
)

// ClassifyMode returns the run mode for the given request.
func ClassifyMode(req BackfillRequest) runMode {
	if req.CDCRepair {
		return modeRepair
	}
	if len(req.Items) > 0 {
		return modeTargeted
	}
	if req.Since != nil {
		return modeFiltered
	}
	return modeFull
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
	log.InfoContext(ctx, "backfill started", "type", req.Type)
	defer log.InfoContext(ctx, "backfill complete")

	switch mode {
	case modeTargeted:
		r.runTargeted(ctx, log, req)
		return nil
	case modeRepair:
		// Repair is driven by the handler via PrepareRepair + RunRepair so the
		// quota gate and selected_count are computed synchronously. Run should
		// not be used for repair; guard against misuse.
		log.ErrorContext(ctx, "repair mode must be run via PrepareRepair/RunRepair, not Run")
		return fmt.Errorf("repair mode is not supported through Run")
	}

	// full / filtered: exactly one type per request (BREAKING: no all-types).
	t := req.Type
	if mode == modeFull {
		log.WarnContext(ctx, "full reindex started", "type", t, "full_reindex_started", true)
	}

	var err error
	if mode == modeFull && r.natsClient != nil {
		release, lockErr := natspkg.AcquireFullRunLock(ctx, r.natsClient, req.RunID, t)
		if lockErr != nil {
			log.WarnContext(ctx, "full reindex skipped — lock held",
				"type", t,
				"full_reindex_rejected_lock_held", true,
				"error", lockErr)
			return nil
		}
		err = r.runType(ctx, log, req, t)
		release()
	} else {
		err = r.runType(ctx, log, req, t)
	}

	if errors.Is(err, errQuotaStop) {
		// Clean stop: the quota guard tripped at start or between pages. Greppable
		// via stopped_early=true. Return the error so the avatar-backfill Job exits
		// non-zero and reschedules; the fire-and-forget HTTP path ignores it.
		log.WarnContext(ctx, "backfill stopped early — salesforce quota threshold reached",
			"type", t, "stopped_early", true, "publish_failed_for_backfill_repair", true)
		return err
	}
	if err != nil {
		log.ErrorContext(ctx, "backfill type failed", "type", t, "error", err)
		return fmt.Errorf("backfill failed for type %q: %w", t, err)
	}

	log.InfoContext(ctx, "backfill summary", "type", t)
	return nil
}

func (r *Runner) runType(ctx context.Context, log *slog.Logger, req BackfillRequest, sfType string) error {
	// Start gate covers the direct-Run path (e.g. the avatar-backfill Job), which
	// bypasses the handler's synchronous gate. Fails open when no gauge is wired.
	if gateErr := r.checkQuotaGate(ctx); gateErr != nil {
		log.WarnContext(ctx, "backfill refused at start — salesforce quota threshold reached",
			"type", sfType, "error", gateErr, "stopped_early", true,
			"publish_failed_for_backfill_repair", true)
		return errQuotaStop
	}

	var total, published int

	logPage := func(pageLen int) {
		log.InfoContext(ctx, "backfill page processed",
			"type", sfType, "page_size", pageLen,
			"total_so_far", total, "published_so_far", published)
	}

	switch sfType {
	case entityTypeB2BOrg:
		return r.iter.IterB2BOrgs(ctx, req.Since, req.Until, func(orgs []*model.B2BOrg) error {
			if r.midRunQuotaExceeded() {
				return errQuotaStop
			}
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
		return r.iter.IterProjectMemberships(ctx, req.Since, req.Until, func(pms []*model.ProjectMembership) error {
			if r.midRunQuotaExceeded() {
				return errQuotaStop
			}
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
		if req.Since != nil || req.Until != nil {
			log.WarnContext(ctx, "since/until filter on key_contact only checks Project_Role__c.LastModifiedDate; Contact/Asset field changes are not captured",
				"since_filter_misses_joined_fields", true)
		}
		return r.iter.IterKeyContacts(ctx, req.Since, req.Until, func(kcs []*model.KeyContact) error {
			if r.midRunQuotaExceeded() {
				return errQuotaStop
			}
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
					PublishKeyContactFGA(ctx, r.publisher, r.grantIndex, kc)
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
		for i, uid := range orgUIDs {
			// Flat loop (no pages): check the passive gauge every N iterations so a
			// long key list still stops mid-run before exhausting quota.
			if i > 0 && i%settingsQuotaCheckInterval == 0 && r.midRunQuotaExceeded() {
				return errQuotaStop
			}
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

// reindexOutcome is the result of reindexing a single entity, shared by the
// targeted path and the cdc_repair drain.
type reindexOutcome int

const (
	// outcomeIssued means every required publish helper call was invoked (or, in
	// dry-run, the record was fetched and would have been published).
	outcomeIssued reindexOutcome = iota
	// outcomeNotFound means the record no longer exists (delete already handled
	// elsewhere; a repair marker for it can be safely removed).
	outcomeNotFound
	// outcomeRetry means a fetch, dependency-resolution, persistence, or partial
	// projection failure occurred; a repair marker must be retained.
	outcomeRetry
)

// newChildUIDsFetcher returns a per-run memoised child-UID fetcher so sibling
// orgs sharing a parent don't each trigger a separate SOQL call.
func (r *Runner) newChildUIDsFetcher(ctx context.Context) func(uid string) ([]string, error) {
	cache := map[string][]string{}
	return func(uid string) ([]string, error) {
		if v, ok := cache[uid]; ok {
			return v, nil
		}
		uids, err := r.b2bReader.FetchChildUIDsByParentUID(ctx, uid)
		if err == nil {
			cache[uid] = uids
		}
		return uids, err
	}
}

// reindexItem reindexes a single (sfType, uid) and returns the outcome. It is
// the shared per-item projection used by targeted reindex and cdc_repair. All
// publish helpers are idempotent, so a repeated call is safe. A dependency
// failure (child lookup, unresolved project UID, settings persist) is classified
// as retry so a repair marker is retained rather than deleted.
func (r *Runner) reindexItem(ctx context.Context, log *slog.Logger, req BackfillRequest, sfType, uid string, fetchChildUIDs func(string) ([]string, error)) reindexOutcome {
	switch sfType {
	case entityTypeB2BOrg:
		org, err := r.b2bReader.GetB2BOrg(ctx, uid)
		if err != nil {
			return logItemFetchOutcome(ctx, log, sfType, uid, err)
		}
		// Resolve all dependencies before publishing so we never leave a partial
		// projection (indexer without the matching parent FGA tuple).
		childUIDs, childErr := fetchChildUIDs(org.UID)
		if childErr != nil {
			log.WarnContext(ctx, "failed to fetch child UIDs — retaining for retry",
				"type", sfType, "uid", org.UID, "error", childErr, "publish_failed_for_backfill_repair", true)
			return outcomeRetry
		}
		var parentChildren []string
		if org.ParentUID != "" {
			parentChildren, err = fetchChildUIDs(org.ParentUID)
			if err != nil {
				log.WarnContext(ctx, "failed to fetch parent children — retaining for retry",
					"type", sfType, "uid", org.UID, "parent_uid", org.ParentUID, "error", err, "publish_failed_for_backfill_repair", true)
				return outcomeRetry
			}
		}
		if req.DryRun {
			return outcomeIssued
		}
		org.IsParent = len(childUIDs) > 0
		PublishB2BOrgIndexer(ctx, r.publisher, org, indexerConstants.ActionUpdated)
		PublishB2BOrgGlobalAdminFGA(ctx, r.publisher, org, r.globalOrgAdminTeamUID)
		if org.ParentUID != "" {
			PublishB2BOrgParentFGA(ctx, r.publisher, org, parentChildren)
		}
		return outcomeIssued

	case entityTypeProjectMembership:
		pm, _, err := r.pmReader.AssembleProjectMembership(ctx, uid)
		if err != nil {
			return logItemFetchOutcome(ctx, log, sfType, uid, err)
		}
		if req.DryRun {
			return outcomeIssued
		}
		resolvedUID, ok := resolveProjectUID(ctx, r.resolver, pm.ProjectSlug, pm.ProjectUID)
		if !ok {
			// Unresolved project UID is a partial projection — publish FGA
			// (preserving refs) but retain the marker for a later drain.
			log.ErrorContext(ctx, "skipping project_membership indexer publish; project_uid unresolved — publishing OpenFGA only",
				"uid", pm.UID, "slug", pm.ProjectSlug, "publish_failed_for_backfill_repair", true)
			PublishProjectMembershipFGAPreservingMissingRefs(ctx, r.publisher, pm)
			return outcomeRetry
		}
		pm.ProjectUID = resolvedUID
		PublishProjectMembershipIndexer(ctx, r.publisher, pm, indexerConstants.ActionUpdated)
		PublishProjectMembershipFGA(ctx, r.publisher, pm)
		return outcomeIssued

	case entityTypeKeyContact:
		kc, _, err := r.kcReader.AssembleKeyContact(ctx, uid)
		if err != nil {
			return logItemFetchOutcome(ctx, log, sfType, uid, err)
		}
		if req.DryRun {
			return outcomeIssued
		}
		resolvedUID, ok := resolveProjectUID(ctx, r.resolver, kc.ProjectSlug, kc.ProjectUID)
		if !ok {
			log.ErrorContext(ctx, "skipping key_contact indexer publish; project_uid unresolved — publishing OpenFGA only",
				"uid", kc.UID, "slug", kc.ProjectSlug, "publish_failed_for_backfill_repair", true)
			PublishKeyContactFGA(ctx, r.publisher, r.grantIndex, kc)
			return outcomeRetry
		}
		kc.ProjectUID = resolvedUID
		PublishKeyContactIndexer(ctx, r.publisher, kc, indexerConstants.ActionUpdated)
		PublishKeyContactFGA(ctx, r.publisher, r.grantIndex, kc)
		return outcomeIssued

	case entityTypeB2BOrgSettings:
		if r.settingsReader == nil {
			log.ErrorContext(ctx, "b2b_org_settings reindex requires settingsReader — wiring error",
				"uid", uid, "publish_failed_for_backfill_repair", true)
			return outcomeRetry
		}
		org, orgErr := r.b2bReader.GetB2BOrg(ctx, uid)
		if orgErr != nil {
			return logItemFetchOutcome(ctx, log, sfType, uid, orgErr)
		}
		settings, revision, settingsErr := r.settingsReader.GetSettings(ctx, uid)
		if settingsErr != nil {
			log.WarnContext(ctx, "targeted item fetch error", "type", sfType, "uid", uid, "error", settingsErr,
				"publish_failed_for_backfill_repair", true)
			return outcomeRetry
		}
		if settings == nil {
			log.WarnContext(ctx, "targeted item not found", "type", sfType, "uid", uid, "not_found", true)
			return outcomeNotFound
		}
		if req.EnrichAvatars && r.userReader != nil {
			r.enrichSettingsAvatars(ctx, log, req, uid, settings)
		}
		if req.DryRun {
			return outcomeIssued
		}
		if req.EnrichAvatars && r.settingsWriter != nil {
			if wErr := r.settingsWriter.UpdateSettings(ctx, settings, revision); wErr != nil {
				if errs.IsConflict(wErr) {
					log.InfoContext(ctx, "settings changed concurrently; skipping (will retry next run)", "uid", uid)
				} else {
					log.WarnContext(ctx, "failed to persist enriched avatars (targeted)", "uid", uid, "error", wErr,
						"publish_failed_for_backfill_repair", true)
				}
				return outcomeRetry
			}
		}
		PublishB2BOrgSettingsIndexer(ctx, r.publisher, org, settings, indexerConstants.ActionUpdated)
		return outcomeIssued

	default:
		log.ErrorContext(ctx, "unhandled reindex type", "type", sfType, "uid", uid)
		return outcomeRetry
	}
}

// logItemFetchOutcome logs a per-item fetch error and maps it to an outcome:
// NotFound → outcomeNotFound; any other error → outcomeRetry.
func logItemFetchOutcome(ctx context.Context, log *slog.Logger, sfType, uid string, err error) reindexOutcome {
	if errs.IsNotFound(err) {
		log.WarnContext(ctx, "targeted item not found", "type", sfType, "uid", uid, "not_found", true)
		return outcomeNotFound
	}
	log.WarnContext(ctx, "targeted item fetch error", "type", sfType, "uid", uid, "error", err,
		"publish_failed_for_backfill_repair", true)
	return outcomeRetry
}

func (r *Runner) runTargeted(ctx context.Context, log *slog.Logger, req BackfillRequest) {
	// PM and KC are the prod volume drivers: when a batch reader is wired, fetch
	// the whole page in one SOQL query instead of per-item Assemble* (~3–5 SF
	// calls/item → ~1 batch). b2b_org stays per-item (see D2), and either type
	// falls back to per-item when its batch reader is unwired (e.g. mock mode).
	switch req.Type {
	case entityTypeProjectMembership:
		if r.membershipBatch != nil {
			r.runTargetedMemberships(ctx, log, req)
			return
		}
	case entityTypeKeyContact:
		if r.keyContactBatch != nil {
			r.runTargetedKeyContacts(ctx, log, req)
			return
		}
	}

	fetchChildUIDs := r.newChildUIDsFetcher(ctx)
	var notFound, published int
	for _, uid := range req.Items {
		switch r.reindexItem(ctx, log, req, req.Type, uid, fetchChildUIDs) {
		case outcomeIssued:
			published++
		case outcomeNotFound:
			notFound++
		case outcomeRetry:
			// Already logged in reindexItem.
		}
	}

	log.InfoContext(ctx, "targeted backfill complete",
		"type", req.Type,
		"total_items", len(req.Items),
		"published", published,
		"not_found", notFound,
		"would_publish_count", len(req.Items)-notFound)
}

// runTargetedMemberships is the batched project_membership arm of targeted
// reindex: one SOQL batch fetch → per record resolveProjectUID + publish. A
// requested SFID absent from the result (soft-deleted or no longer membership-
// eligible) is classified not-found and skipped; a record present in SOQL but
// unconvertible is counted as conversion_error (neither published nor deleted),
// mirroring the CDC batch path. Deliberately no cache-invalidation or
// absent-as-delete convergence (that is LFXV2-2808).
func (r *Runner) runTargetedMemberships(ctx context.Context, log *slog.Logger, req BackfillRequest) {
	memberships, convErrSFIDs, err := r.membershipBatch.FetchMembershipsBySFIDs(ctx, req.Items)
	if err != nil {
		log.ErrorContext(ctx, "targeted batch fetch failed — retry when Salesforce recovers",
			"type", req.Type, "count", len(req.Items), "error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}

	var published int
	for _, pm := range memberships {
		if req.DryRun {
			published++
			continue
		}
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

	returned := makeReturnedSet(memberships, func(pm *model.ProjectMembership) string { return pm.UID }, convErrSFIDs)
	notFound := countAbsent(req.Items, returned)
	logTargetedBatchComplete(ctx, log, req, published, notFound, len(convErrSFIDs))
}

// runTargetedKeyContacts is the batched key_contact arm of targeted reindex.
// See runTargetedMemberships for the not-found / conversion-error semantics.
func (r *Runner) runTargetedKeyContacts(ctx context.Context, log *slog.Logger, req BackfillRequest) {
	contacts, convErrSFIDs, err := r.keyContactBatch.FetchKeyContactsBySFIDs(ctx, req.Items)
	if err != nil {
		log.ErrorContext(ctx, "targeted batch fetch failed — retry when Salesforce recovers",
			"type", req.Type, "count", len(req.Items), "error", err,
			"publish_failed_for_backfill_repair", true)
		return
	}

	var published int
	for _, kc := range contacts {
		if req.DryRun {
			published++
			continue
		}
		uid, ok := resolveProjectUID(ctx, r.resolver, kc.ProjectSlug, kc.ProjectUID)
		if ok {
			kc.ProjectUID = uid
			PublishKeyContactIndexer(ctx, r.publisher, kc, indexerConstants.ActionUpdated)
		} else {
			log.ErrorContext(ctx, "skipping key_contact indexer publish; project_uid unresolved — publishing OpenFGA only",
				"uid", kc.UID, "slug", kc.ProjectSlug, "publish_failed_for_backfill_repair", true)
		}
		PublishKeyContactFGA(ctx, r.publisher, r.grantIndex, kc)
		published++
	}

	returned := makeReturnedSet(contacts, func(kc *model.KeyContact) string { return kc.UID }, convErrSFIDs)
	notFound := countAbsent(req.Items, returned)
	logTargetedBatchComplete(ctx, log, req, published, notFound, len(convErrSFIDs))
}

// countAbsent counts requested SFIDs not present in the returned set (which
// already includes conversion-error SFIDs marked seen, so they are not
// double-counted as not-found).
func countAbsent(requested []string, returned map[string]struct{}) int {
	var absent int
	for _, sfid := range requested {
		if _, ok := returned[sfid]; !ok {
			absent++
		}
	}
	return absent
}

// logTargetedBatchComplete emits the batched targeted summary with a distinct
// conversion_error bucket so published + not_found + conversion_error reconciles
// against total_items.
func logTargetedBatchComplete(ctx context.Context, log *slog.Logger, req BackfillRequest, published, notFound, conversionErrors int) {
	log.InfoContext(ctx, "targeted backfill complete",
		"type", req.Type,
		"total_items", len(req.Items),
		"published", published,
		"not_found", notFound,
		"conversion_error", conversionErrors,
		"would_publish_count", published)
}

// PrepareRepair synchronously gates and selects a cdc_repair page. It refreshes
// the quota reading (falling back to the last valid observation if the active
// refresh fails), returns ServiceUnavailable when the quota is unreadable or
// at/above the admin threshold, and otherwise returns up to repairPageCap
// pending markers for req.Type. There is no distributed lock — concurrent drains
// are safe by design. The caller passes the returned markers to RunRepair.
func (r *Runner) PrepareRepair(ctx context.Context, req BackfillRequest) ([]port.RepairMarker, error) {
	if r.repairStore == nil {
		return nil, errs.NewServiceUnavailable("cdc_repair queue is not configured")
	}
	if r.quotaGauge == nil {
		// Repair fails CLOSED on a missing gauge (unlike the backfill guard, which
		// fails open): a drain deletes durable markers, so it must never run blind.
		return nil, errs.NewServiceUnavailable("cdc_repair quota gauge is not configured")
	}

	// Shared refresh→fallback→threshold core (also used by the backfill guard).
	if gateErr := r.checkQuotaGate(ctx); gateErr != nil {
		return nil, gateErr
	}

	markers, listErr := r.repairStore.ListPending(ctx, req.Type, repairPageCap)
	if listErr != nil {
		return nil, listErr
	}
	return markers, nil
}

// RunRepair drains a selected page of repair markers. For each marker it
// reindexes the record, and on outcomeIssued/outcomeNotFound revision-conditionally
// deletes the marker (the sole race guard); outcomeRetry markers are retained and
// logged for the next drain. Before each item it re-reads the passive gauge and
// stops at the admin threshold without another /limits call. Intended to be
// called in a goroutine with context.WithoutCancel so it outlives the request.
func (r *Runner) RunRepair(ctx context.Context, req BackfillRequest, markers []port.RepairMarker) {
	log := slog.With("run_id", req.RunID, "component", "backfill", "mode", string(modeRepair), "type", req.Type)
	log.InfoContext(ctx, "repair drain started", "selected", len(markers))

	fetchChildUIDs := r.newChildUIDsFetcher(ctx)
	var issued, notFound, retryRetained, stoppedEarly int
	for i, m := range markers {
		if r.midRunQuotaExceeded() {
			stoppedEarly = len(markers) - i
			log.WarnContext(ctx, "repair drain stopping mid-page — quota threshold reached",
				"processed", i, "remaining", stoppedEarly, "publish_failed_for_backfill_repair", true)
			break
		}

		outcome := r.reindexItem(ctx, log, req, m.Type, m.SFID, fetchChildUIDs)
		if outcome == outcomeRetry {
			// Retain the marker (fetch/dependency/partial-projection failure).
			retryRetained++
			log.WarnContext(ctx, "repair item retained for next drain", "type", m.Type, "uid", m.SFID)
			continue
		}

		// issued or not_found → revision-conditionally delete the marker (the
		// sole race guard). A conflict means the consumer re-skipped or another
		// drain acted; retain the newer marker for the next drain.
		if delErr := r.repairStore.DeletePending(ctx, m.Type, m.SFID, m.Revision); delErr != nil {
			retryRetained++
			log.WarnContext(ctx, "repair marker retained — conditional delete failed",
				"type", m.Type, "uid", m.SFID, "error", delErr)
			continue
		}
		if outcome == outcomeNotFound {
			notFound++
		} else {
			issued++
		}
	}

	log.InfoContext(ctx, "repair drain complete",
		"selected", len(markers),
		"issued", issued,
		"not_found", notFound,
		"retry_retained", retryRetained,
		"stopped_early", stoppedEarly)
}

// midRunQuotaExceeded reports whether the passive quota reading has reached the
// admin threshold. It does not issue a /limits call — the run was already gated
// at start; this only stops a long run if live traffic pushes usage up mid-run.
// Shared by the cdc_repair drain (per item) and the backfill guard (per page /
// every N settings iterations). Fails open (returns false) when the gauge is
// nil or has never observed a reading.
func (r *Runner) midRunQuotaExceeded() bool {
	if r.quotaGauge == nil {
		return false
	}
	snap := r.quotaGauge.Snapshot()
	if !snap.Observed() {
		return false
	}
	return snap.Ratio() >= r.repairQuotaThreshold
}

// checkQuotaGate is the shared refresh→fallback→threshold core reused by the
// cdc_repair gate (PrepareRepair) and the full/filtered backfill start gate. It
// issues one active /limits refresh, falls back to the last valid Snapshot when
// the refresh fails, and returns ServiceUnavailable when the quota is truly
// unreadable or at/above the admin threshold.
//
// It fails OPEN on a nil gauge (returns nil) so the backfill guard preserves the
// pre-guard ungated behavior when no gauge is wired. PrepareRepair guards nil
// separately (fail-closed) before calling this, so repair never reaches the
// fail-open path.
func (r *Runner) checkQuotaGate(ctx context.Context) error {
	if r.quotaGauge == nil {
		return nil
	}
	snap, err := r.quotaGauge.Refresh(ctx)
	if err != nil {
		// Fall back to the last valid observation; only refuse when nothing has
		// ever been observed (truly unreadable).
		snap = r.quotaGauge.Snapshot()
		if !snap.Observed() {
			return errs.NewServiceUnavailable("salesforce quota is currently unreadable; retry shortly")
		}
	}
	if snap.Ratio() >= r.repairQuotaThreshold {
		return errs.NewServiceUnavailable(fmt.Sprintf(
			"salesforce quota at/above admin threshold (%.1f%% >= %.1f%%); retry after quota resets",
			snap.Ratio()*100, r.repairQuotaThreshold*100))
	}
	return nil
}

// GateBackfillStart is the handler-facing synchronous quota gate for the
// full/filtered backfill paths. Targeted (items) and repair modes are exempt —
// targeted is bounded (≤100 items, ~1 SOQL batch) and is the operator's surgical
// incident tool that must stay available under quota pressure; repair has its own
// gate via PrepareRepair. Returns ServiceUnavailable (→ HTTP 503) when at/above
// threshold so the operator gets immediate feedback before the async run starts.
func (r *Runner) GateBackfillStart(ctx context.Context, req BackfillRequest) error {
	switch ClassifyMode(req) {
	case modeTargeted, modeRepair:
		return nil
	}
	return r.checkQuotaGate(ctx)
}
