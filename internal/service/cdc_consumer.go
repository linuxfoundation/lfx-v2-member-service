// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// cdcTracer is safe to initialize at package level — otel.Tracer() returns a
// delegating tracer that forwards to whatever TracerProvider is registered at
// call time, so otel.SetTracerProvider() updates it regardless of init order.
var cdcTracer = otel.Tracer("github.com/linuxfoundation/lfx-v2-member-service/internal/service")

// defaultQuotaSkipThreshold is the fraction of the daily Salesforce REST API
// quota at which the CDC consumer begins skipping upsert re-fetches to
// preserve remaining quota for user-facing HTTP reads. Configurable via
// CDC_QUOTA_SKIP_THRESHOLD (float, 0–1). Default: 0.95.
const defaultQuotaSkipThreshold = 0.95

// defaultQuotaRefreshStaleAfter is how old the quota observation may be before
// the guard actively refreshes it (one lightweight /limits GET) rather than
// skipping on a possibly-frozen reading. This breaks the "quota death spiral":
// during a skip loop the consumer makes no Salesforce calls, so the passively
// updated gauge never refreshes on its own. Configurable via
// CDC_QUOTA_REFRESH_STALE_AFTER (Go duration). Default: 5m. Set to 0 to disable
// active refresh.
const defaultQuotaRefreshStaleAfter = 5 * time.Minute

// repairLogIDChunk caps how many skipped IDs are emitted per ERROR log entry
// when a repair-queue write fails, so a large batch does not produce one giant
// log line.
const repairLogIDChunk = 100

// CDCConsumer consumes normalized CDCEvents from a CDCSubscriber and dispatches
// each one to the appropriate handler. It is the single active consumer in
// consumer mode (enforced at the Kubernetes level via replicas:1 + Recreate —
// no application-level lease is needed).
//
// For each entity the handler:
//  1. Separates DELETE from UPSERT record IDs in the event.
//  2. For UPSERT: checks the quota guard, captures old record state (for
//     reparenting diff on b2b_org), invalidates the sObject cache, then issues
//     a single batched SOQL fetch for all IDs in the event.
//  3. IDs absent from the SOQL result (soft-deleted / no longer qualifying) are
//     routed to the delete path for index/FGA convergence.
//  4. Present records are published via indexer + FGA fan-out messages.
//  5. On DELETE: publishes a delete indexer event; no re-fetch.
type CDCConsumer struct {
	subscriber              port.CDCSubscriber
	resolver                port.ProjectResolver
	b2bOrgReader            port.B2BOrgReader
	membershipBatch         port.MembershipBatchReader
	keyContactBatch         port.KeyContactBatchReader
	keyContactsByMembership port.KeyContactsByMembershipReader
	accountBatch            port.AccountBatchReader
	cacheInvalidator        port.CacheInvalidator
	publisher               port.MemberPublisher
	quotaGauge              port.SalesforceQuotaGauge
	quotaSkipThreshold      float64
	quotaRefreshStaleAfter  time.Duration
	repairStore             port.CDCRepairStore
	grantIndex              port.KeyContactGrantIndex
	globalOrgAdminTeamName  string
	b2bOrgAuditorTeams      []string
	userReader              port.UserReader
	orgSettings             OrgSettingsPrincipalWriter
	settingsReader          port.B2BOrgSettingsReader
	// storage supplies ListKeyContactsForOrg for org-dashboard reconciliation
	// when an Inactive contact's provisioning is skipped. Nil disables the scan.
	storage port.MemberReader

	// refreshMu guards lastRefreshAttemptAt so failed/attempted refreshes are
	// throttled to at most once per staleness window even under concurrent calls.
	refreshMu            sync.Mutex
	lastRefreshAttemptAt time.Time
}

// CDCConsumerOption configures a CDCConsumer.
type CDCConsumerOption func(*CDCConsumer)

func WithCDCSubscriber(s port.CDCSubscriber) CDCConsumerOption {
	return func(o *CDCConsumer) { o.subscriber = s }
}

// WithCDCProjectResolver injects the resolver used to populate ProjectUID from a
// record's project slug before publishing, giving CDC re-publishes parity with
// the backfill and HTTP read paths. When nil (e.g. mock mode), ProjectUID is
// left unchanged.
func WithCDCProjectResolver(r port.ProjectResolver) CDCConsumerOption {
	return func(o *CDCConsumer) { o.resolver = r }
}

func WithCDCB2BOrgReader(r port.B2BOrgReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.b2bOrgReader = r }
}

func WithCDCMembershipBatchReader(r port.MembershipBatchReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.membershipBatch = r }
}

func WithCDCKeyContactBatchReader(r port.KeyContactBatchReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.keyContactBatch = r }
}

func WithCDCKeyContactsByMembershipReader(r port.KeyContactsByMembershipReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.keyContactsByMembership = r }
}

func WithCDCAccountBatchReader(r port.AccountBatchReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.accountBatch = r }
}

func WithCDCCacheInvalidator(i port.CacheInvalidator) CDCConsumerOption {
	return func(o *CDCConsumer) { o.cacheInvalidator = i }
}

func WithCDCPublisher(p port.MemberPublisher) CDCConsumerOption {
	return func(o *CDCConsumer) { o.publisher = p }
}

// WithCDCQuotaGauge injects the Salesforce API usage gauge used by the quota
// guard. When nil, the guard is disabled (no quota checking).
func WithCDCQuotaGauge(g port.SalesforceQuotaGauge) CDCConsumerOption {
	return func(o *CDCConsumer) { o.quotaGauge = g }
}

// WithCDCRepairStore injects the durable repair queue. When set, every upsert
// skipped by the quota guard records a pending marker so the record can be
// repaired later via POST /admin/reindex {cdc_repair:true}. When nil, skips are
// only logged (no durable queue).
func WithCDCRepairStore(s port.CDCRepairStore) CDCConsumerOption {
	return func(o *CDCConsumer) { o.repairStore = s }
}

// WithCDCKeyContactGrantIndex injects the durable record of published
// key_contact FGA grants. When set, upserts record the grant they publish and
// deletes use it to address the revoke; when nil, a delete falls back to the
// unaddressable remove that predates the index.
// WithCDCB2BOrgSettingsReader injects the org-settings reader used to rebuild
// per-user writer and auditor grants when Salesforce restores a deleted org.
// The settings record is the authoritative source for those principals and is
// untouched by delete_access, which withdraws FGA tuples, not KV records. When
// nil (mock mode), an UNDELETE restores only the team grants, as before.
func WithCDCB2BOrgSettingsReader(r port.B2BOrgSettingsReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.settingsReader = r }
}

func WithCDCKeyContactGrantIndex(i port.KeyContactGrantIndex) CDCConsumerOption {
	return func(o *CDCConsumer) { o.grantIndex = i }
}

func WithCDCGlobalOrgAdminTeamName(name string) CDCConsumerOption {
	return func(o *CDCConsumer) { o.globalOrgAdminTeamName = name }
}

// WithCDCB2BOrgAuditorTeams sets the LF team names granted blanket auditor
// access on every org the CDC consumer upserts.
func WithCDCB2BOrgAuditorTeams(teams []string) CDCConsumerOption {
	return func(o *CDCConsumer) { o.b2bOrgAuditorTeams = teams }
}

func WithCDCUserReader(r port.UserReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.userReader = r }
}

func WithCDCOrgSettings(w OrgSettingsPrincipalWriter) CDCConsumerOption {
	return func(o *CDCConsumer) { o.orgSettings = w }
}

// WithCDCStorage injects the reader used to scan an org's key contacts for
// org-dashboard reconciliation when an Inactive contact's provisioning is
// skipped. Nil disables reconciliation; the gate on Inactive still applies.
func WithCDCStorage(r port.MemberReader) CDCConsumerOption {
	return func(o *CDCConsumer) { o.storage = r }
}

// NewCDCConsumer constructs a CDCConsumer. The quota-skip threshold is read
// from CDC_QUOTA_SKIP_THRESHOLD (float, 0–1 inclusive; default 0.95) at
// construction time so it is set once at startup rather than on every event.
// Set to 0 to always skip upsert fetches (useful in tests/emergencies); set to
// 1 to disable the guard entirely.
func NewCDCConsumer(opts ...CDCConsumerOption) *CDCConsumer {
	threshold := defaultQuotaSkipThreshold
	if raw := os.Getenv("CDC_QUOTA_SKIP_THRESHOLD"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && v <= 1 {
			threshold = v
		}
	}

	// CDC_QUOTA_REFRESH_STALE_AFTER (Go duration): how old the quota observation
	// may be before the guard actively refreshes it. A non-positive value
	// disables active refresh (the guard falls back to the passive reading).
	staleAfter := defaultQuotaRefreshStaleAfter
	if raw := os.Getenv("CDC_QUOTA_REFRESH_STALE_AFTER"); raw != "" {
		if v, err := time.ParseDuration(raw); err == nil {
			staleAfter = v
		}
	}

	o := &CDCConsumer{quotaSkipThreshold: threshold, quotaRefreshStaleAfter: staleAfter}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Run subscribes to channel, processes events until ctx is cancelled, and
// persists the replay cursor after each event. It blocks until ctx is done.
func (o *CDCConsumer) Run(ctx context.Context, channel string, replay port.ReplayStore) error {
	replayID, err := replay.Load(ctx, channel)
	if err != nil {
		return err
	}

	eventCh, err := o.subscriber.Subscribe(ctx, channel, replayID, replay)
	if err != nil {
		return err
	}

	for event := range eventCh {
		if err := o.processWithAuthorizationRetry(ctx, channel, event); err != nil {
			return err
		}

		// Use a short-lived background context for the Save so that a
		// graceful shutdown (which cancels ctx) does not prevent the last
		// replay cursor from being committed. Without this the final event
		// would be re-processed on every restart.
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		saveErr := replay.Save(saveCtx, channel, event.ReplayID)
		saveCancel()
		if saveErr != nil {
			slog.WarnContext(ctx, "cdc: failed to save replay cursor",
				"channel", channel, "error", saveErr)
		}
	}

	return ctx.Err()
}

const (
	authorizationRetryInitialDelay = 100 * time.Millisecond
	authorizationRetryMaxDelay     = 30 * time.Second
)

func (o *CDCConsumer) processWithAuthorizationRetry(
	ctx context.Context,
	channel string,
	event model.CDCEvent,
) error {
	retryDelay := authorizationRetryInitialDelay
	for {
		handleErr := o.processEvent(channel, event)
		if handleErr != nil {
			slog.ErrorContext(ctx, "cdc: event handling failed",
				"entity", event.Entity,
				"change_type", event.ChangeType,
				"record_ids", event.RecordIDs,
				"error", handleErr,
			)
		}
		if !errors.Is(handleErr, errPurgeUnrecorded) && !errors.Is(handleErr, errRestoreIncomplete) &&
			!errors.Is(handleErr, errKeyContactRevokeIncomplete) {
			return nil
		}

		// A retry re-runs the whole event by default. When the handler
		// attached the subset of IDs that actually still need work, shrink
		// to that subset so already-settled IDs are not redone.
		if ids := extractRetryIDs(handleErr); len(ids) > 0 {
			slog.WarnContext(ctx, "cdc: retrying event with failed subset",
				"entity", event.Entity, "record_ids", ids)
			event.RecordIDs = ids
		}

		slog.ErrorContext(ctx, "cdc: authorization change incomplete — retrying event before advancing",
			"entity", event.Entity,
			"record_ids", event.RecordIDs,
			"error", handleErr,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay):
		}
		retryDelay = min(retryDelay*2, authorizationRetryMaxDelay)
	}
}

func (o *CDCConsumer) processEvent(channel string, event model.CDCEvent) error {
	handleCtx, handleCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer handleCancel()
	handleCtx, span := cdcTracer.Start(handleCtx, "salesforce.cdc.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "salesforce"),
			attribute.String("messaging.destination.name", channel),
			attribute.String("messaging.operation.type", "process"),
			attribute.String("cdc.entity", event.Entity),
			attribute.String("cdc.change_type", string(event.ChangeType)),
			attribute.Int("cdc.record_count", len(event.RecordIDs)),
		),
	)
	defer span.End()

	err := o.handle(handleCtx, event)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// handle dispatches a single CDCEvent to the correct entity handler.
func (o *CDCConsumer) handle(ctx context.Context, event model.CDCEvent) error {
	// GAP_* change types signal that Salesforce could not deliver granular
	// events (overflow, create gap, etc.). We re-fetch the record as an upsert
	// but log a WARN so operators know granular delivery was interrupted and
	// can cross-check /admin/reindex if needed.
	if strings.HasPrefix(string(event.ChangeType), "GAP_") {
		slog.WarnContext(ctx, "cdc: GAP event received — granular delivery interrupted, treating as upsert",
			"entity", event.Entity,
			"change_type", event.ChangeType,
			"record_ids", event.RecordIDs,
		)
	}

	switch event.Entity {
	case "Account":
		return o.handleAccount(ctx, event)
	case "Asset":
		return o.handleAsset(ctx, event)
	case "Project_Role__c":
		return o.handleProjectRole(ctx, event)
	default:
		slog.DebugContext(ctx, "cdc: unhandled entity, skipping", "entity", event.Entity)
		return nil
	}
}

// isDelete reports whether the given change type should be routed to the delete
// path. Exact equality is used to avoid matching UNDELETE (which HasSuffix
// on "DELETE" would incorrectly catch).
func isDelete(ct model.CDCChangeType) bool {
	return ct == model.CDCChangeDelete || ct == model.CDCChangeGapDelete
}

// isRestore reports whether Salesforce says a deleted record was restored.
// Exact equality avoids treating unrelated GAP_* events as restores.
func isRestore(ct model.CDCChangeType) bool {
	return ct == model.CDCChangeUndelete || ct == model.CDCChangeGapUndelete
}

// quotaExceeded reports whether the Salesforce REST API quota has been consumed
// beyond the configured threshold, refreshing a stale reading first so a skip
// loop cannot freeze the gauge (the "quota death spiral"). When it decides to
// skip, it also records each affected record to the durable repair queue.
//
// When quotaGauge is nil, the threshold is ≥ 1, or no valid reading has ever
// been observed (even after an attempted refresh), this returns false
// (fail-open).
func (o *CDCConsumer) quotaExceeded(ctx context.Context, entity string, ids []string) bool {
	exceeded, snap := o.quotaGuardExceeded(ctx, entity)
	if !exceeded {
		return false
	}

	slog.WarnContext(ctx, "cdc: Salesforce API quota threshold reached — skipping upsert fetch; use /admin/reindex to repair",
		"entity", entity,
		"record_count", len(ids),
		"api_usage_current", snap.Current,
		"api_usage_limit", snap.Limit,
		"threshold", o.quotaSkipThreshold,
		"publish_failed_for_backfill_repair", true,
	)
	o.recordSkippedForRepair(ctx, entity, ids)
	return true
}

// quotaGuardExceeded is the core quota-threshold check shared by quotaExceeded
// (the batched upsert path, which also logs and queues repair markers for the
// skipped IDs) and quotaGuardedLister (the per-record live recheck path,
// which has no batch of IDs to queue). entity is used only for the
// refresh-failure log line.
func (o *CDCConsumer) quotaGuardExceeded(ctx context.Context, entity string) (bool, port.QuotaSnapshot) {
	if o.quotaGauge == nil {
		return false, port.QuotaSnapshot{}
	}
	if o.quotaSkipThreshold >= 1 {
		// Threshold of 1 means "never skip" — guard disabled regardless of usage.
		return false, port.QuotaSnapshot{}
	}

	snap := o.quotaGauge.Snapshot()

	// Break the spiral: if the reading is stale (or never observed) and we have
	// not already attempted a refresh within the window, actively refresh so the
	// passive transport records a fresh observation before we decide to skip.
	if o.shouldAttemptQuotaRefresh(snap) {
		refreshed, err := o.quotaGauge.Refresh(ctx)
		if err != nil {
			slog.WarnContext(ctx, "cdc: quota refresh failed; evaluating last known reading",
				"entity", entity, "error", err)
		} else {
			snap = refreshed
		}
	}

	if !snap.Observed() {
		// Never observed a valid reading (and refresh, if any, did not produce
		// one) — fail open.
		return false, snap
	}

	return snap.Ratio() >= o.quotaSkipThreshold, snap
}

// errQuotaGuardSkipped reports that a live key_contact sibling recheck was
// skipped because the Salesforce quota guard is active. It must read as
// uncertain, not as an empty sibling list: an empty result would fake
// certainty that no sibling justifies the pair.
var errQuotaGuardSkipped = errors.New("live key_contact sibling recheck skipped: Salesforce quota threshold reached")

// quotaGuardedLister wraps a LIVE membershipKeyContactLister so the CDC delete
// path's per-record recheck consults the quota guard before every Salesforce
// fetch, instead of the O(N) unguarded calls a multi-ID delete event would
// otherwise make. These rechecks must stay live (never prefetched), so
// batching is not an option; consulting the guard per fetch is.
type quotaGuardedLister struct {
	inner membershipKeyContactLister
	o     *CDCConsumer
}

func (l quotaGuardedLister) ListKeyContactsForMembership(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	if exceeded, _ := l.o.quotaGuardExceeded(ctx, "Project_Role__c"); exceeded {
		return nil, errQuotaGuardSkipped
	}
	return l.inner.ListKeyContactsForMembership(ctx, membershipUID)
}

// quotaGuardedLiveSiblingLister builds the quota-aware LIVE recheck lister
// used throughout the CDC key_contact delete path: the same live reader
// siblingListerFor wraps, gated so quota pressure surfaces as an error
// (holding the replay cursor via errKeyContactRevokeIncomplete) instead of
// hammering Salesforce. The quota gate wraps only the base fetch, with the
// email resolver layered on afterward exactly as siblingListerFor does: the
// gate must not sit above withEmailResolver, or the resulting lister no
// longer exposes usernameByEmailResolver and every sibling reads as
// unresolvable instead of quota-skipped. Returns nil when no reader is wired.
func (o *CDCConsumer) quotaGuardedLiveSiblingLister() membershipKeyContactLister {
	if o.keyContactsByMembership == nil {
		return nil
	}
	gated := quotaGuardedLister{inner: keyContactsByMembershipLister{reader: o.keyContactsByMembership}, o: o}
	return withEmailResolver(gated, o.userReader)
}

// shouldAttemptQuotaRefresh reports whether the guard should issue an active
// /limits refresh now: the reading must be stale (older than the window, or
// never observed) and no refresh may have been attempted within the window.
// It records the attempt time before returning true so failed refreshes are
// throttled to at most once per window (avoiding a spiral on /limits calls).
func (o *CDCConsumer) shouldAttemptQuotaRefresh(snap port.QuotaSnapshot) bool {
	if o.quotaRefreshStaleAfter <= 0 {
		return false // active refresh disabled
	}
	now := time.Now()
	stale := !snap.Observed() || now.Sub(snap.ObservedAt) >= o.quotaRefreshStaleAfter
	if !stale {
		return false
	}

	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()
	if !o.lastRefreshAttemptAt.IsZero() && now.Sub(o.lastRefreshAttemptAt) < o.quotaRefreshStaleAfter {
		return false // already attempted within the window
	}
	o.lastRefreshAttemptAt = now
	return true
}

// reindexTypeForCDCEntity maps a CDC entity name to its primary reindex target
// type. The mapping is fixed and 1:1.
func reindexTypeForCDCEntity(entity string) (string, bool) {
	switch entity {
	case "Account":
		return entityTypeB2BOrg, true
	case "Asset":
		return entityTypeProjectMembership, true
	case "Project_Role__c":
		return entityTypeKeyContact, true
	default:
		return "", false
	}
}

// recordSkippedForRepair writes one pending repair marker per skipped record so
// it can be repaired later. ids are already canonical 18-character SFIDs
// (partitionRecordIDs normalizes before the quota check). Each marker is written
// once; on any failure the exact failed IDs are logged at ERROR (chunked) as the
// recovery backstop. The caller still advances the replay cursor regardless.
func (o *CDCConsumer) recordSkippedForRepair(ctx context.Context, entity string, ids []string) {
	if o.repairStore == nil || len(ids) == 0 {
		return
	}
	reindexType, ok := reindexTypeForCDCEntity(entity)
	if !ok {
		slog.ErrorContext(ctx, "cdc: no repair target mapping for entity — skipped records not queued",
			"entity", entity, "record_count", len(ids))
		return
	}

	var failed []string
	for _, id := range ids {
		if err := o.repairStore.PutPending(ctx, reindexType, id); err != nil {
			failed = append(failed, id)
		}
	}
	if len(failed) == 0 {
		return
	}
	for start := 0; start < len(failed); start += repairLogIDChunk {
		end := start + repairLogIDChunk
		if end > len(failed) {
			end = len(failed)
		}
		slog.ErrorContext(ctx, "cdc: repair-queue write failed for skipped records — recover from these IDs",
			"entity", entity,
			"reindex_type", reindexType,
			"failed_ids", failed[start:end],
			"failed_count", len(failed),
			"publish_failed_for_backfill_repair", true,
		)
	}
}

// errPurgeUnrecorded marks the one CDC failure that must not be forgotten: a
// delete_access purge that was neither delivered nor recorded for recovery.
//
// Every other handler failure is logged and skipped, because /admin/reindex can
// repair it from the live Salesforce record. A purge cannot be repaired that
// way — the record is gone, so a reindex resolves it as not-found and clears
// any marker without re-emitting the purge — and the tuples it should have
// withdrawn outlive the object that justified them. When the recovery marker
// also fails to write, redelivery is the only remaining chance, so this error
// is carried up to the replay-cursor decision instead of being swallowed.
var errPurgeUnrecorded = errors.New("delete_access purge lost with no recovery marker")

// errRestoreIncomplete marks a restored authorization set that was not rebuilt
// from authoritative state and confirmed delivered. Unlike ordinary upserts,
// /admin/reindex does not rebuild these per-user relations, so replay must stop.
var errRestoreIncomplete = errors.New("authorization restore incomplete")

// errKeyContactRevokeIncomplete marks a CDC key_contact delete whose recorded
// grant was not confirmed revoked. The Salesforce record is gone, so no later
// upsert or reindex retries it: replay must stop and redeliver the event.
var errKeyContactRevokeIncomplete = errors.New("key_contact delete revoke incomplete")

// retryIDsError carries the subset of an event's record IDs whose handler
// failed with a sentinel error, so processWithAuthorizationRetry can retry
// only those IDs instead of the whole event. A handler that has no per-ID
// view of a sentinel failure simply does not attach one, and the caller falls
// back to retrying the full event.
type retryIDsError struct {
	ids []string
}

func (e *retryIDsError) Error() string {
	return fmt.Sprintf("retry pending for %d record(s)", len(e.ids))
}

// newRetryIDsError wraps ids as an error for errors.Join, or returns nil when
// ids is empty so joining it is a no-op.
func newRetryIDsError(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return &retryIDsError{ids: ids}
}

// extractRetryIDs walks err's tree, which errors.Join and fmt.Errorf("%w")
// may nest arbitrarily deep, collecting the IDs from every retryIDsError
// found into one deduplicated, order-preserving slice. errors.As is not used
// here because it stops at the first match; a joined tree can carry several.
func extractRetryIDs(err error) []string {
	if err == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if rid, ok := e.(*retryIDsError); ok {
			for _, id := range rid.ids {
				if _, dup := seen[id]; !dup {
					seen[id] = struct{}{}
					ids = append(ids, id)
				}
			}
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			for _, sub := range x.Unwrap() {
				walk(sub)
			}
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		}
	}
	walk(err)
	return ids
}

// deleteAccessMarkerTimeout bounds the detached recovery-marker write in
// recordFailedDeleteAccess. It matches the replay-cursor Save timeout in Run
// because both must still complete while the handler context is being
// cancelled during a graceful shutdown.
const deleteAccessMarkerTimeout = 5 * time.Second

// maxDeleteAccessMarkerAttempts bounds the immediate retry of that write. The
// attempts intentionally share one short detached timeout; if all fail, the
// event-level authorization retry applies its capped exponential backoff before
// trying the whole idempotent purge again.
const maxDeleteAccessMarkerAttempts = 3

// recordFailedDeleteAccess durably records a delete_access that was not
// confirmed delivered — either the publish failed or the flush that confirms
// broker receipt did — so an operator can find and manually re-purge it later. This is deliberately
// not wired into any automated drain: /admin/reindex's targeted repair
// re-fetches and re-upserts the live Salesforce record, which cannot repair a
// purge — the record is gone, so the fetch reports outcomeNotFound and no
// delete_access is re-emitted (see ReindexTypeB2BOrgDeleteAccess). The marker
// exists purely so ListPending(ctx, reindexType, ...) surfaces the exact
// (type, uid) pairs that need a manual delete_access republish.
//
// It reports whether the marker landed. That is what separates a purge that
// failed but is durably recorded — recoverable, so the stream may move on —
// from one that is not recorded at all, where redelivery is the only remaining
// chance and the caller must hold the replay cursor.
//
// The write is detached from ctx and retried because a marker lost here loses
// the purge permanently — see deleteAccessMarkerTimeout and
// maxDeleteAccessMarkerAttempts.
//
// A nil repairStore (mock mode) reports success: there is no store to record
// into, so holding the cursor would stall the stream forever rather than
// preserve anything.
func (o *CDCConsumer) recordFailedDeleteAccess(ctx context.Context, reindexType, uid string) error {
	if o.repairStore == nil {
		return nil
	}

	// Run already hands handlers a context detached from shutdown, so a
	// cancelled caller cannot reach here. What can is that context's 30 s
	// deadline: a handler close to it would fail this write on an expired
	// deadline, and the replay cursor advances regardless, so the purge would
	// be lost with nothing recorded. Dropping the inherited deadline and
	// taking a fresh one removes that dependency on how long the rest of the
	// handler took. WithoutCancel rather than Background so trace and log
	// correlation values survive.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteAccessMarkerTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= maxDeleteAccessMarkerAttempts; attempt++ {
		lastErr = o.repairStore.PutPending(writeCtx, reindexType, uid)
		if lastErr == nil {
			return nil
		}
	}

	slog.ErrorContext(ctx, "cdc: failed to record delete_access failure for manual recovery — holding replay cursor",
		"reindex_type", reindexType, "uid", uid, "error", lastErr,
		"attempts", maxDeleteAccessMarkerAttempts,
		"publish_failed_for_backfill_repair", true)
	return lastErr
}

// purgeFailure builds the error a delete path returns when a purge was not
// delivered, tagging it with errPurgeUnrecorded only when the recovery marker
// also failed to write. markerErr nil means the failure is durably captured and
// the stream may advance; non-nil means redelivery is the last chance.
func purgeFailure(reindexType, uid string, publishErr, markerErr error) error {
	if markerErr == nil {
		return fmt.Errorf("cdc: %s delete_access failed for uid %s: %w", reindexType, uid, publishErr)
	}
	return fmt.Errorf("cdc: %s delete_access failed for uid %s: %w",
		reindexType, uid, errors.Join(publishErr, errPurgeUnrecorded))
}

// confirmDeleteAccessDelivery blocks until the broker acknowledges the purge
// published just before it, recording a recovery marker if it cannot.
//
// Access alone only hands the message to the local NATS connection, so it
// returns nil for a purge the broker never received — a crash or disconnect in
// that window drops the revocation with no error to propagate and nothing to
// mark. The key_contact delete path flushes for the same reason before it
// erases its grant-index entry (see handleProjectRoleDelete). A purge is
// the one message with no second chance: the Salesforce record is gone, so
// neither the next CDC event nor /admin/reindex will re-emit it, and the
// tuples outlive the object they authorized.
func (o *CDCConsumer) confirmDeleteAccessDelivery(ctx context.Context, reindexType, uid string) error {
	if err := o.publisher.Flush(ctx); err != nil {
		return purgeFailure(reindexType, uid, err, o.recordFailedDeleteAccess(ctx, reindexType, uid))
	}
	return nil
}

// partitionRecordIDs normalizes each raw CDC record ID to its canonical 18-char
// SFID and splits the event into delete vs upsert lists. An ID that cannot be
// normalized (wrong length / non-base-62) is logged and skipped, so a malformed
// ID never drives a spurious delete or a cache miss against the 18-char keys
// returned by SOQL.
func partitionRecordIDs(ctx context.Context, entity string, event model.CDCEvent) (deleteIDs, upsertIDs []string) {
	for _, raw := range event.RecordIDs {
		id, err := sfuuid.Normalize18(raw)
		if err != nil {
			slog.WarnContext(ctx, "cdc: skipping record with non-normalizable SFID",
				"entity", entity, "raw_uid", raw, "error", err)
			continue
		}
		if isDelete(event.ChangeType) {
			deleteIDs = append(deleteIDs, id)
		} else {
			upsertIDs = append(upsertIDs, id)
		}
	}
	return
}

// dispatchEntity runs each ID in deleteIDs through deleteHandler (logging
// failures). Callers partition the event's record IDs themselves (via
// partitionRecordIDs) so a caller that needs the delete list before dispatch,
// batching sibling lookups across it, can build deleteHandler from it first.
// Shared by all three entity top-level handlers.
//
// Every delete failure is logged and the loop continues, so one bad ID never
// costs the rest of the batch. Only errPurgeUnrecorded and
// errKeyContactRevokeIncomplete are also returned: a purge that reached
// neither the broker nor the repair bucket, or a key_contact revoke left
// unconfirmed for a record that is already gone, has no repair route left
// except redelivery, which requires the replay cursor to stop. Any other
// failure stays logged-and-continue as before, so a repairable error cannot
// stall the stream.
func (o *CDCConsumer) dispatchEntity(ctx context.Context, entity string, event model.CDCEvent, deleteIDs []string,
	deleteHandler func(context.Context, string) error) error {
	var unrecorded error
	var retryIDs []string
	for _, id := range deleteIDs {
		if err := deleteHandler(ctx, id); err != nil {
			slog.ErrorContext(ctx, "cdc: handler failed",
				"entity", entity, "uid", id, "change_type", event.ChangeType, "error", err)
			if errors.Is(err, errPurgeUnrecorded) || errors.Is(err, errKeyContactRevokeIncomplete) {
				unrecorded = errors.Join(unrecorded, err)
				retryIDs = append(retryIDs, id)
			}
		}
	}
	return errors.Join(unrecorded, newRetryIDsError(retryIDs))
}

// logBatchFetchError logs a handler failure for each ID in a batch when the
// upstream SOQL fetch returns an error.
func logBatchFetchError(ctx context.Context, entity string, ids []string, changeType model.CDCChangeType, err error) {
	for _, id := range ids {
		slog.ErrorContext(ctx, "cdc: handler failed",
			"entity", entity, "uid", id, "change_type", changeType, "error", err)
	}
}

// ── Account (b2b_org) ─────────────────────────────────────────────────────────

func (o *CDCConsumer) handleAccount(ctx context.Context, event model.CDCEvent) error {
	// The upserts run before the error is returned: a lost purge on one ID must
	// not suppress unrelated work carried in the same event.
	deleteIDs, upsertIDs := partitionRecordIDs(ctx, "Account", event)
	err := o.dispatchEntity(ctx, "Account", event, deleteIDs, o.handleAccountDelete)
	if len(upsertIDs) > 0 {
		err = errors.Join(err, o.handleAccountUpsertBatch(ctx, upsertIDs, event.ChangeType))
	}
	return err
}

func (o *CDCConsumer) handleAccountUpsertBatch(ctx context.Context, upsertIDs []string, changeType model.CDCChangeType) error {
	if o.accountBatch == nil {
		slog.WarnContext(ctx, "cdc: accountBatch reader not wired — skipping Account upsert; use /admin/reindex to repair",
			"record_count", len(upsertIDs), "publish_failed_for_backfill_repair", true)
		if isRestore(changeType) {
			return errors.Join(errRestoreIncomplete, errors.New("account batch reader not wired"))
		}
		return nil
	}
	if o.quotaExceeded(ctx, "Account", upsertIDs) {
		if isRestore(changeType) {
			return errors.Join(errRestoreIncomplete, errors.New("account restore skipped by Salesforce quota guard"))
		}
		return nil
	}

	// Capture old record state BEFORE cache eviction for reparenting diff.
	// If the cache is cold, GetB2BOrg returns the post-change record — in that
	// case oldOrg == new org and no reparenting messages are emitted (safe).
	oldOrgs := make(map[string]*model.B2BOrg, len(upsertIDs))
	if !isRestore(changeType) {
		for _, id := range upsertIDs {
			if current, err := o.b2bOrgReader.GetB2BOrg(ctx, id); err == nil {
				oldOrgs[id] = current
			}
		}
	}

	for _, id := range upsertIDs {
		if err := o.cacheInvalidator.InvalidateB2BOrg(ctx, id); err != nil {
			slog.WarnContext(ctx, "cdc: b2b_org cache invalidation failed, continuing",
				"uid", id, "error", err, "publish_failed_for_backfill_repair", true)
		}
	}

	orgs, convErrSFIDs, err := o.accountBatch.FetchAccountsBySFIDs(ctx, upsertIDs)
	if err != nil {
		logBatchFetchError(ctx, "Account", upsertIDs, changeType, err)
		if isRestore(changeType) {
			return errors.Join(errRestoreIncomplete, err)
		}
		return nil
	}

	// SFIDs absent from the SOQL result are soft-deleted or no longer hold a
	// membership Asset — route them to the absent path for *index* convergence.
	// The second case is a live org, so this path withdraws no authorization.
	// SFIDs present but unconvertible are also marked seen so they are not deleted.
	returned := makeReturnedSet(orgs, func(o *model.B2BOrg) string { return o.UID }, convErrSFIDs)
	// handleAccountAbsent always returns nil, so this can never produce a
	// sentinel; captured anyway since the caller already has a return path.
	absentErr := o.handleAbsentAsDelete(ctx, "Account", upsertIDs, returned, o.handleAccountAbsent)

	// One batched query for the whole batch — replaces N per-org FetchChildUIDsByParentUID calls.
	// Include each org's ParentUID so we also have the parent's full child list for the
	// hierarchy tuple emitted below.
	uidSet := make(map[string]struct{}, len(orgs))
	for _, org := range orgs {
		uidSet[org.UID] = struct{}{}
		if org.ParentUID != "" {
			uidSet[org.ParentUID] = struct{}{}
		}
	}
	uids := make([]string, 0, len(uidSet))
	for uid := range uidSet {
		uids = append(uids, uid)
	}
	childMap, childMapErr := o.b2bOrgReader.FetchChildUIDsByParentUIDs(ctx, uids)
	if childMapErr != nil {
		slog.WarnContext(ctx, "cdc: failed to bulk-fetch child UIDs; is_parent may be false for orgs outside successful chunks",
			"error", childMapErr, "publish_failed_for_backfill_repair", true)
		if childMap == nil {
			childMap = map[string][]string{}
		}
	}
	for _, org := range orgs {
		org.IsParent = len(childMap[org.UID]) > 0
	}

	// CDC always passes globalOrgAdminTeamName (not create-only like the writer).
	var restoreErr error
	if isRestore(changeType) && childMapErr != nil {
		restoreErr = fmt.Errorf("read org hierarchy for restore: %w", childMapErr)
	}
	restoreErr = errors.Join(restoreErr, absentErr)
	restorePublished := false
	for _, org := range orgs {
		if isRestore(changeType) {
			PublishB2BOrgIndexer(ctx, o.publisher, org, indexerConstants.ActionUpdated)
			published, err := o.restoreOrgAccess(
				ctx, org, childMap[org.UID], childMap[org.ParentUID], childMapErr == nil)
			restorePublished = restorePublished || published
			restoreErr = errors.Join(restoreErr, err)
			continue
		}

		publishB2BOrgUpsertEvents(ctx, o.b2bOrgReader, o.publisher, oldOrgs[org.UID], org, indexerConstants.ActionUpdated, o.globalOrgAdminTeamName, o.b2bOrgAuditorTeams)
		// Emit the parent hierarchy tuple unconditionally for parented orgs so a
		// CDC-created child org gets its parent + child-list tuples even when no
		// reparent was detected.
		if org.ParentUID != "" {
			PublishB2BOrgParentFGA(ctx, o.publisher, org, childMap[org.ParentUID])
		}
	}
	if restorePublished {
		restoreErr = errors.Join(restoreErr, o.publisher.Flush(ctx))
	}

	slog.InfoContext(ctx, "cdc: account batch published",
		"upsert_count", len(orgs),
		"absent_delete_count", len(upsertIDs)-len(returned))
	if restoreErr != nil {
		return errors.Join(errRestoreIncomplete, restoreErr)
	}
	return nil
}

// makeReturnedSet builds a set of UIDs from a batch-fetch result. Items in
// seenButFailed were present in the SOQL result but could not be converted;
// they are included so the caller does not treat them as absent records.
func makeReturnedSet[T any](items []T, uid func(T) string, seenButFailed []string) map[string]struct{} {
	m := make(map[string]struct{}, len(items)+len(seenButFailed))
	for _, item := range items {
		m[uid(item)] = struct{}{}
	}
	for _, sfid := range seenButFailed {
		m[sfid] = struct{}{}
	}
	return m
}

// handleAbsentAsDelete routes IDs that were requested in a batch upsert but
// absent from the SOQL result (soft-deleted or no longer qualifying) to the
// provided handler for index convergence. Callers pass the *Absent entry point
// rather than the *Delete one: absence does not prove deletion, so this path
// must not withdraw FGA tuples.
//
// Every failure is logged and the loop continues, mirroring dispatchEntity's
// policy. Only errors matching errPurgeUnrecorded, errRestoreIncomplete, or
// errKeyContactRevokeIncomplete are also returned: those have no repair route
// left except redelivery, which requires the replay cursor to stop.
func (o *CDCConsumer) handleAbsentAsDelete(ctx context.Context, entity string, upsertIDs []string, returned map[string]struct{}, deleteHandler func(context.Context, string) error) error {
	var unrecorded error
	var retryIDs []string
	for _, id := range upsertIDs {
		if _, found := returned[id]; !found {
			slog.DebugContext(ctx, "cdc: absent from SOQL result, routing to delete for convergence",
				"entity", entity, "uid", id)
			if delErr := deleteHandler(ctx, id); delErr != nil {
				slog.ErrorContext(ctx, "cdc: handler failed",
					"entity", entity, "uid", id, "change_type", "absent→delete", "error", delErr)
				if errors.Is(delErr, errPurgeUnrecorded) || errors.Is(delErr, errRestoreIncomplete) ||
					errors.Is(delErr, errKeyContactRevokeIncomplete) {
					unrecorded = errors.Join(unrecorded, delErr)
					retryIDs = append(retryIDs, id)
				}
			}
		}
	}
	return errors.Join(unrecorded, newRetryIDsError(retryIDs))
}

// handleAccountDelete handles an Account genuinely deleted in Salesforce.
func (o *CDCConsumer) handleAccountDelete(ctx context.Context, uid string) error {
	o.publishAccountDeleteIndex(ctx, uid)

	// A surviving "team:" subject is expected rather than a failure. fga-sync
	// declines to delete team-subject tuples, so a deleted org keeps the staff
	// reader grant from LFXV2-2937. It confers access to an object that no
	// longer resolves, so it is inert.
	// Propagated rather than swallowed: MemberPublisher's delete policy (see
	// port.MemberPublisher) requires this, and /admin/reindex cannot repair a
	// dropped purge anyway — a genuinely deleted record reindexes as
	// outcomeNotFound, which clears any repair marker without re-emitting
	// delete_access. dispatchEntity continues to the next ID either way, so no
	// other record in the batch is affected; only an unrecorded purge travels
	// further, up to the replay-cursor decision in Run.
	if err := o.publisher.Access(ctx, constants.FGASyncDeleteAccessSubject, BuildB2BOrgDeleteAccessMessage(uid)); err != nil {
		return purgeFailure(constants.ReindexTypeB2BOrgDeleteAccess, uid, err,
			o.recordFailedDeleteAccess(ctx, constants.ReindexTypeB2BOrgDeleteAccess, uid))
	}
	return o.confirmDeleteAccessDelivery(ctx, constants.ReindexTypeB2BOrgDeleteAccess, uid)
}

// handleAccountAbsent handles an Account that was requested in a batch upsert
// but did not come back from SOQL. Absence is not deletion: an org that still
// exists but no longer holds a qualifying membership lands here too, which is
// why this entry point exists separately rather than sharing the delete one.
func (o *CDCConsumer) handleAccountAbsent(ctx context.Context, uid string) error {
	o.publishAccountDeleteIndex(ctx, uid)
	return nil
}

// publishAccountDeleteIndex converges the search state shared by both a genuine
// delete and a record that is merely absent from the qualifying SOQL query.
// Authorization cleanup deliberately remains in handleAccountDelete so the
// absent path cannot reach it through a boolean mode flag.
func (o *CDCConsumer) publishAccountDeleteIndex(ctx context.Context, uid string) {
	if err := o.cacheInvalidator.InvalidateB2BOrg(ctx, uid); err != nil {
		slog.WarnContext(ctx, "cdc: b2b_org cache invalidation failed on delete",
			"uid", uid, "error", err)
	}

	stubOrg := &model.B2BOrg{UID: uid}
	PublishB2BOrgIndexer(ctx, o.publisher, stubOrg, indexerConstants.ActionDeleted)
}

// ── Asset (project_membership) ────────────────────────────────────────────────

func (o *CDCConsumer) handleAsset(ctx context.Context, event model.CDCEvent) error {
	// See handleAccount: upserts run before the error is returned.
	deleteIDs, upsertIDs := partitionRecordIDs(ctx, "Asset", event)
	err := o.dispatchEntity(ctx, "Asset", event, deleteIDs, o.handleAssetDelete)
	if len(upsertIDs) > 0 {
		err = errors.Join(err, o.handleAssetUpsertBatch(ctx, upsertIDs, event.ChangeType))
	}
	return err
}

func (o *CDCConsumer) handleAssetUpsertBatch(ctx context.Context, upsertIDs []string, changeType model.CDCChangeType) error {
	if o.membershipBatch == nil {
		slog.WarnContext(ctx, "cdc: membershipBatch reader not wired — skipping Asset upsert; use /admin/reindex to repair",
			"record_count", len(upsertIDs), "publish_failed_for_backfill_repair", true)
		if isRestore(changeType) {
			return errors.Join(errRestoreIncomplete, errors.New("membership batch reader not wired"))
		}
		return nil
	}
	if o.quotaExceeded(ctx, "Asset", upsertIDs) {
		if isRestore(changeType) {
			return errors.Join(errRestoreIncomplete, errors.New("membership restore skipped by Salesforce quota guard"))
		}
		return nil
	}

	// Evict the sObject cache entry for each ID so subsequent re-fetch goes to
	// Salesforce rather than returning a stale cached record.
	for _, id := range upsertIDs {
		if err := o.cacheInvalidator.InvalidateProjectMembership(ctx, id); err != nil {
			slog.WarnContext(ctx, "cdc: project_membership cache invalidation failed",
				"uid", id, "error", err, "publish_failed_for_backfill_repair", true)
		}
	}

	memberships, convErrSFIDs, err := o.membershipBatch.FetchMembershipsBySFIDs(ctx, upsertIDs)
	if err != nil {
		logBatchFetchError(ctx, "Asset", upsertIDs, changeType, err)
		if isRestore(changeType) {
			return errors.Join(errRestoreIncomplete, err)
		}
		return nil
	}

	// IDs absent from the SOQL result are soft-deleted or no longer qualify
	// (e.g. Product2.Family flipped off Membership) — route to the absent path,
	// which converges the index only, since the latter case is a live record.
	// SFIDs present but unconvertible are also marked seen so they are not deleted.
	returned := makeReturnedSet(memberships, func(pm *model.ProjectMembership) string { return pm.UID }, convErrSFIDs)
	// handleAssetAbsent always returns nil, so this can never produce a
	// sentinel; captured anyway since the caller already has a return path.
	absentErr := o.handleAbsentAsDelete(ctx, "Asset", upsertIDs, returned, o.handleAssetAbsent)

	action := indexerConstants.ActionUpdated
	if changeType == model.CDCChangeCreate {
		action = indexerConstants.ActionCreated
	}

	contactsByMembership := make(map[string][]*model.KeyContact)
	if isRestore(changeType) && len(memberships) > 0 {
		if o.keyContactsByMembership == nil {
			slog.ErrorContext(ctx, "cdc: keyContactsByMembership reader not wired — restored contacts remain locked out",
				"membership_count", len(memberships), "publish_failed_for_backfill_repair", true)
			return errors.Join(errRestoreIncomplete,
				errors.New("keyContactsByMembership reader not wired"))
		}
		membershipUIDs := make([]string, 0, len(memberships))
		for _, pm := range memberships {
			membershipUIDs = append(membershipUIDs, pm.UID)
		}
		contactsByMembership, err = o.keyContactsByMembership.FetchKeyContactsByAssetSFIDs(ctx, membershipUIDs)
		if err != nil {
			slog.ErrorContext(ctx, "cdc: failed to batch-read current key_contacts for restored memberships — contacts remain locked out",
				"membership_count", len(membershipUIDs), "error", err,
				"publish_failed_for_backfill_repair", true)
			return errors.Join(errRestoreIncomplete,
				fmt.Errorf("batch-read key_contacts for restored memberships: %w", err))
		}
	}

	restoreErr := absentErr
	restorePublished := false
	for _, pm := range memberships {
		if isRestore(changeType) {
			published, err := o.publishRestoredProjectMembership(ctx, pm, action)
			restorePublished = restorePublished || published
			restoreErr = errors.Join(restoreErr, err)

			published, err = o.restoreKeyContactGrants(ctx, pm.UID, contactsByMembership[pm.UID])
			restorePublished = restorePublished || published
			restoreErr = errors.Join(restoreErr, err)
			continue
		}

		// Resolve ProjectUID from the slug (parity with backfill/HTTP paths) so
		// the indexer doc carries the project_uid tag + parent_ref. On a transient
		// resolver failure, skip only the indexer publish and still emit OpenFGA so
		// b2b_org / auditor tuples converge without overwriting project_uid in the
		// index — repaired by the next CDC event or /admin/reindex.
		uid, ok := resolveProjectUID(ctx, o.resolver, pm.ProjectSlug, pm.ProjectUID)
		if ok {
			pm.ProjectUID = uid
			PublishProjectMembershipIndexer(ctx, o.publisher, pm, action)
			PublishProjectMembershipFGA(ctx, o.publisher, pm)
		} else {
			slog.ErrorContext(ctx, "cdc: skipping project_membership indexer publish; project_uid unresolved — publishing OpenFGA only",
				"uid", pm.UID, "slug", pm.ProjectSlug, "publish_failed_for_backfill_repair", true)
			PublishProjectMembershipFGAPreservingMissingRefs(ctx, o.publisher, pm)
		}
	}
	if restorePublished {
		restoreErr = errors.Join(restoreErr, o.publisher.Flush(ctx))
	}

	slog.InfoContext(ctx, "cdc: asset batch published",
		"upsert_count", len(memberships),
		"absent_delete_count", len(upsertIDs)-len(returned))
	if restoreErr != nil {
		return errors.Join(errRestoreIncomplete, restoreErr)
	}
	return nil
}

func (o *CDCConsumer) publishRestoredProjectMembership(
	ctx context.Context,
	pm *model.ProjectMembership,
	action indexerConstants.MessageAction,
) (bool, error) {
	uid, resolveErr := resolveProjectUIDWithError(ctx, o.resolver, pm.ProjectSlug, pm.ProjectUID)
	resolved := resolveErr == nil
	if resolved {
		pm.ProjectUID = uid
		PublishProjectMembershipIndexer(ctx, o.publisher, pm, action)
	}

	var restoreErr error
	if resolveErr != nil && !pkgerrors.IsNotFound(resolveErr) {
		restoreErr = fmt.Errorf(
			"resolve project reference for restored project_membership %s: %w",
			pm.UID,
			resolveErr,
		)
	}
	missingB2BOrg := pm.B2BOrgUID == ""
	missingProject := pm.ProjectUID == ""
	if missingB2BOrg || missingProject {
		slog.ErrorContext(ctx, "cdc: restored project_membership has no authoritative structural reference",
			"uid", pm.UID,
			"missing_b2b_org", missingB2BOrg,
			"missing_project", missingProject,
			"retryable", restoreErr != nil,
			"publish_failed_for_backfill_repair", true,
		)
	}

	msg := BuildProjectMembershipFGAMessage(pm)
	if missingB2BOrg || missingProject {
		msg = BuildProjectMembershipFGAMessagePreserveMissingRefs(pm)
	}
	if err := o.publisher.Access(ctx, constants.FGASyncUpdateAccessSubject, msg); err != nil {
		return false, errors.Join(restoreErr,
			fmt.Errorf("publish structural restore for project_membership %s: %w", pm.UID, err))
	}
	return true, restoreErr
}

// restoreOrgAccess rebuilds every b2b_org tuple this service manages and that
// delete_access removed: configured teams, direct principals, parent, and child.
func (o *CDCConsumer) restoreOrgAccess(
	ctx context.Context,
	org *model.B2BOrg,
	children, parentChildren []string,
	hierarchyReady bool,
) (bool, error) {
	var writers, auditors []string
	if o.settingsReader != nil {
		settings, _, err := o.settingsReader.GetSettings(ctx, org.UID)
		if err != nil {
			return false, fmt.Errorf("read settings for restored org %s: %w", org.UID, err)
		}
		if settings != nil {
			writers = settings.ActiveWriterUsernames()
			auditors = settings.ActiveAuditorUsernames()
		}
	}

	messages := []fgatypes.GenericFGAMessage{BuildB2BOrgFGAMessage(org, B2BOrgFGARefs{
		GlobalOrgAdminTeamName: o.globalOrgAdminTeamName,
		AuditorTeams:           o.b2bOrgAuditorTeams,
		Writers:                writers,
		Auditors:               auditors,
	})}
	if hierarchyReady && org.ParentUID != "" {
		if parentChildren == nil {
			parentChildren = []string{}
		}
		messages = append(messages,
			BuildB2BOrgReparentingMessages(&model.B2BOrg{UID: org.UID}, org, nil, parentChildren)...)
	}
	if hierarchyReady {
		if children == nil {
			children = []string{}
		}
		messages = append(messages, BuildChildListMessage(org.UID, children))
	}

	published := false
	var restoreErr error
	for _, msg := range messages {
		if err := o.publisher.Access(ctx, constants.FGASyncUpdateAccessSubject, msg); err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		published = true
	}
	return published, restoreErr
}

// restoreKeyContactGrants re-publishes the current key_contact grants of a
// membership that Salesforce has just restored.
//
// A delete_access purge withdraws every tuple on the object, key_contact
// included, and the ordinary upsert republish cannot bring those back: both
// project_membership builders exclude the key_contact relation unconditionally
// (see buildProjectMembershipFGAMessage), because a membership upsert has no
// knowledge of the contacts granted on it. Without this, a restored membership
// comes back with its contacts locked out.
//
// Salesforce is authoritative here. The grant index exists to address later
// revokes and can be incomplete for grants created before that index existed.
// Each successful member_put refreshes the index through PublishKeyContactFGA.
// The caller batch-fetches contacts for every restored membership before
// publishing any restore message, so a source failure produces no partial
// restore and one CDC event uses one batched Salesforce read.
func (o *CDCConsumer) restoreKeyContactGrants(
	ctx context.Context,
	membershipUID string,
	contacts []*model.KeyContact,
) (bool, error) {
	restored := 0
	published := false
	var restoreErr error
	// contacts is already every key contact on this membership, so the
	// sibling-check lister costs no extra Salesforce/cache read here. Another
	// membership (a superseded pair) is uncovered and falls back to a live read.
	lister := sliceSiblingLister(contacts, o.keyContactsByMembership, o.userReader)
	live := siblingListerFor(o.keyContactsByMembership, o.userReader)
	for _, contact := range contacts {
		if contact.Username == "" && contact.Email != "" {
			if o.userReader == nil {
				restoreErr = errors.Join(restoreErr,
					fmt.Errorf("resolve LFID for restored key_contact %s: user reader not wired", contact.UID))
				continue
			}
			username, usernameErr := o.userReader.UsernameByEmail(ctx, contact.Email)
			if usernameErr != nil {
				if !pkgerrors.IsNotFound(usernameErr) {
					slog.WarnContext(ctx, "cdc: resolve LFID for restored key_contact failed",
						"uid", contact.UID, "error", usernameErr)
					restoreErr = errors.Join(restoreErr,
						fmt.Errorf("resolve LFID for restored key_contact %s: %w", contact.UID, usernameErr))
				}
			} else {
				contact.Username = username
			}
		}
		if contact.Username == "" && !strings.EqualFold(contact.Status, constants.RoleStatusInactive) {
			// No put can be published without an LFID. An Inactive contact
			// still goes through: its revoke addresses the recorded pair.
			continue
		}
		contactPublished, contactErr := publishKeyContactFGA(ctx, o.publisher, o.grantIndex, contact, lister, live)
		published = published || contactPublished
		if contactErr != nil {
			restoreErr = errors.Join(restoreErr, contactErr)
			continue
		}
		if contactPublished {
			restored++
		}
	}
	slog.InfoContext(ctx, "cdc: restored key_contact grants for undeleted membership",
		"membership_uid", membershipUID, "restored", restored, "current", len(contacts))
	return published, restoreErr
}

// handleAssetDelete handles an Asset genuinely deleted in Salesforce.
func (o *CDCConsumer) handleAssetDelete(ctx context.Context, uid string) error {
	o.publishAssetDeleteIndex(ctx, uid)

	// Propagate unrecorded delivery failures for the same reason as
	// handleAccountDelete: no later upsert can rebuild a deleted membership.
	if err := o.publisher.Access(ctx, constants.FGASyncDeleteAccessSubject, BuildProjectMembershipDeleteAccessMessage(uid)); err != nil {
		return purgeFailure(constants.ReindexTypeProjectMembershipDeleteAccess, uid, err,
			o.recordFailedDeleteAccess(ctx, constants.ReindexTypeProjectMembershipDeleteAccess, uid))
	}
	return o.confirmDeleteAccessDelivery(ctx, constants.ReindexTypeProjectMembershipDeleteAccess, uid)
}

// handleAssetAbsent handles an Asset that was requested in a batch upsert but
// did not come back from SOQL — soft-deleted, or no longer qualifying because
// Product2.Family flipped off Membership. The second case is a live record, so
// this path must not withdraw authorization.
func (o *CDCConsumer) handleAssetAbsent(ctx context.Context, uid string) error {
	o.publishAssetDeleteIndex(ctx, uid)
	return nil
}

// publishAssetDeleteIndex converges the search state shared by both a genuine
// delete and a record that no longer qualifies for the membership query.
// Authorization cleanup deliberately remains in handleAssetDelete.
func (o *CDCConsumer) publishAssetDeleteIndex(ctx context.Context, uid string) {
	if err := o.cacheInvalidator.InvalidateProjectMembership(ctx, uid); err != nil {
		slog.WarnContext(ctx, "cdc: project_membership cache invalidation failed on delete",
			"uid", uid, "error", err)
	}

	stubPM := &model.ProjectMembership{UID: uid}
	PublishProjectMembershipIndexer(ctx, o.publisher, stubPM, indexerConstants.ActionDeleted)
}

// ── Project_Role__c (key_contact) ─────────────────────────────────────────────

func (o *CDCConsumer) handleProjectRole(ctx context.Context, event model.CDCEvent) error {
	// See handleAccount: upserts run before the error is returned. This entity
	// publishes no delete_access; the returned error is non-nil only for an
	// unconfirmed key_contact revoke (errKeyContactRevokeIncomplete).
	//
	// The delete IDs are batched into one deleteHandler up front so a
	// multi-record delete event shares a single sibling fetch across every
	// referenced membership, instead of one Salesforce read per deleted
	// record (projectRoleDeleteBatcher).
	deleteIDs, upsertIDs := partitionRecordIDs(ctx, "Project_Role__c", event)
	deleteHandler := o.projectRoleDeleteBatcher(ctx, deleteIDs)
	dispatchErr := o.dispatchEntity(ctx, "Project_Role__c", event, deleteIDs, deleteHandler)
	var upsertBatchErr error
	if len(upsertIDs) > 0 {
		upsertBatchErr = o.handleProjectRoleUpsertBatch(ctx, upsertIDs, event.ChangeType)
	}
	return errors.Join(dispatchErr, upsertBatchErr)
}

// handleProjectRoleUpsertBatch returns a non-nil error when a key_contact
// revoke could not be confirmed, on either the absent-from-SOQL path (via
// handleAbsentAsDelete) or an Inactive contact's revoke/drain/transfer/flush
// in processKeyContact, so the replay cursor holds for redelivery.
func (o *CDCConsumer) handleProjectRoleUpsertBatch(ctx context.Context, upsertIDs []string, changeType model.CDCChangeType) error {
	if o.keyContactBatch == nil {
		slog.WarnContext(ctx, "cdc: keyContactBatch reader not wired — skipping Project_Role__c upsert; use /admin/reindex to repair",
			"record_count", len(upsertIDs), "publish_failed_for_backfill_repair", true)
		return nil
	}
	if o.quotaExceeded(ctx, "Project_Role__c", upsertIDs) {
		return nil
	}

	for _, id := range upsertIDs {
		if err := o.cacheInvalidator.InvalidateKeyContact(ctx, id); err != nil {
			slog.WarnContext(ctx, "cdc: key_contact cache invalidation failed",
				"uid", id, "error", err, "publish_failed_for_backfill_repair", true)
		}
	}

	contacts, convErrSFIDs, err := o.keyContactBatch.FetchKeyContactsBySFIDs(ctx, upsertIDs)
	if err != nil {
		logBatchFetchError(ctx, "Project_Role__c", upsertIDs, changeType, err)
		return nil
	}

	// Build a set of returned UIDs to detect absent records. SFIDs that were
	// IDs absent from the SOQL result are soft-deleted — route to delete.
	// SFIDs present but unconvertible are also marked seen so they are not deleted.
	returned := makeReturnedSet(contacts, func(kc *model.KeyContact) string { return kc.UID }, convErrSFIDs)
	absentIDs := make([]string, 0, len(upsertIDs)-len(returned))
	for _, id := range upsertIDs {
		if _, found := returned[id]; !found {
			absentIDs = append(absentIDs, id)
		}
	}
	// Same batching as the genuine-delete path: one sibling fetch shared
	// across every membership referenced by the absent IDs' grant entries.
	absentDeleteHandler := o.projectRoleDeleteBatcher(ctx, absentIDs)
	absentErr := o.handleAbsentAsDelete(ctx, "Project_Role__c", upsertIDs, returned, absentDeleteHandler)

	action := indexerConstants.ActionUpdated
	if changeType == model.CDCChangeCreate {
		action = indexerConstants.ActionCreated
	}

	// One prefetch serves every Inactive contact's sibling check on this page,
	// instead of a per-contact Salesforce query.
	lister := batchedSiblingLister(ctx, o.keyContactsByMembership, o.userReader, contacts)
	var contactErr error
	var retryIDs []string
	for _, kc := range contacts {
		// Keep processing the rest of the batch: one contact's revoke failure
		// must not block the others from publishing.
		if err := o.processKeyContact(ctx, kc, action, lister); err != nil {
			slog.ErrorContext(ctx, "cdc: handler failed",
				"entity", "Project_Role__c", "uid", kc.UID, "change_type", changeType, "error", err)
			contactErr = errors.Join(contactErr, err)
			if errors.Is(err, errKeyContactRevokeIncomplete) {
				retryIDs = append(retryIDs, kc.UID)
			}
		}
	}
	contactErr = errors.Join(contactErr, newRetryIDsError(retryIDs))

	slog.InfoContext(ctx, "cdc: project_role batch published",
		"upsert_count", len(contacts),
		"absent_delete_count", len(upsertIDs)-len(returned))
	return errors.Join(absentErr, contactErr)
}

// processKeyContact handles LFID resolution, publish, and silent org-dashboard
// provisioning for a single key contact within a CDC upsert batch.
//
// It returns a non-nil error, wrapped to match errKeyContactRevokeIncomplete,
// only when kc.Status is Inactive and its revoke, drain, durable-ownership
// transfer, or flush failed: the Salesforce record is still live and none of
// those failures self-heal on a later touch, unlike an active-path grant
// recording failure, which stays best-effort logged as before.
func (o *CDCConsumer) processKeyContact(ctx context.Context, kc *model.KeyContact, action indexerConstants.MessageAction, lister membershipKeyContactLister) error {
	// A live lister, not the batched one: an un-prefetched membership would
	// read as empty siblings and fake certainty. Also passed as the post-remove
	// recheck below, so a different-UID regrant racing the batched scan is caught.
	live := siblingListerFor(o.keyContactsByMembership, o.userReader)

	// Attempt LFID resolution when the contact has no stored username. CDC is a
	// passive sync and must never send emails — provisioning is always silent.
	var revokeErr error
	if o.userReader != nil && kc.Username == "" && kc.Email != "" {
		if username, usernameErr := o.userReader.UsernameByEmail(ctx, kc.Email); usernameErr != nil {
			if pkgerrors.IsNotFound(usernameErr) {
				// A definitive miss: the email no longer resolves to any registered
				// account (e.g. a rename or deregistration since the last time this
				// contact was granted). Revoke any grant still recorded for it.
				// Wrapped so a failure holds the cursor and the ID is retried.
				if err := revokeKeyContactGrantIfNoLongerLive(ctx, o.publisher, o.grantIndex, live, live, kc.UID, "", "", reasonEmailUnregistered); err != nil {
					revokeErr = fmt.Errorf("revoke key_contact grant for unregistered email %s: %w",
						kc.UID, errors.Join(err, errKeyContactRevokeIncomplete))
				}
			} else {
				// Transport-level failure — not evidence the email is unregistered;
				// leave Username empty and any existing grant untouched.
				slog.WarnContext(ctx, "cdc: resolve LFID for key contact failed",
					"uid", kc.UID, "error", usernameErr)
			}
		} else {
			kc.Username = username
		}
	}

	// Resolve ProjectUID from the slug (parity with backfill/HTTP paths) so the
	// indexer doc carries the project_uid tag + parent_ref. On a transient
	// resolver failure, skip only the indexer publish and still emit OpenFGA —
	// repaired by the next CDC event or /admin/reindex.
	uid, projectUIDResolved := resolveProjectUID(ctx, o.resolver, kc.ProjectSlug, kc.ProjectUID)
	if projectUIDResolved {
		kc.ProjectUID = uid
		PublishKeyContactIndexer(ctx, o.publisher, kc, action)
	} else {
		slog.ErrorContext(ctx, "cdc: skipping key_contact indexer publish; project_uid unresolved — publishing OpenFGA only",
			"uid", kc.UID, "slug", kc.ProjectSlug, "publish_failed_for_backfill_repair", true)
	}

	// publishKeyContactFGA only needs Username + MembershipUID, not ProjectUID.
	// The error-returning form is used, not the discarding PublishKeyContactFGA
	// wrapper, so an Inactive contact's revoke failure can hold the cursor.
	_, pubErr := publishKeyContactFGA(ctx, o.publisher, o.grantIndex, kc, lister, live)
	if pubErr != nil {
		if strings.EqualFold(kc.Status, constants.RoleStatusInactive) {
			pubErr = fmt.Errorf("publish key_contact FGA for inactive contact %s: %w",
				kc.UID, errors.Join(pubErr, errKeyContactRevokeIncomplete))
		} else {
			// Active-path failure (e.g. grant recording after a successful
			// put): repairable by the next CDC event or /admin/reindex, so it
			// stays best-effort logged rather than holding the cursor.
			slog.WarnContext(ctx, "cdc: key_contact FGA publish failed (best-effort)",
				"uid", kc.UID, "error", pubErr, "publish_failed_for_backfill_repair", true)
			pubErr = nil
		}
	}

	inactive := strings.EqualFold(kc.Status, constants.RoleStatusInactive)

	// Provision org-dashboard access silently for registered, non-Inactive
	// contacts when the indexer path ran (project_uid resolved). kc.Username is
	// non-empty only when UsernameByEmail resolved a trusted LFID, unregistered
	// contacts remain pending.
	if projectUIDResolved && kc.Username != "" && !inactive && o.orgSettings != nil && kc.B2BOrgUID != "" && kc.Email != "" {
		if _, provErr := o.orgSettings.AddPrincipal(ctx, B2BOrgSettingsAddPrincipal{
			OrgUID:               kc.B2BOrgUID,
			Email:                kc.Email,
			InvitedAs:            kcRoleToOrgRole(kc.Role),
			Name:                 kc.Name(),
			SuppressNotification: true,
		}); provErr != nil && !pkgerrors.IsConflict(provErr) {
			slog.WarnContext(ctx, "cdc: key contact org-dashboard provision failed (best-effort)",
				"uid", kc.UID, "error", provErr)
		}
	} else if inactive && o.orgSettings != nil && kc.B2BOrgUID != "" && kc.Email != "" {
		// Reconcile using remaining active siblings instead of re-asserting
		// access for a contact whose key_contact tuple was just revoked.
		// Best-effort: derived access, repairable, never holds the cursor.
		reconcileOrgDashboardAccess(ctx, o.orgSettings, o.storage, kc)
	}

	return errors.Join(pubErr, revokeErr)
}

// keyContactGrantLookup carries one grant-index read's result, captured once
// per deleted UID by projectRoleDeleteBatcher's pre-pass so the per-record
// delete handler below never re-reads the same entry.
type keyContactGrantLookup struct {
	grant port.KeyContactGrant
	found bool
	err   error
}

// projectRoleDeleteBatcher builds one deleteHandler for the whole batch of
// key_contact UIDs in ids: it reads each UID's grant-index entry once (a
// local KV read, not the Salesforce query this exists to batch), collects
// every membership referenced by those entries (a live pair's own membership
// and any PendingRevoke marker's) and fetches all of their siblings in a
// single Salesforce call, instead of the one-fetch-per-record
// cost siblingListerFor would otherwise pay for each deleted contact. A
// single-ID delete event runs the same path with a batch of one.
//
// The returned lister is for the sibling SCAN only; recheck stays a live,
// per-record lister built inside handleProjectRoleDelete, since recheck only
// runs after a remove actually publishes and that per-remove live read is
// the accepted budget.
func (o *CDCConsumer) projectRoleDeleteBatcher(ctx context.Context, ids []string) func(context.Context, string) error {
	lookups := make(map[string]keyContactGrantLookup, len(ids))
	memberships := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		grant, found, err := o.lookupKeyContactGrant(ctx, id)
		lookups[id] = keyContactGrantLookup{grant: grant, found: found, err: err}
		if err != nil || !found {
			continue
		}
		if grant.MembershipUID != "" {
			memberships[grant.MembershipUID] = struct{}{}
		}
		if grant.PendingRevoke != nil && grant.PendingRevoke.MembershipUID != "" {
			memberships[grant.PendingRevoke.MembershipUID] = struct{}{}
		}
	}
	lister := batchedSiblingListerForMemberships(ctx, o.keyContactsByMembership, o.userReader, memberships)
	return func(ctx context.Context, id string) error {
		return o.handleProjectRoleDelete(ctx, id, lookups[id], lister)
	}
}

func (o *CDCConsumer) handleProjectRoleDelete(ctx context.Context, uid string, lookup keyContactGrantLookup, lister membershipKeyContactLister) error {
	if err := o.cacheInvalidator.InvalidateKeyContact(ctx, uid); err != nil {
		slog.WarnContext(ctx, "cdc: key_contact cache invalidation failed on delete",
			"uid", uid, "error", err)
	}

	stubKC := &model.KeyContact{UID: uid}
	PublishKeyContactIndexer(ctx, o.publisher, stubKC, indexerConstants.ActionDeleted)

	// key_contact delete FGA revoke: best-effort, alertable on failure.
	// Uses GenericMemberRemoveSubject to match the key_contact_writer delete path.
	// The CDC event carries only the key contact's own SFID and the Salesforce
	// record is already gone, so the grant index is the only place the membership
	// object and granted username can be recovered from.
	grant, found, lookupErr := lookup.grant, lookup.found, lookup.err
	if lookupErr != nil {
		// Retries exhausted: the index may still hold the exact address
		// needed to revoke this grant, but the read itself failed. Hold the
		// replay cursor so redelivery retries the lookup.
		return fmt.Errorf("read key_contact grant index for %s: %w",
			uid, errors.Join(lookupErr, errKeyContactRevokeIncomplete))
	}
	if !found || ((grant.MembershipUID == "" || grant.Username == "") && grant.PendingRevoke == nil) {
		// Pre-index contact, or a grant that was never recorded. Retained for
		// parity with the behaviour that predates the index, though fga-sync
		// rejects a remove with an empty username without cleaning anything up:
		// this contact's grant, if any, is already dangling either way.
		removeMsg := BuildKeyContactFGARemoveMessage(uid, "")
		slog.WarnContext(ctx, "cdc: key_contact delete has no recorded grant — revoke cannot be addressed",
			"uid", uid, "fga_revoke_failed_dangling_tuple", true)
		if err := o.publisher.Access(ctx, fgaconstants.GenericMemberRemoveSubject, removeMsg); err != nil {
			// fga_revoke_failed_dangling_tuple=true signals a dangling FGA tuple:
			// the key_contact was deleted in Salesforce but the FGA relation was not
			// revoked. Unlike publish_failed_for_backfill_repair, this cannot be
			// recovered by /admin/reindex — requires a targeted FGA sync or
			// re-sending the remove message manually.
			slog.ErrorContext(ctx, "cdc: key_contact delete FGA revoke failed — dangling tuple requires manual cleanup",
				"uid", uid, "error", err, "fga_revoke_failed_dangling_tuple", true)
		}
		return nil
	}

	// recheck must be LIVE, never the batched scan lister: it only runs after
	// a remove actually publishes, so one live read per remove stays within
	// the accepted per-record budget even though the scan above is batched.
	// Quota-guarded so a multi-ID delete event cannot bypass the same
	// Salesforce budget the upsert path already respects.
	live := o.quotaGuardedLiveSiblingLister()

	// Marker-only entry: the live pair was already revoked and cleared, and
	// only the PendingRevoke marker for a superseded pair remains.
	if grant.MembershipUID == "" || grant.Username == "" {
		return o.revokeKeyContactMarkerOnDelete(ctx, uid, grant, lister, live)
	}

	// The record is already gone in Salesforce, so the stored pair's email
	// cannot be recovered: justification runs by resolution alone. The choke
	// point flushes before the entry is cleared, the same as the API delete.
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, o.publisher, lister, keyContactPairRevoke{
		membershipUID: grant.MembershipUID,
		username:      grant.Username,
		excludeUID:    uid,
		reason:        "key contact deleted in Salesforce",
		flush:         true,
		recheck:       live,
	})
	if outcome == revokeUncertain || outcome == revokeFailed {
		// Keep the index entry: it is the only record of what still needs
		// revoking, and the contact is gone so nothing will rebuild it. The
		// sentinel holds the replay cursor so redelivery retries this revoke.
		return fmt.Errorf("revoke key_contact grant for %s: %w",
			uid, errors.Join(revokeErr, errKeyContactRevokeIncomplete))
	}
	// A live sibling justified the pair per the BATCHED snapshot scan above,
	// but that snapshot can be stale (the sibling may have gone Inactive and
	// completed its own revoke since the scan ran): revalidate against the
	// LIVE lister before trusting it, and revoke instead if the snapshot no
	// longer holds.
	if outcome == revokeUnneeded && justifiedBy != nil {
		if err := o.revalidateJustifiedKeyContactPair(ctx, uid, grant.MembershipUID, grant.Username, live,
			"key contact deleted in Salesforce"); err != nil {
			// A failed transfer leaves the entry keyed by the just-deleted UID,
			// which nothing else will ever revisit: hold the replay cursor.
			return fmt.Errorf("revalidate key_contact justified pair for %s: %w",
				uid, errors.Join(err, errKeyContactRevokeIncomplete))
		}
	}

	// The live pair is settled. A PendingRevoke marker for a superseded pair
	// must be drained too before the entry can be deleted outright.
	if grant.PendingRevoke != nil {
		return o.drainKeyContactMarkerAndDelete(ctx, uid, grant, lister, live)
	}
	if err := o.grantIndex.Delete(ctx, uid, grant.Revision); err != nil {
		slog.WarnContext(ctx, "cdc: key_contact grant index cleanup failed after revoke",
			"uid", uid, "error", err)
	}
	return nil
}

// revalidateJustifiedKeyContactPair re-confirms, against the LIVE lister,
// that {membershipUID, username} is still justified before it is reasserted
// and its durable address transferred. The caller's own scan lister for this
// pair was the batched CDC snapshot, which can be stale: the sibling may have
// gone Inactive and completed its own revoke since the scan ran. When the
// live lister confirms a sibling, this reasserts and transfers ownership to
// it exactly as the caller would have. When it does not, the snapshot was
// stale: this runs the revoke path instead (live lister as both scan and
// recheck, flushed), retaining the original entry's pair as the outcome.
//
// A nil error means the pair was settled, either way, and the caller's own
// entry may be cleared. A non-nil error means it must be retained and the
// replay cursor held.
func (o *CDCConsumer) revalidateJustifiedKeyContactPair(ctx context.Context, uid, membershipUID, username string, live membershipKeyContactLister, reason string) error {
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, o.publisher, live, keyContactPairRevoke{
		membershipUID: membershipUID,
		username:      username,
		excludeUID:    uid,
		reason:        reason,
		flush:         true,
		recheck:       live,
	})
	switch outcome {
	case revokeUncertain, revokeFailed:
		return revokeErr
	case revokeUnneeded:
		if justifiedBy == nil {
			return nil
		}
		return o.settleJustifiedKeyContactPair(ctx, uid, membershipUID, username, justifiedBy)
	default:
		// revokePublished: the pair proved unjustified live and was revoked.
		return nil
	}
}

// settleJustifiedKeyContactPair reasserts a live-confirmed pair before its
// durable address transfers to sib, so a prior unconfirmed remove is
// repaired first. A non-nil error means the caller's entry must be retained.
func (o *CDCConsumer) settleJustifiedKeyContactPair(ctx context.Context, uid, membershipUID, username string, sib *model.KeyContact) error {
	if err := reassertKeyContactPendingRevokePair(ctx, o.publisher, uid, membershipUID, username); err != nil {
		return err
	}
	if !pairDurablyOwned(ctx, o.grantIndex, sib, membershipUID, username) {
		return fmt.Errorf("transfer durable revoke address for key_contact %s", uid)
	}
	return nil
}

// revokeKeyContactMarkerOnDelete handles a CDC delete for an index entry
// whose live pair was already revoked and cleared, leaving only a
// PendingRevoke marker for a superseded pair. It revokes that pending pair
// using the marker's own membership and username, then clears the entry.
// lister serves the sibling scan (the batch-wide prefetch); recheck must be a
// LIVE lister, mirroring handleProjectRoleDelete's own recheck.
func (o *CDCConsumer) revokeKeyContactMarkerOnDelete(ctx context.Context, uid string, grant port.KeyContactGrant, lister, recheck membershipKeyContactLister) error {
	marker := grant.PendingRevoke
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, o.publisher, lister, keyContactPairRevoke{
		membershipUID: marker.MembershipUID,
		username:      marker.Username,
		excludeUID:    uid,
		reason:        "key contact deleted in Salesforce (pending revoke marker)",
		flush:         true,
		recheck:       recheck,
	})
	if outcome == revokeUncertain || outcome == revokeFailed {
		return fmt.Errorf("revoke key_contact pending marker for %s: %w",
			uid, errors.Join(revokeErr, errKeyContactRevokeIncomplete))
	}
	// justifiedBy came from the batched snapshot scan above: revalidate live
	// before trusting it, same reasoning as handleProjectRoleDelete.
	if outcome == revokeUnneeded && justifiedBy != nil {
		if err := o.revalidateJustifiedKeyContactPair(ctx, uid, marker.MembershipUID, marker.Username, recheck,
			"key contact deleted in Salesforce (pending revoke marker)"); err != nil {
			return fmt.Errorf("revalidate key_contact pending marker for %s: %w",
				uid, errors.Join(err, errKeyContactRevokeIncomplete))
		}
	}
	if err := o.grantIndex.Delete(ctx, uid, grant.Revision); err != nil {
		slog.WarnContext(ctx, "cdc: key_contact grant index cleanup failed after marker revoke",
			"uid", uid, "error", err)
	}
	return nil
}

// drainKeyContactMarkerAndDelete drains a PendingRevoke marker after the
// entry's live pair was already revoked, then deletes the entry. If the
// drain fails or is uncertain, the entry is rewritten with the live pair
// cleared but the marker intact (mirroring clearRevokedGrant), preserving the
// marker as a retry address rather than deleting the entry outright.
// lister serves the sibling scan (the batch-wide prefetch); recheck must be a
// LIVE lister, mirroring handleProjectRoleDelete's own recheck.
func (o *CDCConsumer) drainKeyContactMarkerAndDelete(ctx context.Context, uid string, grant port.KeyContactGrant, lister, recheck membershipKeyContactLister) error {
	marker := grant.PendingRevoke
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, o.publisher, lister, keyContactPairRevoke{
		membershipUID: marker.MembershipUID,
		username:      marker.Username,
		excludeUID:    uid,
		reason:        "key contact deleted in Salesforce (draining superseded marker)",
		flush:         true,
		recheck:       recheck,
	})
	drainFailed := outcome == revokeUncertain || outcome == revokeFailed
	// justifiedBy came from the batched snapshot scan above: revalidate live
	// before trusting it, same reasoning as handleProjectRoleDelete.
	if !drainFailed && outcome == revokeUnneeded && justifiedBy != nil {
		if err := o.revalidateJustifiedKeyContactPair(ctx, uid, marker.MembershipUID, marker.Username, recheck,
			"key contact deleted in Salesforce (draining superseded marker)"); err != nil {
			revokeErr = err
			drainFailed = true
		}
	}
	if drainFailed {
		clearRevokedGrant(ctx, o.grantIndex, uid, grant)
		return fmt.Errorf("drain key_contact pending marker for %s: %w",
			uid, errors.Join(revokeErr, errKeyContactRevokeIncomplete))
	}
	if err := o.grantIndex.Delete(ctx, uid, grant.Revision); err != nil {
		slog.WarnContext(ctx, "cdc: key_contact grant index cleanup failed after revoke",
			"uid", uid, "error", err)
	}
	return nil
}

// maxGrantIndexReadAttempts bounds the retry when a grant-index read fails
// during a CDC delete. A read failure here is not the same as a Get miss: the
// index may still hold the exact address needed to revoke this grant, but the
// read itself — not the answer — failed. This handler's caller commits the
// replay cursor regardless of outcome, and unlike a live contact (repairable
// by the next CDC event or /admin/reindex), a deleted one has no other chance
// to retry — so a transient blip that a bounded, immediate retry would have
// ridden out must not be treated the same as "no grant was ever recorded."
const maxGrantIndexReadAttempts = 3

// lookupKeyContactGrant returns the grant recorded for uid. found reports
// whether an entry exists at all, even a marker-only one with an empty live
// pair; the grant is returned raw, not blanked. err is non-nil only once
// maxGrantIndexReadAttempts is exhausted, distinguishing a read failure
// (the index may still hold the address needed to revoke this grant) from a
// genuine miss, so the caller can hold the replay cursor instead of silently
// falling back to an unaddressed revoke.
func (o *CDCConsumer) lookupKeyContactGrant(ctx context.Context, uid string) (port.KeyContactGrant, bool, error) {
	if o.grantIndex == nil {
		return port.KeyContactGrant{}, false, nil
	}
	var lastErr error
	for attempt := 1; attempt <= maxGrantIndexReadAttempts; attempt++ {
		grant, found, err := o.grantIndex.Get(ctx, uid)
		if err == nil {
			return grant, found, nil
		}
		lastErr = err
	}
	slog.ErrorContext(ctx, "cdc: key_contact grant index read failed on delete after retries",
		"uid", uid, "error", lastErr, "attempts", maxGrantIndexReadAttempts, "fga_revoke_failed_dangling_tuple", true)
	return port.KeyContactGrant{}, false, lastErr
}
