# Backfill / Reindex — Member Service

This document is the authoritative reference for the **backfill runner** — the code that re-reads entities from Salesforce (and, for settings, the `org-settings` KV) and re-publishes them to the indexer and fga-sync services on demand, as a recovery/repair tool.

One `service.Runner` (`internal/service/backfill_runner.go`) backs two entry points:

- **`POST /admin/reindex`** — an async HTTP admin action that reindexes a full, since-filtered, or targeted set of entities.
- **`RUN_MODE=avatar-backfill`** — a one-off Kubernetes Job that re-enriches `b2b_org_settings` avatars and republishes them.

Both hand a `BackfillRequest` to the same `Runner.Run`, so they share the run-id, dry-run, and full-run-lock control plane.

**Update this document in the same PR as any change to the backfill runner, the `/admin/reindex` payload/validation, or the avatar-backfill Job.**

For the downstream message payloads the runner produces, see:

- [FGA Contract](./fga-contract.md) — the fga-sync messages (`update_access`, `member_put`).
- [Indexer Contract](./indexer-contract.md) — the OpenSearch indexer messages (`created`/`updated`).
- [CDC Consumer](./cdc-consumer.md) — the near-real-time sync path; `/admin/reindex` is its backstop.

---

## Contents

- [Entry Points](#entry-points)
- [Request Model](#request-model)
- [Run Modes](#run-modes)
- [HTTP Endpoint](#http-endpoint)
- [Full-Run Lock](#full-run-lock)
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

`BackfillRequest` (`internal/service/backfill_request.go`) is the validated, normalised input for a single run:

| Field | Meaning |
|---|---|
| `RunID` | Correlation ID stamped on every log line for the run. Set to a fresh UUID by the HTTP handler and the avatar Job. |
| `Types` | Entity types to reindex; empty = all in-scope. Used in full/filtered modes. |
| `Since` | `nil` = full reindex; otherwise only records with `LastModifiedDate >= Since`. |
| `Items` | Targeted list of `{Type, UID}` for surgical reindex. Mutually exclusive with `Types`/`Since`. |
| `DryRun` | Walk the read path but skip publishing. |
| `EnrichAvatars` / `AvatarMissingOnly` / `AvatarSleep` | Avatar-backfill Job only; **not** exposed on the HTTP payload. |

In-scope types (`allBackfillTypes`): `b2b_org`, `project_membership`, `key_contact`, `b2b_org_settings`.

`ReindexItem` is `{Type, UID}` for targeted mode.

---

## Run Modes

`ClassifyMode` selects the mode from the request:

| Mode | Condition | Iteration source |
|---|---|---|
| `targeted` | `len(Items) > 0` | the explicit `Items` list (`runTargeted`) |
| `filtered` | `Since != nil` | paged SOQL `Iter*` with the `since` filter (`runType`) |
| `full` | otherwise | paged SOQL `Iter*` over all records (`runType`) |

`filtered` and `full` iterate `effectiveTypes(req.Types)` — the requested types, or all in-scope types when `Types` is empty. Only **full** mode acquires the per-type NATS lock and logs `full_reindex_started=true`.

---

## HTTP Endpoint

`POST /admin/reindex` — handler `AdminReindex` (`cmd/member-api/service/membership_service.go`).

**Authorization:** `member` on `team:{globalOrgAdminTeamUID}` (Heimdall ruleset).

**Payload** (`AdminReindexPayload`, `cmd/member-api/design/type.go`):

| Field | Notes |
|---|---|
| `types` | Optional array; default = all in-scope. Mutually exclusive with `items`. |
| `since` | RFC 3339 with explicit zone; normalised to UTC. Mutually exclusive with `items`. |
| `items` | Targeted `{type, uid}` list; **max 100** (`MaxLength(100)`). Mutually exclusive with `types`/`since`. |
| `dry_run` | Default `false`. |

**Validation** (`ValidateAndBuildRequest`):

- Unknown types are rejected; `membership_tier` is rejected with a specific message (not currently supported).
- `items` is mutually exclusive with `types`/`since`.
- Each item's `type` must be valid and its `uid` must be a Salesforce ID (`sfuuid.IsSFID`).
- `since` must parse as RFC 3339; it is converted to UTC.

**Response:** HTTP `202 Accepted` with `{ "run_id": "<uuid>" }`. The handler generates the `run_id`, logs the accepted request, then runs the backfill in a goroutine on `context.WithoutCancel(ctx)` so it outlives the HTTP request.

- The goroutine's return value is **ignored** — `/admin/reindex` is fire-and-forget; track progress by grepping logs for `run_id=<uuid>`.
- A pod restart during a large run interrupts it mid-flight (partial index, no error logged); re-trigger to repair.
- If the runner is not initialised, the handler logs a warning and still returns the `run_id`.

---

## Full-Run Lock

In **full** mode (only), each type is guarded by a cluster-wide NATS KV lock so two pods don't full-reindex the same type at once (`AcquireFullRunLock`, `internal/infrastructure/nats/backfill_lock.go`):

- Bucket: `membership-cache`; key: `backfill-lock/full/<type>`; value: `<run_id>|<RFC3339 timestamp>`.
- Acquired via an atomic KV `Create`. If the key already exists and is **not** stale, the type is **skipped** (logged, added to `skipped_locked`).
- A held lock older than `backfillLockStaleTTL` (2h) — or a malformed value — is treated as stale and force-acquired (delete + re-create).
- The returned `release` deletes the key on a fresh `context.Background()` (5s timeout), so it still runs if the caller's context has expired.

Filtered and targeted modes take **no** lock.

---

## Per-Type Handling

### Full / filtered (`runType`, paged)

| Type | Per-record actions (when not dry-run) |
|---|---|
| `b2b_org` | Batched child-UID fetch for every org + its parent (`FetchChildUIDsByParentUIDs`, one query per page) → set `IsParent` → `PublishB2BOrgIndexer` (`updated`) + `PublishB2BOrgGlobalAdminFGA` + `PublishB2BOrgParentFGA` when `ParentUID` is set and children are cached. |
| `project_membership` | resolve `project_uid` → `PublishProjectMembershipIndexer` (`updated`) + `PublishProjectMembershipFGA`. |
| `key_contact` | resolve `project_uid` → `PublishKeyContactIndexer` (`updated`) + `PublishKeyContactFGA`. Logs a warning when `since` is set (the filter only checks `Project_Role__c.LastModifiedDate`; Contact/Asset field changes are not captured). |
| `b2b_org_settings` | List org UIDs (`ListSettingsOrgUIDs`) → `GetSettings` → (optionally enrich avatars) → `GetB2BOrg` → `PublishB2BOrgSettingsIndexer` (`updated`). Requires a `settingsReader`; avatar enrichment additionally requires a `userReader` + `settingsWriter`. Publishes the **indexer** doc only (no FGA message). |

### Targeted (`runTargeted`, per item)

Each `Item` is fetched individually and republished with the same per-type publish calls as above (`GetB2BOrg`, `AssembleProjectMembership`, `AssembleKeyContact`, `GetB2BOrg`+`GetSettings`). Child-UID lookups are memoised within the request so siblings sharing a parent don't each issue a SOQL call. A missing record is counted as `not_found` and skipped. The final log line reports `total_items`, `published`, `not_found`, and `would_publish_count`.

`PublishKeyContactFGA` emits its `member_put` grant only when the assembled record has a non-empty `Username` (`internal/service/messaging.go`); the backfill publishes the record's FGA state as assembled from Salesforce and does not itself perform an email→LFID lookup.

---

## `project_uid` Resolution Parity

`project_membership` and `key_contact` records carry a project **slug**, but the indexer's project-scoped tags/parent-refs key off the v2 project **UID**. The runner resolves it through a helper shared with the CDC consumer (`internal/service/project_uid.go`). The API read path (`salesforce.MemberReader`) resolves the same way via `resolver.UIDFromSlug` but does not go through this helper:

```24:34:internal/service/project_uid.go
func resolveProjectUID(ctx context.Context, resolver port.ProjectResolver, slug, current string) (string, bool) {
	if current != "" || slug == "" || resolver == nil {
		return current, true
	}
	uid, err := resolver.UIDFromSlug(ctx, slug)
	if err != nil {
		slog.WarnContext(ctx, "failed to resolve project UID", "slug", slug, "error", err)
		return "", false
	}
	return uid, true
}
```

- Applied to `project_membership` and `key_contact` in both `runType` and `runTargeted` before publishing.
- On a **transient resolution failure** (`ok == false`) the runner **skips** the publish for that record (`publish_failed_for_backfill_repair=true`) rather than overwriting an existing `project_uid` with an empty value; re-run the backfill once project-service is reachable.
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
| `full reindex skipped — lock held` (`full_reindex_rejected_lock_held=true`) | Another pod holds the full-run lock for this type. | Wait for the running reindex, or retry later. |
| `not_found=true` | A targeted/settings item did not resolve to a record. | Verify the UID; nothing published for it. |
| `since filter on key_contact only checks Project_Role__c.LastModifiedDate` | The `since` window misses Contact/Asset-only field changes for key contacts. | Use a full `key_contact` reindex if joined-field changes must be captured. |
| `avatar enrichment lookup failed` | An auth-service lookup failed (avatar Job). | Tolerated up to the failure limit; a systemic outage fails the Job (non-zero exit). |

**Run-level result:** in full/filtered mode, `Run` returns an error listing any `failed` types. The HTTP path ignores this (fire-and-forget); the avatar Job uses it for its exit code. The summary log line reports `succeeded`, `failed`, and `skipped_locked`.

> **Operational note — `key_contact` is high-volume (~300k records in prod).** Reindex only the active window with a `since` ~2 years back (e.g. `{"types":["key_contact"],"since":"2024-06-01T00:00:00Z"}`) rather than a full `key_contact` reindex, which takes hours and is likely to be interrupted by pod eviction.

---

## Configuration

The HTTP endpoint takes no dedicated environment variables beyond the shared service/Salesforce/NATS config (see [README](../README.md) / [CLAUDE.md](../CLAUDE.md)).

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

---

## Observability

Every log line carries `run_id`, `component=backfill`, `mode`, and `dry_run`. The lifecycle emits `backfill started` / `backfill page processed` (with `total_so_far` / `published_so_far`) / `backfill summary` for full/filtered runs, and `targeted backfill complete` for targeted runs. Correlate a run by grepping logs for its `run_id`.
