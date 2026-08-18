# Backfill / Reindex — Member Service

This document is the authoritative reference for the **backfill runner** — the code that re-reads entities from Salesforce (and, for settings, the `org-settings` KV) and re-publishes them to the indexer and fga-sync services on demand, as a recovery/repair tool.

One `service.Runner` (`internal/service/backfill_runner.go`) backs three entry points:

- **`POST /admin/reindex`** — an async HTTP admin action that reindexes a full, since-filtered, or targeted set of entities for one type.
- **`POST /admin/reindex {cdc_repair:true}`** — drains the durable CDC quota-repair queue for one type (records the CDC consumer skipped while the Salesforce API quota was exhausted). See [CDC Quota-Repair Drain](#cdc-quota-repair-drain).
- **`RUN_MODE=avatar-backfill`** — a one-off Kubernetes Job that re-enriches `b2b_org_settings` avatars and republishes them.

The first two hand a `BackfillRequest` to `Runner.Run` (full/filtered/targeted) or the dedicated `Runner.PrepareRepair` + `Runner.RunRepair` pair (`cdc_repair`); the avatar Job also uses `Runner.Run`. All three share the run-id and dry-run control plane; only full mode additionally takes the per-type full-run lock (`cdc_repair` takes no lock — see [CDC Quota-Repair Drain](#cdc-quota-repair-drain)).

**Update this document in the same PR as any change to the backfill runner, the `/admin/reindex` payload/validation, or the avatar-backfill Job.**

For the downstream message payloads the runner produces, see:

- [FGA Contract](./fga-contract.md) — the fga-sync messages (`update_access`, `member_put`).
- [Indexer Contract](./indexer-contract.md) — the OpenSearch indexer messages (`created`/`updated`).
- [CDC Consumer](./cdc-consumer.md) — the near-real-time sync path; `/admin/reindex` is its backstop, and the quota guard's stale-refresh + write-on-skip behaviour is documented there. `cdc_repair` drains the queue it writes.

---

## Contents

- [Entry Points](#entry-points)
- [Request Model](#request-model)
- [Run Modes](#run-modes)
- [HTTP Endpoint](#http-endpoint)
- [Full-Run Lock](#full-run-lock)
- [Quota Guard & Windowed Reindex](#quota-guard--windowed-reindex)
- [CDC Quota-Repair Drain](#cdc-quota-repair-drain)
- [Per-Type Handling](#per-type-handling)
- [`project_uid` Resolution Parity](#project_uid-resolution-parity)
- [Avatar Enrichment (`avatar-backfill`)](#avatar-enrichment-avatar-backfill)
- [Dry Run](#dry-run)
- [Failure Modes & Log Signals](#failure-modes--log-signals)
- [Configuration](#configuration)
- [NATS Storage](#nats-storage)
- [Observability](#observability)

---

## Entry Points

| Trigger | Builds request via | Calls |
|---|---|---|
| `POST /admin/reindex` | `ValidateAndBuildRequest` (`internal/service/backfill_request.go`) | `Runner.Run` in a fire-and-forget goroutine |
| `RUN_MODE=avatar-backfill` | `AvatarBackfillRequest` (`internal/service/backfill_request.go`) | `Runner.Run`, returning the error as the Job exit code |

The runner is constructed by `BackfillRunnerImpl` (`cmd/member-api/service/providers.go`), which wires the SOQL iterators/readers, the publisher, the NATS client, the project resolver, and the avatar-enrichment collaborators (`WithSettingsWriter`, `WithUserReader`).

---

## Request Model

`BackfillRequest` (`internal/service/backfill_request.go`) is the validated, normalised input for a single run. **Every request has exactly one `Type` — there is no all-types shortcut** (this is a breaking change from the earlier `Types []string` array; see [Per-Type Handling](#per-type-handling) for why the migration was worth it).

| Field | Meaning |
|---|---|
| `RunID` | Correlation ID stamped on every log line for the run. Set to a fresh UUID by the HTTP handler and the avatar Job. |
| `Type` | Required: the single entity type for this run. |
| `Since` | `nil` = full reindex; otherwise only records with `LastModifiedDate >= Since`. Mutually exclusive with `Items`/`CDCRepair`. |
| `Until` | Optional **inclusive** upper bound (`LastModifiedDate <= Until`). **Requires `Since`**; together they define a bounded `[Since, Until]` window sized to fit available quota. Mutually exclusive with `Items`/`CDCRepair`; rejected if `Since > Until`. See [Quota Guard & Windowed Reindex](#quota-guard--windowed-reindex). |
| `Items` | Targeted UIDs, all of `Type`, for surgical reindex (up to 100). Mutually exclusive with `Since`/`Until`/`CDCRepair`. |
| `CDCRepair` | Drain the CDC quota-repair queue for `Type` instead of reading from Salesforce paged/targeted. Mutually exclusive with `Since`/`Until`/`Items`. Only `b2b_org`, `project_membership`, `key_contact` are supported (no CDC skip path exists for `b2b_org_settings`). See [CDC Quota-Repair Drain](#cdc-quota-repair-drain). |
| `DryRun` | Walk the read path but skip publishing. |
| `EnrichAvatars` / `AvatarMissingOnly` / `AvatarSleep` | Avatar-backfill Job only; **not** exposed on the HTTP payload. |

In-scope types (`allBackfillTypes`): `b2b_org`, `project_membership`, `key_contact`, `b2b_org_settings`.

---

## Run Modes

`ClassifyMode` selects the mode from the request:

| Mode | Condition | Iteration source |
|---|---|---|
| `repair` | `CDCRepair == true` | pending markers from the `cdc-repair` NATS KV bucket for `Type` (`PrepareRepair` + `RunRepair`) — see [CDC Quota-Repair Drain](#cdc-quota-repair-drain) |
| `targeted` | `len(Items) > 0` | the explicit `Items` list, all of `Type` (`runTargeted`) |
| `filtered` | `Since != nil` | paged SOQL `Iter*` for `Type` with the `since` (and optional `until`) window (`runType`) |
| `full` | otherwise | paged SOQL `Iter*` for `Type` over all records (`runType`) |

> **`until` never changes the mode.** `ClassifyMode` keys off `Since` only, and `until` requires `since`, so a windowed `[since, until]` request is always `filtered` — it never wrongly takes the full-run lock.

`filtered` and `full` iterate exactly `req.Type` — every run reindexes one type; call the endpoint once per type to reindex several. Only **full** mode acquires the per-type NATS lock and logs `full_reindex_started=true`. `repair` mode is dispatched by the HTTP handler directly to `PrepareRepair`/`RunRepair`, not through `Runner.Run` (`Run` returns an error if called with `CDCRepair` set — a defensive guard against misuse).

---

## HTTP Endpoint

`POST /admin/reindex` — handler `AdminReindex` (`cmd/member-api/service/membership_service.go`).

**Authorization:** `member` on `team:{globalOrgAdminTeamUID}` (Heimdall ruleset).

**Payload** (`AdminReindexPayload`, `cmd/member-api/design/type.go`):

| Field | Notes |
|---|---|
| `type` | **Required**; exactly one of `b2b_org`, `project_membership`, `key_contact`, `b2b_org_settings` — no all-types shortcut. |
| `since` | RFC 3339 with explicit zone; normalised to UTC. Mutually exclusive with `items`/`cdc_repair`. |
| `until` | RFC 3339 with explicit zone; normalised to UTC. Inclusive upper bound. **Requires `since`**; rejected if `since > until`. Mutually exclusive with `items`/`cdc_repair`. See [Quota Guard & Windowed Reindex](#quota-guard--windowed-reindex). |
| `items` | Targeted UIDs of `type`, as `[{"uid": "..."}, ...]`; **max 100** (`MaxLength(100)`). Mutually exclusive with `since`/`until`/`cdc_repair`. |
| `cdc_repair` | Drain the CDC quota-repair queue for `type` instead of a Salesforce read. Mutually exclusive with `since`/`until`/`items`; only `b2b_org`, `project_membership`, `key_contact` support it. See [CDC Quota-Repair Drain](#cdc-quota-repair-drain). |
| `dry_run` | Default `false`. Rejected (`400`) together with `cdc_repair` — there is no dry-run drain in this release, and the underlying reindex path would otherwise delete real pending markers without republishing them. |

**Validation** (`ValidateAndBuildRequest`):

- `type` must be one of the four in-scope types; unknown types are rejected, and `membership_tier` is rejected with a specific message (not currently supported).
- `items` is mutually exclusive with `since`, `until`, and `cdc_repair`.
- Each item's `uid` must be a Salesforce ID (`sfuuid.IsSFID`); the type is the top-level `type` for every item (there is no per-item type — mixed-type item batches are structurally impossible).
- `cdc_repair` is mutually exclusive with `since`/`until`/`items`, and only `b2b_org`, `project_membership`, `key_contact` are accepted for it (`b2b_org_settings` is rejected — no CDC skip path exists for it).
- `since` and `until` must parse as RFC 3339; both are converted to UTC. `until` **requires `since`** (rejected otherwise), and `since > until` is rejected (an inverted window returns `400` rather than silently matching zero records).

**Response:** HTTP `202 Accepted`.

- **Full/filtered/targeted:** `{ "run_id": "<uuid>" }`. The handler generates the `run_id`, logs the accepted request, then runs the backfill in a goroutine on `context.WithoutCancel(ctx)` so it outlives the HTTP request. The goroutine's return value is **ignored** — fire-and-forget; track progress by grepping logs for `run_id=<uuid>`. A pod restart during a large run interrupts it mid-flight (partial index, no error logged); re-trigger to repair. If the runner is not initialised, the handler logs a warning and still returns the `run_id`.
  - **Quota gate (full/filtered only):** before launching the goroutine, the handler synchronously gates full/filtered runs (`GateBackfillStart`) and returns HTTP `503` when the Salesforce quota is at/above `ADMIN_REINDEX_QUOTA_THRESHOLD` — symmetric with `cdc_repair`. **Targeted (`items`) is exempt** (bounded surgical tool). See [Quota Guard & Windowed Reindex](#quota-guard--windowed-reindex).
- **`cdc_repair`:** `{ "run_id": "<uuid>", "selected_count": <n> }`. `PrepareRepair` runs **synchronously** in the request (quota gate + page selection), so `selected_count` reflects the exact number of markers handed to the async drain. A quota-unreadable or at/above-threshold gate returns HTTP `503` instead of `202` — see [CDC Quota-Repair Drain](#cdc-quota-repair-drain).

---

## Full-Run Lock

In **full** mode (only), each type is guarded by a cluster-wide NATS KV lock so two pods don't full-reindex the same type at once (`AcquireFullRunLock`, `internal/infrastructure/nats/backfill_lock.go`):

- Bucket: `membership-cache`; key: `backfill-lock/full/<type>`; value: `<run_id>|<RFC3339 timestamp>`.
- Acquired via an atomic KV `Create`. If the key already exists and is **not** stale, the type is **skipped** (logged, added to `skipped_locked`).
- A held lock older than `backfillLockStaleTTL` (2h) — or a malformed value — is treated as stale and force-acquired (delete + re-create).
- The returned `release` deletes the key on a fresh `context.Background()` (5s timeout), so it still runs if the caller's context has expired.

Filtered and targeted modes take **no** lock. `cdc_repair` also takes **no** lock — see below.

---

## Quota Guard & Windowed Reindex

The `cdc_repair` drain has always been quota-gated. The **full** and **filtered** reindex paths are gated too: a large operator-triggered reindex pages all of Salesforce and can run the org to quota exhaustion (the 2026-07-20 incident, LFXV2-2765). **Targeted (`items`) is exempt** — it is bounded (≤100 items, ~1 SOQL batch after batching) and is the surgical incident tool that must stay available under pressure.

### Where the gate fires

- **Handler start gate (HTTP 503).** `GateBackfillStart` runs synchronously in `AdminReindex` before the fire-and-forget goroutine for full/filtered requests, returning `503` at/above `ADMIN_REINDEX_QUOTA_THRESHOLD` (default `0.80`) so the operator gets immediate feedback. Targeted/repair are exempt (repair has its own gate via `PrepareRepair`).
- **`runType` start gate (Job path).** The same `checkQuotaGate` core also runs at the top of `runType`, covering the avatar-backfill Job's direct `Run` call (which bypasses the handler). On a gated start the Job logs and exits non-zero (reschedule when quota recovers).
- **Mid-run passive check.** Paged full/filtered runs re-read the **passive** gauge (no extra `/limits` call) at the top of each page and stop before the next; the flat `b2b_org_settings` loop checks every 100 iterations. A trip returns the `errQuotaStop` sentinel, which `Run` logs as a clean stop with `stopped_early=true` (not a failure).

The gate **fails open** when no quota gauge is wired (`REPOSITORY_SOURCE != salesforce`, or mock mode) — preserving the pre-guard ungated behavior. `PrepareRepair` differs: it fails **closed** (a drain deletes durable markers, so it must never run blind). Only the refresh→fallback→threshold core is shared.

### Stop-and-rerun converges only for BOUNDED runs

When the guard trips, the run **stops** — there is no watermark. Because the `Iter*` queries carry no `ORDER BY`, a re-run re-pages from the start. For a **bounded** run (targeted, or a `[since, until]` window small enough to finish below threshold) an idempotent re-run converges. For an **unbounded** full/since reindex under sustained quota pressure, stop-and-rerun re-burns quota on already-processed early pages, trips again, and never reaches the tail — a livelock. **Part B alone does not guarantee a large full reindex completes.**

### Windowed reindex (`since` + `until`)

`until` is the operator lever that makes a large reindex converge under pressure: slice it into bounded `[since, until]` windows each sized to finish below threshold.

```bash
# Before any large reindex, check headroom (>85% ⇒ pause):
./bin/scripts/check-sf-quota.sh

# Reindex a bounded month-long window (both bounds inclusive):
curl -XPOST .../admin/reindex -d '{
  "type": "project_membership",
  "since": "2025-01-01T00:00:00Z",
  "until": "2025-02-01T00:00:00Z"
}'
```

- **Run windows sequentially.** Filtered/windowed runs take **no** full-run lock (only `full` does), so several concurrent windows compound quota against the same gate. Fire them one at a time; the per-run start gate still refuses each new window once the shared gauge crosses threshold.
- **`key_contact` caveat.** The window (like `since`) only observes `Project_Role__c.LastModifiedDate`; Contact/Asset field changes are not captured (logged as a warning).

---

## CDC Quota-Repair Drain

The CDC consumer's quota guard (`internal/service/cdc_consumer.go`, documented in [CDC Consumer — Quota Guard](./cdc-consumer.md#quota-guard)) writes a durable pending marker to the `cdc-repair` NATS KV bucket for every record it skips while the Salesforce API quota is exhausted. `cdc_repair:true` on `/admin/reindex` drains that queue for one type.

### Marker schema

`internal/infrastructure/nats/cdc_repair.go`, `cdc-repair` bucket (no TTL, `history: 1` — see [NATS Storage](#nats-storage)):

- **Key:** `pending.{reindex_type}.{sfid}` — `reindex_type` is one of `b2b_org`, `project_membership`, `key_contact`; `sfid` is the canonical 18-character Salesforce ID. One key per `(type, record)` pair; a later skip for the same record overwrites the same key (`PutPending` is a plain `kv.Put`, not append-only).
- **Two more registered types are drain-exempt.** `pkg/constants/reindex_types.go` also registers `b2b_org_delete_access` and `project_membership_delete_access` in the same `cdc-repair` bucket's allowlist (`internal/infrastructure/nats/cdc_repair.go`) so `PutPending` accepts them, but this drain (`cdc_repair:true`) never selects them — `Type` must be one of the three quota-skip types above; the other two are markers for a failed `delete_access` publish, not a quota skip, and `reindexItem`'s targeted repair cannot re-emit a purge for a genuinely deleted record. See [Delete_access failure marker](./cdc-consumer.md#delete_access-failure-marker) for that manual recovery path.
- **Value:** `{"skipped_at": "<RFC 3339 timestamp>"}` — deliberately minimal. There is no retry count, last-error field, or original-entity field in the marker; the type and SFID live in the key, and per-item failure detail is in the drain's structured logs (`repair item retained for next drain`), not the marker itself. A malformed value is still retained and acted on (`SkippedAt` left zero) rather than treated as absent.
- **Read/list is targeted-only:** both the consumer's writer and the drain's `ListPending` operate through a type-filtered key stream (`pending.{type}.>`); neither ever enumerates the full bucket, so queue depth/age across all types is not observable from a single call (there is no depth counter or `>48h` staleness alert in this release — deferred as non-essential to the fix).

### Why there is no distributed lock

Every other write path that could race across the API's 3 replicas takes a NATS KV lock (see [Full-Run Lock](#full-run-lock)). `cdc_repair` deliberately does not, because the concurrency analysis comes out safe without one:

- `reindexItem` (the function `RunRepair` shares with `runTargeted`) only republishes **idempotent** projections — indexer docs and FGA tuples keyed by entity UID. Two replicas draining the same type concurrently would double-publish, which is a no-op downstream.
- The only shared mutable state is the marker itself, and `DeletePending` is **revision-conditional**: the loser of a concurrent delete gets a `Conflict` and simply retains the marker for the next drain (`RunRepair` logs it as `retry_retained`, no data is lost).
- A distributed lock across 3 replicas needs a lease/TTL to survive a pod crash mid-drain (the async goroutine outlives the HTTP request that acquired it); building and testing that lease correctly is real complexity bought only to save a handful of duplicate Salesforce reads on the rare occasion two operators trigger a drain at once. The quota gate and the 100-item page cap already bound how much duplicate work is possible.

So: **idempotent reindex + revision-conditional delete is the sole race guard.** Concurrent `cdc_repair` runs for the same type are safe by design, not by mutual exclusion.

### Two-phase drain

1. **`PrepareRepair` (synchronous, in the HTTP request):**
   - Requires both a repair store and a quota gauge to be wired (`WithRepairStore`, `WithQuotaGauge` in `cmd/member-api/service/providers.go`, Salesforce mode only) — returns `503` ("not configured") otherwise.
   - Issues an active quota refresh (falls back to the last valid observation if the refresh request fails); returns `503` if the quota has never been observed (truly unreadable).
   - Returns `503` if `current/limit >= ADMIN_REINDEX_QUOTA_THRESHOLD` (default `0.80` — see [Threshold asymmetry](#threshold-asymmetry) below).
   - Otherwise lists up to 100 pending markers for `Type` from the `cdc-repair` bucket (`ListPending`) and returns them as `selected_count`.
2. **`RunRepair` (asynchronous goroutine, `context.WithoutCancel`):**
   - For each marker: calls `reindexItem`, then on `issued`/`not_found` revision-conditionally deletes the marker; on `retry` (a fetch, dependency-resolution, persistence, or partial-projection failure) the marker is retained and logged (`repair item retained for next drain`) for the next run.
   - Before each item, re-reads the **passive** gauge (no additional `/limits` call) and stops mid-page if usage has crossed the threshold since the page was selected (`stopped_early` in the summary log) — this catches live traffic pushing usage up while a page is draining.

### Threshold asymmetry

The consumer's `CDC_QUOTA_SKIP_THRESHOLD` (default `0.95`) and the drain's `ADMIN_REINDEX_QUOTA_THRESHOLD` (default `0.80`) are deliberately different values. Between 0.80 and 0.95, the consumer is processing normally (below its skip threshold) while `cdc_repair` refuses to start (at/above its own, lower threshold). This is intentional headroom so a drain never competes with live CDC traffic for the last slice of daily quota — but it means an operator can observe "consumer healthy, repair queue not draining" in that band; that is expected, not a bug.

### Operator workflow / runbook

1. **Confirm there is something to drain.** The bucket is never enumerated across all types by the service itself (see [Marker schema](#marker-schema)), so inspect it directly against the running NATS server (e.g. via `kubectl exec` into a pod with `nats` CLI access, or `nats context select` against the cluster):
   ```bash
   # List pending markers for one type (keys only, cheap):
   nats kv ls cdc-repair --keys | grep '^pending.project_membership\.'

   # Inspect one marker's value (skipped_at) and revision:
   nats kv get cdc-repair 'pending.project_membership.001XXXXXXXXXXXXAAA' --raw
   ```
2. **Drain a page.** `POST /admin/reindex {"type": "<type>", "cdc_repair": true}` for each affected type (`b2b_org`, `project_membership`, `key_contact`). A `503` response means the quota gate refused the run — wait for quota headroom (see [Threshold asymmetry](#threshold-asymmetry)) and retry.
3. **Re-run until drained.** Each call selects up to 100 markers, so a backlog larger than that needs multiple calls; repeat step 2 until the response's `selected_count` is `0`. There is no `dry_run` or `has_more` signal for `cdc_repair` in this release (deferred as non-essential to the fix — see the change proposal), so `selected_count` is the only progress signal.
4. **Remediate persistent failures.** Markers that keep coming back are logged each drain as `repair item retained for next drain` (carries `type`/`uid`/`error`). A record retried across several drains without ever clearing usually means a dependency is down (e.g. the project-service NATS RPC the `project_uid` resolver depends on) rather than a transient blip — fix the underlying cause, then re-run the drain; the drain itself cannot resolve a persistent dependency failure.
5. **Confirm convergence.** After `selected_count` reaches `0` for a type, re-run `nats kv ls` for that type's prefix to confirm no markers remain (a marker can reappear if the consumer skips the same record again before you finish draining — that is expected, not a bug).

---

## Per-Type Handling

### Full / filtered (`runType`, paged)

| Type | Per-record actions (when not dry-run) |
|---|---|
| `b2b_org` | Batched child-UID fetch for every org + its parent (`FetchChildUIDsByParentUIDs`, one query per page) → set `IsParent` → `PublishB2BOrgIndexer` (`updated`) + `PublishB2BOrgTeamGrantsFGA` (the `global_org_admin` grant and the auditor team grants; publishes when *either* is configured) + `PublishB2BOrgParentFGA` when `ParentUID` is set and children are cached. |
| `project_membership` | resolve `project_uid` → on success: `PublishProjectMembershipIndexer` (`updated`) + `PublishProjectMembershipFGA`. On resolver failure: skip indexer, log ERROR, **OpenFGA only**. |
| `key_contact` | resolve LFID via `userReader.UsernameByEmail` when the assembled record has no `Username` (the SOQL/sObject sources set `Email` only) → resolve `project_uid` → on success: `PublishKeyContactIndexer` (`updated`) + `PublishKeyContactFGA`. On resolver failure: skip indexer, log ERROR, **OpenFGA only** (when `Username` resolved). Logs a warning when `since` is set (the filter only checks `Project_Role__c.LastModifiedDate`; Contact/Asset field changes are not captured). |
| `b2b_org_settings` | List org UIDs (`ListSettingsOrgUIDs`) → `GetSettings` → (optionally enrich avatars) → `GetB2BOrg` → `PublishB2BOrgSettingsIndexer` (`updated`). Requires a `settingsReader`; avatar enrichment additionally requires a `userReader` + `settingsWriter`. Publishes the **indexer** doc only (no FGA message). |

### Targeted (`runTargeted`)

`project_membership` and `key_contact` — the prod volume drivers — are fetched in **one SOQL batch** (`FetchMembershipsBySFIDs` / `FetchKeyContactsBySFIDs`, the same batch ports the CDC consumer uses), cutting per-request cost from ~3–5 SF calls/item to ~1 batch. Each returned record is resolved (`resolveProjectUID`) and published with the same per-type calls as the full/filtered path. A requested SFID **absent** from the batch result (soft-deleted or no longer membership-eligible) is classified `not_found` and skipped; a SFID present in SOQL but unconvertible is counted as a distinct **`conversion_error`** (neither published nor deleted). This is the *lighter* batch path — deliberately **no** cache-invalidation or absent-as-delete convergence (that is LFXV2-2808). When the batch reader is unwired (mock mode) PM/KC fall back to the per-item path below.

`b2b_org` (and any type without a wired batch reader) stays **per item** via the shared `reindexItem` helper (the same one `RunRepair` uses for `cdc_repair`): `GetB2BOrg` applies child-UID/parent-FGA logic that a batch semi-join would change, so it is intentionally not batched. `b2b_org_settings` targeted also runs per item (`GetB2BOrg`+`GetSettings`). Child-UID lookups are memoised within the request so siblings sharing a parent don't each issue a SOQL call. `outcomeNotFound` is counted as `not_found`; `outcomeRetry` is logged inside `reindexItem` and otherwise ignored (targeted mode does not retain/retry a marker — that persistence is exclusive to `cdc_repair`).

The final log line reports `total_items`, `published`, `not_found`, and `would_publish_count`; the batched arms additionally report `conversion_error` so `published + not_found + conversion_error` reconciles against `total_items`.

`PublishKeyContactFGA` emits its `member_put` grant only when the assembled record has a non-empty `Username` (`internal/service/key_contact_grant.go`). Every `key_contact` source this runner reads (full/filtered SOQL page, single-item sObject assembly, and the SFID batch reader) sets `Email` but never `Username`; `resolveKeyContactUsername` (`internal/service/backfill_runner.go`) resolves it via `userReader.UsernameByEmail` before every publish call, mirroring the CDC consumer's `processKeyContact`. Without that resolution step every `key_contact` reindex would silently publish nothing and record nothing.

`UsernameByEmail` distinguishes a definitive "no registered account" miss from a transport-level failure (auth-service unreachable). A definitive miss leaves `Username` empty (so `PublishKeyContactFGA` publishes nothing this pass) and revokes any grant still recorded for the contact in the `key-contact-grants` bucket — the email was renamed or deregistered since the last successful grant, so a stale FGA tuple would otherwise survive indefinitely. A transport-level failure is not evidence the email is unregistered: it is logged and swallowed, `Username` stays empty, and any existing grant is left untouched for a later pass to retry.

Each published grant is recorded in the `key-contact-grants` KV bucket, which is what lets a later CDC delete address the revoke. A `key_contact` reindex is therefore also a way to populate that bucket for contacts granted before it existed. Over a cold bucket the run emits `member_put` only: there is no recorded grant to compare against, so no supersede `member_remove` traffic is produced. It does **not** repair tuples that were already orphaned before the bucket existed — those grants were never recorded, so nothing knows they need revoking.

---

## `project_uid` Resolution Parity

`project_membership` and `key_contact` records carry a project **slug**, but the indexer's project-scoped tags/parent-refs key off the v2 project **UID**. The runner resolves it through a helper shared with the CDC consumer (`internal/service/project_uid.go`). The API read path (`salesforce.MemberReader`) resolves the same way via `resolver.UIDFromSlug` but does not go through this helper:

```25:35:internal/service/project_uid.go
func resolveProjectUID(ctx context.Context, resolver port.ProjectResolver, slug, current string) (string, bool) {
	if current != "" || slug == "" || resolver == nil {
		return current, true
	}
	uid, err := resolver.UIDFromSlug(ctx, slug)
	if err != nil {
		slog.ErrorContext(ctx, "failed to resolve project UID", "slug", slug, "error", err)
		return "", false
	}
	return uid, true
}
```

- Applied to `project_membership` and `key_contact` in both `runType` and `runTargeted` before publishing.
- On a **transient resolution failure** (`ok == false`) the runner **skips only the indexer publish** (logged at **ERROR**, `publish_failed_for_backfill_repair=true`) and **still publishes OpenFGA**; re-run the backfill once project-service is reachable to repair the indexer doc.
- An already-populated `ProjectUID` is never re-resolved.

See [`project_uid` Resolution Parity](./cdc-consumer.md#project_uid-resolution-parity) in the CDC doc for the same helper on the streaming path.

---

## Avatar Enrichment (`avatar-backfill`)

`RUN_MODE=avatar-backfill` runs `RunAvatarBackfill` (`cmd/member-api/service/avatar_backfill.go`), which builds a full-mode `b2b_org_settings` request with `EnrichAvatars=true` (`AvatarBackfillRequest`) and hands it to the same `Runner.Run`. It reuses the run-id, dry-run, and full-run-lock control plane rather than a separate backfill path, and returns the runner's error so the Job exits non-zero on a systemic failure.

Per org, `enrichSettingsAvatars` (`backfill_runner.go`) refreshes each accepted writer/auditor avatar from the auth-service (`UserReader.UserMetadataByPrincipal`):

- **Sync-and-clear:** a successful lookup overwrites the stored avatar (an emptied Auth0 photo blanks the indexed avatar); a `NotFound` leaves the existing value untouched.
- **Idempotent:** an org with no avatar drift is skipped (nothing persisted or republished) — so the Job doubles as a recurring refresh.
- **`AvatarMissingOnly`** limits enrichment to principals whose avatar is currently empty.
- **`AvatarSleep`** waits between lookups to respect Auth0 rate limits; a context cancellation during the sleep aborts the pass cleanly (not counted as a failure).
- **Failure tolerance:** transient lookup failures are counted and isolated; the type is reported failed only when they exceed `maxToleratedAvatarFailures` (50).
- Enriched avatars are persisted via `UpdateSettings` (skipped on a revision conflict) **before** the indexer doc is republished, so the doc only reflects persisted values.

---

## Dry Run

`dry_run=true` walks the read path but skips every publish. The full/filtered path counts records; the targeted path reports `would_publish_count`. For `b2b_org_settings`, a **non-enrich** dry-run counts only (it does not read each settings record); an enrich dry-run still performs the avatar drift computation without persisting or publishing.

---

## Failure Modes & Log Signals

The runner is resilient: a per-record fetch/publish failure is logged and the run continues.

| Log key / line | Meaning | Recovery |
|---|---|---|
| `publish_failed_for_backfill_repair=true` | A fetch/persist/publish for one record failed; it may be missing/stale downstream. | Re-run the reindex for the affected type/record. |
| `skipping … indexer publish; project_uid unresolved` | Resolver could not map slug→UID; indexer skipped, OpenFGA still published (ERROR). | Re-run once project-service is reachable to repair the indexer doc. |
| `full reindex skipped — lock held` (`full_reindex_rejected_lock_held=true`) | Another pod holds the full-run lock for this type. | Wait for the running reindex, or retry later. |
| `not_found=true` | A targeted/settings item did not resolve to a record. | Verify the UID; nothing published for it. |
| `since/until filter on key_contact only checks Project_Role__c.LastModifiedDate` | The `since`/`until` window misses Contact/Asset-only field changes for key contacts. | Use a full `key_contact` reindex if joined-field changes must be captured. |
| `backfill stopped early — salesforce quota threshold reached` (`stopped_early=true`) | A full/filtered run hit `ADMIN_REINDEX_QUOTA_THRESHOLD` at start or between pages. | Re-run once quota recovers; for large runs, window it via `since`/`until` (see [Quota Guard & Windowed Reindex](#quota-guard--windowed-reindex)). |
| `avatar enrichment lookup failed` | An auth-service lookup failed (avatar Job). | Tolerated up to the failure limit; a systemic outage fails the Job (non-zero exit). |
| `repair item retained for next drain` | A `cdc_repair` item hit a fetch/dependency/persistence/partial-projection failure; its marker was kept. | Inspect the log line's `error` field; re-run `cdc_repair` for the type once the underlying cause (e.g. project-service outage) is fixed. |
| `repair marker retained — conditional delete failed` | The revision-conditional `DeletePending` lost a race (another drain or a fresh consumer skip updated the marker first). | Harmless — the newer marker is picked up by the next drain. |
| `repair drain stopping mid-page — quota threshold reached` (`publish_failed_for_backfill_repair=true`) | Live traffic pushed passive usage to/above `ADMIN_REINDEX_QUOTA_THRESHOLD` while a page was draining. | Re-run `cdc_repair` for the type once quota headroom returns; unprocessed markers in the page remain pending. |

**Run-level result:** in full/filtered mode, `Run` returns an error listing any `failed` types. The HTTP path ignores this (fire-and-forget); the avatar Job uses it for its exit code. The summary log line reports `succeeded`, `failed`, and `skipped_locked`.

> **Operational note — `key_contact` is high-volume (~300k records in prod).** Reindex only the active window with a `since` ~2 years back (e.g. `{"type":"key_contact","since":"2024-06-01T00:00:00Z"}`) rather than a full `key_contact` reindex, which takes hours and is likely to be interrupted by pod eviction.

---

## Configuration

| Variable | Description | Default |
|---|---|---|
| `ADMIN_REINDEX_QUOTA_THRESHOLD` | Fraction of the daily Salesforce REST quota (`current/limit`) at/above which the backfill quota guard refuses/stops a run. Covers the `cdc_repair` drain (`PrepareRepair`/`RunRepair`) **and** the full/filtered reindex paths (`GateBackfillStart` → HTTP `503`; `runType` start + mid-run stop). Targeted (`items`) is exempt. See [Quota Guard & Windowed Reindex](#quota-guard--windowed-reindex). | `0.80` |

The full/filtered/targeted paths take no other dedicated environment variables beyond the shared service/Salesforce/NATS config (see [README](../README.md) / [CLAUDE.md](../CLAUDE.md)). The corresponding consumer-side variable (`CDC_QUOTA_REFRESH_STALE_AFTER`, `CDC_QUOTA_SKIP_THRESHOLD`) that governs what gets written into the queue this endpoint drains is documented in [CDC Consumer — Configuration](./cdc-consumer.md#configuration).

Avatar-backfill Job variables (read only when `RUN_MODE=avatar-backfill`):

| Variable | Description | Default |
|---|---|---|
| `AVATAR_BACKFILL_DRY_RUN` | Compute drift without writing; set `false` to persist. | `true` |
| `AVATAR_BACKFILL_MISSING_ONLY` | Only enrich principals whose avatar is currently empty. | `false` |
| `AVATAR_BACKFILL_SLEEP` | Go duration between auth-service lookups (Auth0 rate cap). | `0` |

`REPOSITORY_SOURCE=mock` runs the avatar Job end to end without Salesforce creds (mock readers/writers).

---

## NATS Storage

| Bucket | Use by the runner | Notes |
|---|---|---|
| `membership-cache` | (1) Full-mode per-type lock (`backfill-lock/full/<type>`). (2) Read/written by the injected `ProjectResolver` for `project-uid.<slug>` lookups while resolving `project_uid` before publishing `project_membership` / `key_contact`. | Lock held for the run; force-acquired if the embedded timestamp is older than 2h (`backfillLockStaleTTL`). Resolver entries use the standard 6 h stale / 23 h expire envelope. |
| `org-settings` | `b2b_org_settings` reindex reads settings; avatar enrichment persists updated avatars back (`UpdateSettings`). | Authoritative (no soft-TTL). |
| `cdc-repair` | `cdc_repair` mode lists (`ListPending`) and revision-conditionally deletes (`DeletePending`) pending markers written by the CDC consumer. **Not written by the runner** — see [CDC Consumer — NATS Storage](./cdc-consumer.md) for the write side. | Authoritative (no TTL, `history: 1`); no distributed lock guards concurrent drains (see [CDC Quota-Repair Drain](#cdc-quota-repair-drain)). |

---

## Observability

Every log line carries `run_id`, `component=backfill`, `mode`, and `dry_run`. The lifecycle emits `backfill started` / `backfill page processed` (with `total_so_far` / `published_so_far`) / `backfill summary` for full/filtered runs, and `targeted backfill complete` for targeted runs. Correlate a run by grepping logs for its `run_id`.
