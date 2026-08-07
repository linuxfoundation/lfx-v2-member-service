// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
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
	subscriber             port.CDCSubscriber
	resolver               port.ProjectResolver
	b2bOrgReader           port.B2BOrgReader
	membershipBatch        port.MembershipBatchReader
	keyContactBatch        port.KeyContactBatchReader
	accountBatch           port.AccountBatchReader
	cacheInvalidator       port.CacheInvalidator
	publisher              port.MemberPublisher
	quotaGauge             port.SalesforceQuotaGauge
	quotaSkipThreshold     float64
	quotaRefreshStaleAfter time.Duration
	repairStore            port.CDCRepairStore
	grantIndex             port.KeyContactGrantIndex
	globalOrgAdminTeamUID  string
	b2bOrgAuditorTeams     []string
	userReader             port.UserReader
	orgSettings            OrgSettingsPrincipalWriter

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
func WithCDCKeyContactGrantIndex(i port.KeyContactGrantIndex) CDCConsumerOption {
	return func(o *CDCConsumer) { o.grantIndex = i }
}

func WithCDCGlobalOrgAdminTeamUID(uid string) CDCConsumerOption {
	return func(o *CDCConsumer) { o.globalOrgAdminTeamUID = uid }
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
		// Wrap the handler in a closure so that defers guarantee span.End()
		// and handleCancel() run even if o.handle panics. Both defers run
		// before the closure returns, so neither is included in the
		// replay-cursor save that follows. span.End() fires first because it
		// is deferred after handleCancel() (LIFO order).
		handleErr := func(event model.CDCEvent) error {
			// Give each handler a short-lived background context so that an
			// in-flight Salesforce fetch or NATS cache write is not aborted by a
			// concurrent graceful shutdown. 30 s matches the graceful-shutdown
			// window; any handler that runs longer than that is already a problem.
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
		}(event)
		if handleErr != nil {
			// Log and continue — /admin/reindex is the backstop for missed events.
			slog.ErrorContext(ctx, "cdc: event handling failed, continuing",
				"entity", event.Entity,
				"change_type", event.ChangeType,
				"record_ids", event.RecordIDs,
				"error", handleErr,
			)
		}

		// Commit-after-process: persist cursor regardless of handler error so
		// a transient failure doesn't block the stream indefinitely.
		//
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

// quotaExceeded reports whether the Salesforce REST API quota has been consumed
// beyond the configured threshold, refreshing a stale reading first so a skip
// loop cannot freeze the gauge (the "quota death spiral"). When it decides to
// skip, it also records each affected record to the durable repair queue.
//
// When quotaGauge is nil, the threshold is ≥ 1, or no valid reading has ever
// been observed (even after an attempted refresh), this returns false
// (fail-open).
func (o *CDCConsumer) quotaExceeded(ctx context.Context, entity string, ids []string) bool {
	if o.quotaGauge == nil {
		return false
	}
	if o.quotaSkipThreshold >= 1 {
		// Threshold of 1 means "never skip" — guard disabled regardless of usage.
		return false
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
		return false
	}

	if snap.Ratio() < o.quotaSkipThreshold {
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

// dispatchEntity normalizes and partitions event record IDs, runs each delete
// ID through deleteHandler (logging failures), and returns the upsert IDs for
// the caller to batch-process. Shared by all three entity top-level handlers.
func (o *CDCConsumer) dispatchEntity(ctx context.Context, entity string, event model.CDCEvent,
	deleteHandler func(context.Context, string) error) []string {
	deleteIDs, upsertIDs := partitionRecordIDs(ctx, entity, event)
	for _, id := range deleteIDs {
		if err := deleteHandler(ctx, id); err != nil {
			slog.ErrorContext(ctx, "cdc: handler failed",
				"entity", entity, "uid", id, "change_type", event.ChangeType, "error", err)
		}
	}
	return upsertIDs
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
	if upsertIDs := o.dispatchEntity(ctx, "Account", event, o.handleAccountDelete); len(upsertIDs) > 0 {
		o.handleAccountUpsertBatch(ctx, upsertIDs, event.ChangeType)
	}
	return nil
}

func (o *CDCConsumer) handleAccountUpsertBatch(ctx context.Context, upsertIDs []string, changeType model.CDCChangeType) {
	if o.accountBatch == nil {
		slog.WarnContext(ctx, "cdc: accountBatch reader not wired — skipping Account upsert; use /admin/reindex to repair",
			"record_count", len(upsertIDs), "publish_failed_for_backfill_repair", true)
		return
	}
	if o.quotaExceeded(ctx, "Account", upsertIDs) {
		return
	}

	// Capture old record state BEFORE cache eviction for reparenting diff.
	// If the cache is cold, GetB2BOrg returns the post-change record — in that
	// case oldOrg == new org and no reparenting messages are emitted (safe).
	oldOrgs := make(map[string]*model.B2BOrg, len(upsertIDs))
	for _, id := range upsertIDs {
		if current, err := o.b2bOrgReader.GetB2BOrg(ctx, id); err == nil {
			oldOrgs[id] = current
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
		return
	}

	// SFIDs absent from the SOQL result are soft-deleted or no longer hold a
	// membership Asset — route them to the delete path for index/FGA convergence.
	// SFIDs present but unconvertible are also marked seen so they are not deleted.
	returned := makeReturnedSet(orgs, func(o *model.B2BOrg) string { return o.UID }, convErrSFIDs)
	o.handleAbsentAsDelete(ctx, "Account", upsertIDs, returned, o.handleAccountDelete)

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

	// CDC always passes globalOrgAdminTeamUID (not create-only like the writer).
	for _, org := range orgs {
		publishB2BOrgUpsertEvents(ctx, o.b2bOrgReader, o.publisher, oldOrgs[org.UID], org, indexerConstants.ActionUpdated, o.globalOrgAdminTeamUID, o.b2bOrgAuditorTeams)
		// Emit the parent hierarchy tuple unconditionally for parented orgs so a
		// CDC-created child org gets its parent + child-list tuples even when no
		// reparent was detected. publishB2BOrgUpsertEvents only emits reparenting
		// messages on a parent *change* (nil on a cold-cache create); this closes
		// that gap. Both are idempotent update_access upserts, so a genuine
		// reparent double-emitting the new parent tuple is safe.
		if org.ParentUID != "" {
			PublishB2BOrgParentFGA(ctx, o.publisher, org, childMap[org.ParentUID])
		}
	}

	slog.InfoContext(ctx, "cdc: account batch published",
		"upsert_count", len(orgs),
		"absent_delete_count", len(upsertIDs)-len(returned))
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
// provided delete handler for index/FGA convergence.
func (o *CDCConsumer) handleAbsentAsDelete(ctx context.Context, entity string, upsertIDs []string, returned map[string]struct{}, deleteHandler func(context.Context, string) error) {
	for _, id := range upsertIDs {
		if _, found := returned[id]; !found {
			slog.DebugContext(ctx, "cdc: absent from SOQL result, routing to delete for convergence",
				"entity", entity, "uid", id)
			if delErr := deleteHandler(ctx, id); delErr != nil {
				slog.ErrorContext(ctx, "cdc: handler failed",
					"entity", entity, "uid", id, "change_type", "absent→delete", "error", delErr)
			}
		}
	}
}

func (o *CDCConsumer) handleAccountDelete(ctx context.Context, uid string) error {
	if err := o.cacheInvalidator.InvalidateB2BOrg(ctx, uid); err != nil {
		slog.WarnContext(ctx, "cdc: b2b_org cache invalidation failed on delete",
			"uid", uid, "error", err)
	}

	stubOrg := &model.B2BOrg{UID: uid}
	PublishB2BOrgIndexer(ctx, o.publisher, stubOrg, indexerConstants.ActionDeleted)

	// nil access (writers/auditors) = preserve; empty = clear. Passing nil here
	// reconciles nothing away: every relation lands in ExcludeRelations, so the
	// org's per-user grants and hierarchy edges outlive the delete. Nothing
	// reaps them today — fga-sync subscribes to no indexer subject, so the
	// delete indexer event above does not drive FGA cleanup. See LFXV2-3034.
	//
	// No team references are asserted here — neither the global-admin UID nor
	// the auditor teams. fga-sync never deletes a tuple whose subject begins
	// with "team:", so any team reference written for an org that no longer
	// exists is a permanent orphan on a dead object that nothing can reap.
	fgaMsg := BuildB2BOrgFGAMessage(stubOrg, B2BOrgFGARefs{})
	if err := o.publisher.Access(ctx, constants.FGASyncUpdateAccessSubject, fgaMsg); err != nil {
		slog.WarnContext(ctx, "cdc: b2b_org delete FGA publish failed",
			"uid", uid, "error", err, "publish_failed_for_backfill_repair", true)
	}
	return nil
}

// ── Asset (project_membership) ────────────────────────────────────────────────

func (o *CDCConsumer) handleAsset(ctx context.Context, event model.CDCEvent) error {
	if upsertIDs := o.dispatchEntity(ctx, "Asset", event, o.handleAssetDelete); len(upsertIDs) > 0 {
		o.handleAssetUpsertBatch(ctx, upsertIDs, event.ChangeType)
	}
	return nil
}

func (o *CDCConsumer) handleAssetUpsertBatch(ctx context.Context, upsertIDs []string, changeType model.CDCChangeType) {
	if o.membershipBatch == nil {
		slog.WarnContext(ctx, "cdc: membershipBatch reader not wired — skipping Asset upsert; use /admin/reindex to repair",
			"record_count", len(upsertIDs), "publish_failed_for_backfill_repair", true)
		return
	}
	if o.quotaExceeded(ctx, "Asset", upsertIDs) {
		return
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
		return
	}

	// IDs absent from the SOQL result are soft-deleted or no longer qualify
	// (e.g. Product2.Family flipped off Membership) — route to delete.
	// SFIDs present but unconvertible are also marked seen so they are not deleted.
	returned := makeReturnedSet(memberships, func(pm *model.ProjectMembership) string { return pm.UID }, convErrSFIDs)
	o.handleAbsentAsDelete(ctx, "Asset", upsertIDs, returned, o.handleAssetDelete)

	action := indexerConstants.ActionUpdated
	if changeType == model.CDCChangeCreate {
		action = indexerConstants.ActionCreated
	}

	for _, pm := range memberships {
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

	slog.InfoContext(ctx, "cdc: asset batch published",
		"upsert_count", len(memberships),
		"absent_delete_count", len(upsertIDs)-len(returned))
}

func (o *CDCConsumer) handleAssetDelete(ctx context.Context, uid string) error {
	if err := o.cacheInvalidator.InvalidateProjectMembership(ctx, uid); err != nil {
		slog.WarnContext(ctx, "cdc: project_membership cache invalidation failed on delete",
			"uid", uid, "error", err)
	}

	stubPM := &model.ProjectMembership{UID: uid}
	PublishProjectMembershipIndexer(ctx, o.publisher, stubPM, indexerConstants.ActionDeleted)
	return nil
}

// ── Project_Role__c (key_contact) ─────────────────────────────────────────────

func (o *CDCConsumer) handleProjectRole(ctx context.Context, event model.CDCEvent) error {
	if upsertIDs := o.dispatchEntity(ctx, "Project_Role__c", event, o.handleProjectRoleDelete); len(upsertIDs) > 0 {
		o.handleProjectRoleUpsertBatch(ctx, upsertIDs, event.ChangeType)
	}
	return nil
}

func (o *CDCConsumer) handleProjectRoleUpsertBatch(ctx context.Context, upsertIDs []string, changeType model.CDCChangeType) {
	if o.keyContactBatch == nil {
		slog.WarnContext(ctx, "cdc: keyContactBatch reader not wired — skipping Project_Role__c upsert; use /admin/reindex to repair",
			"record_count", len(upsertIDs), "publish_failed_for_backfill_repair", true)
		return
	}
	if o.quotaExceeded(ctx, "Project_Role__c", upsertIDs) {
		return
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
		return
	}

	// Build a set of returned UIDs to detect absent records. SFIDs that were
	// IDs absent from the SOQL result are soft-deleted — route to delete.
	// SFIDs present but unconvertible are also marked seen so they are not deleted.
	returned := makeReturnedSet(contacts, func(kc *model.KeyContact) string { return kc.UID }, convErrSFIDs)
	o.handleAbsentAsDelete(ctx, "Project_Role__c", upsertIDs, returned, o.handleProjectRoleDelete)

	action := indexerConstants.ActionUpdated
	if changeType == model.CDCChangeCreate {
		action = indexerConstants.ActionCreated
	}

	for _, kc := range contacts {
		o.processKeyContact(ctx, kc, action)
	}

	slog.InfoContext(ctx, "cdc: project_role batch published",
		"upsert_count", len(contacts),
		"absent_delete_count", len(upsertIDs)-len(returned))
}

// processKeyContact handles LFID resolution, publish, and silent org-dashboard
// provisioning for a single key contact within a CDC upsert batch.
func (o *CDCConsumer) processKeyContact(ctx context.Context, kc *model.KeyContact, action indexerConstants.MessageAction) {
	// Attempt LFID resolution when the contact has no stored username. CDC is a
	// passive sync and must never send emails — provisioning is always silent.
	if o.userReader != nil && kc.Username == "" && kc.Email != "" {
		if username, usernameErr := o.userReader.UsernameByEmail(ctx, kc.Email); usernameErr != nil {
			if !pkgerrors.IsNotFound(usernameErr) {
				slog.WarnContext(ctx, "cdc: resolve LFID for key contact failed",
					"uid", kc.UID, "error", usernameErr)
			}
			// NotFound is expected for unregistered emails — leave Username empty.
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

	// PublishKeyContactFGA only needs Username + MembershipUID, not ProjectUID.
	PublishKeyContactFGA(ctx, o.publisher, o.grantIndex, kc)

	// Provision org-dashboard access silently for registered contacts when the
	// indexer path ran (project_uid resolved). kc.Username is non-empty only when
	// UsernameByEmail resolved a trusted LFID — unregistered contacts remain pending.
	if projectUIDResolved && kc.Username != "" && o.orgSettings != nil && kc.B2BOrgUID != "" && kc.Email != "" {
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
	}
}

func (o *CDCConsumer) handleProjectRoleDelete(ctx context.Context, uid string) error {
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
	grant, indexed := o.lookupKeyContactGrant(ctx, uid)
	removeMsg := BuildKeyContactFGARemoveMessage(grant.MembershipUID, grant.Username)
	if !indexed {
		// Pre-index contact, or a grant that was never recorded. Retained for
		// parity with the behaviour that predates the index, though fga-sync
		// rejects a remove with an empty username without cleaning anything up:
		// this contact's grant, if any, is already dangling either way.
		removeMsg = BuildKeyContactFGARemoveMessage(uid, "")
		slog.WarnContext(ctx, "cdc: key_contact delete has no recorded grant — revoke cannot be addressed",
			"uid", uid, "fga_revoke_failed_dangling_tuple", true)
	}

	if err := o.publisher.Access(ctx, fgaconstants.GenericMemberRemoveSubject, removeMsg); err != nil {
		// fga_revoke_failed_dangling_tuple=true signals a dangling FGA tuple:
		// the key_contact was deleted in Salesforce but the FGA relation was not
		// revoked. Unlike publish_failed_for_backfill_repair, this cannot be
		// recovered by /admin/reindex — requires a targeted FGA sync or
		// re-sending the remove message manually.
		slog.ErrorContext(ctx, "cdc: key_contact delete FGA revoke failed — dangling tuple requires manual cleanup",
			"uid", uid, "error", err, "fga_revoke_failed_dangling_tuple", true)
		// Keep the index entry: it is the only record of what still needs
		// revoking, and the contact is gone so nothing will rebuild it.
		return nil
	}

	// Access only hands the revoke to the local NATS connection; it does not
	// confirm the broker received it. Flush before clearing the index entry
	// below, the same as the API delete path: without it, a crash or
	// disconnect in the window between Access and actual broker delivery
	// loses the member_remove while this call has already erased the only
	// {membership_uid, username} record needed to retry it — and unlike a
	// live contact, a deleted one gets no other chance to.
	if indexed {
		if flushErr := o.publisher.Flush(ctx); flushErr != nil {
			slog.ErrorContext(ctx, "cdc: key_contact delete FGA revoke flush failed — delivery indeterminate, keeping index entry",
				"uid", uid, "error", flushErr, "fga_revoke_failed_dangling_tuple", true)
			return nil
		}
		if err := o.grantIndex.Delete(ctx, uid, grant.Revision); err != nil {
			slog.WarnContext(ctx, "cdc: key_contact grant index cleanup failed after revoke",
				"uid", uid, "error", err)
		}
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

// lookupKeyContactGrant returns the grant recorded for uid. It reports false
// when the index is not wired or holds no entry — both mean, correctly, that
// the caller has no addressable grant to revoke. A read failure is retried
// (see maxGrantIndexReadAttempts); only once retries are exhausted does it
// also report false, logged distinctly from a genuine miss so it is
// alertable as a fresh dangling tuple rather than the accepted pre-index gap.
func (o *CDCConsumer) lookupKeyContactGrant(ctx context.Context, uid string) (port.KeyContactGrant, bool) {
	if o.grantIndex == nil {
		return port.KeyContactGrant{}, false
	}
	var lastErr error
	for attempt := 1; attempt <= maxGrantIndexReadAttempts; attempt++ {
		grant, found, err := o.grantIndex.Get(ctx, uid)
		if err == nil {
			if !found || grant.MembershipUID == "" || grant.Username == "" {
				return port.KeyContactGrant{}, false
			}
			return grant, true
		}
		lastErr = err
	}
	slog.ErrorContext(ctx, "cdc: key_contact grant index read failed on delete after retries — falling back to unaddressed revoke",
		"uid", uid, "error", lastErr, "attempts", maxGrantIndexReadAttempts, "fga_revoke_failed_dangling_tuple", true)
	return port.KeyContactGrant{}, false
}
