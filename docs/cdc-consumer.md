# CDC Consumer — Member Service

This document is the authoritative reference for the **Change Data Capture (CDC) consumer** — the mode of the member-service binary that keeps OpenSearch and OpenFGA in near-real-time sync with Salesforce.

The consumer subscribes to Salesforce [Pub/Sub API](https://developer.salesforce.com/docs/platform/pub-sub-api/overview) change events, re-fetches each changed record from Salesforce, and republishes it to the indexer and fga-sync services — the same messages the write API and the `/admin/reindex` backfill emit.

**Update this document in the same PR as any change to CDC event handling, dispatch, or the Pub/Sub adapter.**

For the downstream message payloads this consumer produces, see:

- [FGA Contract](./fga-contract.md) — the fga-sync messages (`update_access`, `member_put`, `member_remove`).
- [Indexer Contract](./indexer-contract.md) — the OpenSearch indexer messages (`created`/`updated`/`deleted`).

---

## Contents

- [Run Mode](#run-mode)
- [Architecture](#architecture)
- [Event Model](#event-model)
- [Replay Cursor & Delivery Semantics](#replay-cursor--delivery-semantics)
- [Dispatch Flow](#dispatch-flow)
- [Per-Entity Handling](#per-entity-handling)
- [`project_uid` Resolution Parity](#project_uid-resolution-parity)
- [b2b_org Hierarchy Tuples](#b2b_org-hierarchy-tuples)
- [Cross-Cutting Behaviour](#cross-cutting-behaviour)
- [Failure Modes & Log Signals](#failure-modes--log-signals)
- [Kubernetes Deployment](#kubernetes-deployment)
- [Configuration](#configuration)
- [NATS Storage](#nats-storage)
- [Observability](#observability)

---

## Run Mode

The same binary runs as either the HTTP API or the CDC consumer, selected by `RUN_MODE`:

| `RUN_MODE` | Behaviour | Entry point |
|---|---|---|
| _(unset)_ | HTTP API server (default) | `cmd/member-api/main.go` |
| `consumer` | CDC event consumer | `runConsumer` — `cmd/member-api/main.go` |
| `avatar-backfill` | One-off avatar backfill Job | `RunAvatarBackfill` — `cmd/member-api/service/avatar_backfill.go` |

`runConsumer` wires the consumer via `service.CDCConsumerImpl` (`cmd/member-api/service/providers.go`), reads the channel from `service.CDCChannelFromEnv` (default `/data/ChangeEvents`), and blocks on `CDCConsumer.Run` until a SIGINT/SIGTERM cancels the context. A minimal `GET /livez` health server runs on the same port as the API.

---

## Architecture

The consumer follows the hexagonal boundary used across the service. `goavro`/gRPC/proto types never cross into the domain — the Pub/Sub adapter normalises everything to `model.CDCEvent` first.

```text
Salesforce Pub/Sub gRPC API (Avro-encoded ChangeEvents)
        │
        ▼
pubsub.Client  (internal/infrastructure/salesforce/pubsub)      ── implements port.CDCSubscriber
  • bidirectional gRPC stream, batch FetchRequest (100 events)
  • Avro schema + codec cache (per schema_id)
  • decode → model.CDCEvent
        │  <-chan model.CDCEvent
        ▼
CDCConsumer.Run  (internal/service/cdc_consumer.go)
  • one event at a time; commit replay cursor after each
  • dispatch by Entity → per-entity handler
        │
        ├── Account          → handleAccount        → b2b_org
        ├── Asset            → handleAsset          → project_membership
        └── Project_Role__c  → handleProjectRole    → key_contact
        │
        ▼
  invalidate sObject cache → batched SOQL re-fetch → resolve project_uid (Asset / Project_Role__c)
        │
        ▼
  MemberPublisher → NATS → indexer-service + fga-sync
  (on resolver failure: OpenFGA only — indexer skipped, ERROR logged)
```

Key ports (`internal/domain/port/cdc.go`):

- **`CDCSubscriber`** — `Subscribe(ctx, channel, replayID, replayStore) (<-chan model.CDCEvent, error)`. Implemented by `pubsub.Client`.
- **`ReplayStore`** — `Load` / `Save` the opaque replay cursor. Implemented by `pubsub.ReplayStore` over the `pubsub-state` NATS KV bucket.

The consumer's dependencies are injected through `CDCConsumerOption` functions (`WithCDC*`). The batch readers (`MembershipBatchReader`, `KeyContactBatchReader`, `AccountBatchReader`) are **uncached SOQL repos** (`salesforce.NewMembershipRepo` / `NewKeyContactRepo` / `NewAccountRepo`) — one batched `IN`-clause query per event instead of a per-record sObject fan-out. They sit below the `MemberReader` stale-while-revalidate layer and call the Salesforce query API directly, so the published record always reflects the CDC change.

---

## Event Model

`model.CDCEvent` (`internal/domain/model/cdc_event.go`) is the transport-agnostic event:

| Field | Meaning |
|---|---|
| `Entity` | Salesforce `entityName` from the `ChangeEventHeader` (`Account`, `Asset`, `Project_Role__c`). |
| `RecordIDs` | **Every** record ID in the event — batch DML emits multiple IDs per event; the consumer processes all of them. |
| `ChangeType` | The `ChangeEventHeader.changeType`. |
| `ReplayID` | Opaque per-event replay cursor, persisted commit-after-process. |

Change types (`model.CDCChangeType`):

| Value | Routed as | Notes |
|---|---|---|
| `CREATE` | upsert | Indexer action `created`. |
| `UPDATE` | upsert | Indexer action `updated`. |
| `UNDELETE` | upsert | Treated as upsert (`isDelete` uses exact equality so `UNDELETE` is **not** matched as a delete). |
| `DELETE` | delete | Publishes a delete indexer event; no re-fetch. |
| `GAP_DELETE` | delete | Delete during a CDC overflow gap. |
| `GAP_OVERFLOW` / other `GAP_*` | upsert | Granular delivery was interrupted — re-fetch as upsert and log a WARN. |

---

## Replay Cursor & Delivery Semantics

Delivery is **at-least-once** with **commit-after-process** semantics:

1. `Run` loads the cursor from `ReplayStore` and passes it to `Subscribe` (nil ⇒ start at `LATEST`).
2. Each event is fully handled, then the cursor is `Save`d — **regardless of handler error** — so a transient failure does not block the stream forever.
3. The `Save` runs on a fresh `context.Background()` (5 s timeout) so a graceful shutdown that cancels the main context still commits the final cursor.
4. On gRPC stream reconnect, the adapter **reloads** the cursor from the store (not the last in-flight delivery), so an event delivered but not yet committed is re-delivered rather than skipped.

**Consequence:** handlers must be idempotent. The indexer and fga-sync messages are all idempotent upserts, so re-delivery is safe.

Cursor storage (`pubsub.ReplayStore`): key `pubsub-replay.<channel>` in the `pubsub-state` bucket, with `/` replaced by `_` (e.g. `pubsub-replay._data_ChangeEvents`). Authoritative state — no TTL.

---

## Dispatch Flow

For each event, `CDCConsumer.handle` switches on `Entity` and calls the per-entity handler. Every entity follows the same shape (`dispatchEntity` + `handle<Entity>UpsertBatch`):

1. **Normalise + partition** — each raw record ID is normalised to a canonical 18-char SFID (`sfuuid.Normalize18`); un-normalisable IDs are logged and skipped. IDs split into `deleteIDs` vs `upsertIDs` by change type.
2. **Deletes** — each delete ID runs through the entity's delete handler immediately.
3. **Upserts (batched):**
   - **Quota guard** — skip the re-fetch if the Salesforce REST API quota is near-exhausted (see [Quota guard](#quota-guard)).
   - **(Account only)** capture old record state for the reparenting diff before eviction.
   - **Invalidate** the sObject cache entry for each ID.
   - **Batched SOQL re-fetch** for all IDs in one query.
   - **Absent → delete convergence** — IDs requested but missing from the SOQL result are soft-deleted / no longer qualifying, so they are routed to the delete handler. IDs present-but-unconvertible are marked "seen" so they are **not** wrongly deleted.
   - **Resolve `project_uid`** from the record's slug (Asset + Project_Role__c).
   - **Publish** indexer + OpenFGA for each present record when resolution succeeds; on failure, **OpenFGA only** (indexer skipped — see [`project_uid` Resolution Parity](#project_uid-resolution-parity)).

---

## Per-Entity Handling

### Account → `b2b_org`

**Handler:** `handleAccount` / `handleAccountUpsertBatch` / `handleAccountDelete`
**Batch reader:** `AccountBatchReader.FetchAccountsBySFIDs`

| Change | Actions |
|---|---|
| Upsert | Invalidate b2b_org cache → fetch accounts → set `IsParent` from a batched child-UID query → `PublishB2BOrgIndexer` (`updated`) + `BuildB2BOrgFGAMessage` (`global_org_admin` always set, not create-only) + reparenting messages on a genuine parent change + **parent/child hierarchy tuples** (see below). |
| Delete | Invalidate cache → `PublishB2BOrgIndexer` (`deleted`, stub org) → `update_access` for the stub org (writers/auditors passed as `nil` = preserve; fga-sync reconciles tuple removal from the delete). |

### Asset → `project_membership`

**Handler:** `handleAsset` / `handleAssetUpsertBatch` / `handleAssetDelete`
**Batch reader:** `MembershipBatchReader.FetchMembershipsBySFIDs`

| Change | Actions |
|---|---|
| Upsert | Invalidate cache → fetch memberships → resolve `project_uid` → on success: `PublishProjectMembershipIndexer` (`created`/`updated`) + `PublishProjectMembershipFGA` (`b2b_org` + `project` refs; `key_contact` excluded). On resolver failure: skip indexer, log ERROR, **OpenFGA only** (`project` relation excluded when ref absent). |
| Delete | Invalidate cache → `PublishProjectMembershipIndexer` (`deleted`, stub — data is the UID string). No FGA (no tuple to revoke). |

### Project_Role__c → `key_contact`

**Handler:** `handleProjectRole` / `handleProjectRoleUpsertBatch` / `processKeyContact` / `handleProjectRoleDelete`
**Batch reader:** `KeyContactBatchReader.FetchKeyContactsBySFIDs`

| Change | Actions |
|---|---|
| Upsert | Invalidate cache → fetch contacts → per contact (`processKeyContact`): resolve LFID via `UserReader.UsernameByEmail` when username is empty → resolve `project_uid` → on success: `PublishKeyContactIndexer` + `PublishKeyContactFGA` (`member_put` only when an LFID resolved) + **silently** provision org-dashboard access (`AddPrincipal` with `SuppressNotification: true`). On resolver failure: skip indexer + provisioning, log ERROR, **OpenFGA only** (when LFID resolved). |
| Delete | Invalidate cache → `PublishKeyContactIndexer` (`deleted`, stub) → `member_remove` with empty username (fga-sync cleans up by object-id). |

> **CDC never sends email.** It is a passive sync. `processKeyContact` provisions org-dashboard access with `SuppressNotification: true`, and the org-settings resend-in-place path honours that flag (it refreshes the pending entry without re-sending an invite).

---

## `project_uid` Resolution Parity

Salesforce records carry a project **slug** and **SFID**, but the indexer's project-scoped search tags and parent references key off the v2 project **UID** (a UUID). The API read path (`salesforce.MemberReader`, via `resolver.UIDFromSlug`) and the `/admin/reindex` backfill resolve this UID from the slug; the CDC path uses a helper shared with the backfill runner:

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

- Applied to `project_membership` (`handleAssetUpsertBatch`) and `key_contact` (`processKeyContact`) before publishing.
- Injected via `WithCDCProjectResolver(ProjectResolverImpl(ctx))`.
- On a **transient resolution failure** (`ok == false`) the consumer **skips only the indexer publish** (logged at **ERROR**, `publish_failed_for_backfill_repair=true`) rather than publishing an empty `project_uid` tag/parent-ref. **OpenFGA still publishes** (`BuildProjectMembershipFGAMessagePreserveMissingRefs` excludes missing `project`/`b2b_org` relations; `key_contact` FGA needs only `Username`/`MembershipUID`). Org-dashboard provisioning for key contacts runs only when `project_uid` resolved. Skipped indexer docs are repaired by the next CDC event or `/admin/reindex`.
- An already-populated `ProjectUID` is never re-resolved.

This gives CDC-published `project_membership` and `key_contact` docs the same `project_uid` tag and `project:` reference as the API and backfill paths when resolution succeeds (LFXV2-2733).

---

## b2b_org Hierarchy Tuples

For an org with a non-empty `ParentUID`, `handleAccountUpsertBatch` emits the parent hierarchy tuples **unconditionally**:

```407:418:internal/service/cdc_consumer.go
	for _, org := range orgs {
		publishB2BOrgUpsertEvents(ctx, o.b2bOrgReader, o.publisher, oldOrgs[org.UID], org, indexerConstants.ActionUpdated, o.globalOrgAdminTeamUID)
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
```

- `publishB2BOrgUpsertEvents` only emits reparenting messages on a **parent change**; on a cold-cache create it sees `oldOrg == org` and emits nothing. `PublishB2BOrgParentFGA` closes that gap so a freshly created child org still gets its `parent` tuple and its parent's `child`-list tuple.
- The batched child-UID query is extended to include each org's `ParentUID`, so the parent's full child list is available for the child-list message.
- Both are idempotent `update_access` upserts, so a genuine reparent (which also emits the new parent tuple) double-publishing is safe.

See the [FGA Contract](./fga-contract.md#b2b-org) for the exact `parent`/`child` message shapes.

---

## Cross-Cutting Behaviour

### Quota guard

Before each upsert re-fetch, `quotaExceeded` checks the Salesforce REST API usage gauge. When usage ≥ `CDC_QUOTA_SKIP_THRESHOLD` (default `0.95`), the batch is **skipped** (logged with `publish_failed_for_backfill_repair=true`) to preserve remaining quota for user-facing reads. Fail-open: disabled when the gauge is nil or the limit has not yet been observed. Set the threshold to `1` to disable, `0` to always skip.

### Absent-from-SOQL → delete convergence

An upsert ID missing from the SOQL result means the record was soft-deleted or no longer qualifies (e.g. a `Product2.Family` flipped off "Membership"). `handleAbsentAsDelete` routes it to the delete handler so the index/FGA state converges. Present-but-unconvertible IDs are excluded from the "absent" check so a transient conversion error never triggers a spurious delete.

### SFID normalisation

Every raw record ID is normalised to its canonical 18-char SFID before use, matching the keys returned by SOQL and stored in the cache. Un-normalisable IDs are skipped (never drive a spurious delete or cache miss).

### Cache invalidation

Each upsert/delete evicts the record's sObject cache entry in the `member-service-cache` bucket (`CacheInvalidator.Invalidate*`, implemented by `salesforce.SObjectClient`). This keeps sObject-cache-backed readers — the HTTP API and the b2b_org reparenting `GetB2BOrg` lookup — from serving a stale copy. The CDC batch re-fetch itself is uncached SOQL, so it does not depend on this eviction. `InvalidateB2BOrg` evicts all four B2BOrg key variants (legacy, full, flat, parent-brief) for the UID.

---

## Failure Modes & Log Signals

The consumer is resilient by design — a single bad event never halts the stream. Handler errors are logged and the cursor still advances; `/admin/reindex` is the backstop for missed records.

| Log key | Meaning | Recovery |
|---|---|---|
| `publish_failed_for_backfill_repair=true` | A publish/fetch failed (or was quota-skipped); the record may be missing/stale downstream. | `POST /admin/reindex` for the affected type/record. |
| `cdc: skipping … indexer publish; project_uid unresolved` | Resolver could not map slug→UID; indexer skipped, OpenFGA still published (ERROR). | Next CDC event or `/admin/reindex` to repair the indexer doc. |
| `fga_revoke_failed_dangling_tuple=true` | A `key_contact` DELETE could not revoke its FGA tuple — a dangling grant. | **Not** repairable via reindex; needs a targeted FGA sync / manual re-send of the remove message. |
| `cdc: GAP event received` | Salesforce could not deliver granular events. | Handled as upsert; cross-check `/admin/reindex` if needed. |
| `cdc: skipping record with non-normalizable SFID` | A malformed record ID was dropped. | Investigate the source event; reindex if the record is legitimate. |
| `cdc: <reader> not wired` | A batch reader dependency is missing (misconfiguration). | Fix wiring; `/admin/reindex` to repair the gap. |

---

## Kubernetes Deployment

The consumer runs as a **separate Deployment** (`charts/lfx-v2-member-service/templates/deployment-consumer.yaml`, gated by `consumer.enabled`):

- **`replicas: 1` + `strategy: Recreate`** — guarantees at most one active consumer at any time. The replay cursor is a single unsharded value; a second consumer would double-process. No application-level lease is used.
- **Liveness probe** hits `GET /livez`, which **always returns 200 while the process is alive** (`cmd/member-api/main.go`) — it does not inspect stream state, so a hung gRPC stream does not trip it directly. Instead, `Run`'s goroutine `defer cancel()` makes an unrecoverable stream failure (or an exited `Run` loop) unblock the shutdown path and exit the process; Kubernetes then restarts the pod. `/livez` deliberately avoids returning 503 on context-cancel so the probe cannot restart the pod before the final replay cursor is committed.
- **Graceful shutdown** — SIGTERM cancels the context; the consumer commits its final cursor within the graceful window before exiting.

---

## Configuration

Consumer-mode environment variables (read only when `RUN_MODE=consumer`):

| Variable | Description | Default | Required |
|---|---|---|---|
| `SF_PUBSUB_ENDPOINT` | Salesforce Pub/Sub gRPC endpoint (e.g. `api.pubsub.salesforce.com:7443`) | — (fatal if empty) | Yes |
| `SF_ORG_ID` | 18-char Salesforce Org ID, sent as the `tenantid` gRPC metadata header | — (fatal if empty) | Yes |
| `SF_CDC_CHANNEL` | CDC channel/topic to subscribe to | `/data/ChangeEvents` | No |
| `CDC_QUOTA_SKIP_THRESHOLD` | Fraction of daily Salesforce REST quota (0–1) at which upsert re-fetches are skipped | `0.95` | No |
| `GLOBAL_ORG_ADMIN_TEAM_UID` | v2 UID of the platform org-admin team | `_null` | No |

Salesforce credentials (`SF_INSTANCE_URL`, `SF_CLIENT_ID`, `SF_CLIENT_SECRET`, `SF_USERNAME`, `SF_PASSWORD`, `SF_SECURITY_TOKEN`, `SF_CONSUMER_RSA_PEM`) and NATS settings are shared with API mode — see the main [README](../README.md) / [CLAUDE.md](../CLAUDE.md).

> **Channel scope:** `/data/ChangeEvents` subscribes to all CDC-enabled objects; the consumer ignores entities other than `Account`, `Asset`, and `Project_Role__c`. A narrower channel (e.g. `/data/AccountChangeEvent`) can be set via `SF_CDC_CHANNEL`.

---

## NATS Storage

The CDC consumer touches four NATS KV buckets:

| Bucket | Use by the consumer | TTL |
|---|---|---|
| `pubsub-state` | Stores the replay cursor per channel (`pubsub-replay.<channel>`). Authoritative. | none |
| `member-service-cache` | sObject cache — the consumer **evicts** entries here on each event (never writes). | no soft-TTL |
| `membership-cache` | Read/written by the injected `ProjectResolver` while resolving `project_uid` for `Asset` / `Project_Role__c` upserts — it looks up and populates `project-uid.<slug>` via `Storage.GetProjectUID` / `PutProjectUID`. | 6 h stale / 23 h expire |
| `org-settings` | Read/written when a **registered** key contact upsert succeeds `project_uid` resolution: `processKeyContact` calls `AddPrincipal` (`SuppressNotification: true`), which reads and updates the authoritative org-settings KV. Not touched on resolver failure. | none (authoritative) |

> The batch record re-fetch path itself is uncached SOQL; the `membership-cache` access is the `ProjectResolver`'s slug→UID cache, and `org-settings` is touched only when `project_uid` resolution succeeds and the key contact has a known LFID (org-dashboard provisioning).

---

## Observability

Each event is processed inside an OpenTelemetry span `salesforce.cdc.process` (`SpanKindConsumer`) with attributes: `messaging.system=salesforce`, `messaging.destination.name=<channel>`, `cdc.entity`, `cdc.change_type`, `cdc.record_count`. Handler errors are recorded on the span. Each batch logs an slog line `cdc: <entity> batch published` with `upsert_count` / `absent_delete_count`.
