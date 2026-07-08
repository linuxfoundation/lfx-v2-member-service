<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# NATS Messaging (member-service)

Repo-local NATS subjects, RPC payload shapes, and KV bucket inventory.
General NATS/KV coding conventions live inline in the parent SKILL.md.
Platform NATS/KV ownership and handoffs live in
`lfx-skills:lfx-platform-architecture`.

## Inbound RPC handled by this service

Subjects are defined in `pkg/constants/nats.go`. Each is a plain `Subscribe`
request/reply handler, drained on shutdown. (The earlier
`lfx.member.sfid-to-uuid.lookup` / `lfx.member.uuid-to-sfid.lookup` subjects
and their handler were removed in LFXV2-2049 — the canonical uid is now the
18-char SFID itself.)

```go
ProjectIDMapLookupSubject = "lfx.member.project-id-map.lookup"
B2BOrgLookupSubject       = "lfx.member.b2b_org_lookup"
```

### Project ID map lookup

Maps a v2 project UID to a Salesforce `Project__c.Id` for caller services
that need to correlate by SFID. Implemented in
`internal/infrastructure/nats/project_id_map_handler.go`. Resolution chains
KV cache → project-service NATS RPC → Salesforce SOQL.

| Field | Value |
| --- | --- |
| Subject | `lfx.member.project-id-map.lookup` |
| Transport | NATS core request/reply |
| Subscription | Plain `Subscribe`; drained on shutdown |

Request body (JSON):

```json
{"project_uid": "<v2 project UUID>"}
```

Success response:

```json
{"project_sfid": "<Salesforce Project__c.Id>"}
```

Error response:

```json
{"error": "<human-readable message>"}
```

All replies are always valid JSON. Callers must check for the `"error"` key
to detect failure.

### B2B org lookup

Validates that an organization id resolves to an indexed `b2b_org` and returns
the canonical 18-char Account SFID. Implemented in
`internal/infrastructure/nats/b2b_org_lookup_handler.go`. Resolution chains
sObject cache → Salesforce Account fetch via `GetB2BOrg`.

| Field | Value |
| --- | --- |
| Subject | `lfx.member.b2b_org_lookup` |
| Transport | NATS core request/reply |
| Subscription | Plain `Subscribe`; drained on shutdown |

Request body (JSON):

```json
{"id": "<b2b_org uid or 15/18-char Account SFID>"}
```

Success response:

```json
{"id": "<canonical 18-char Account SFID>"}
```

Error response (not found):

```json
{"error": "b2b org not found"}
```

Other errors use the same `{"error": "..."}` shape (e.g. `id is required`,
`b2b org lookup failed`). Callers must treat non-`b2b org not found` errors as
infrastructure failures, not as a simple miss.

Since LFXV2-2049, `b2b_org.uid` is the 18-char Account SFID — this RPC
confirms existence and normalizes SFID form; it does not translate CDP UUIDs.

## Outbound RPC this service makes

```go
projectGetSlugSubject   = "lfx.projects-api.get_slug"
projectSlugToUIDSubject = "lfx.projects-api.slug_to_uid"
```

Both are owned by `lfx-v2-project-service`. The member service consumes
them only inside `ProjectResolver` to translate UID, slug, and SFID.

The per-principal settings flows also resolve an email to an OIDC sub via
the auth-service request/reply subject
(`AuthEmailToSubLookupSubject = "lfx.auth-service.email_to_sub"` in
`pkg/constants/subjects.go`, called from
`internal/infrastructure/nats/messaging_request.go`).

## KV buckets

The buckets are initialized by `internal/infrastructure/nats/client.go`
and created by the chart when enabled. Names live in
`pkg/constants/storage.go`.

| Bucket | Constant | Purpose |
| --- | --- | --- |
| `membership-cache` | `KVBucketNameCache` | SOQL-backed soft-TTL cache for tiers, memberships, key contacts, membership list batches, and resolver outputs. Uses `CachedValue[T]`, `CacheStatus`, and `TTLConfig`. 24 h bucket MaxAge. |
| `member-service-cache` | `KVBucketNameSObjectCache` | Salesforce sObject conditional-GET cache keyed by `{sobject_type}.{uid}`. Uses `SObjectCacheEntry`, not `CachedValue[T]`. Freshness via HTTP `ETag`/`Last-Modified`; 7-day bucket MaxAge backstop. |
| `org-settings` | `KVBucketNameOrgSettings` | Authoritative b2b_org access-control state (writers, auditors, pending invites), keyed `org-settings.{uid}` → raw `model.B2BOrgSettings` JSON. No soft-TTL envelopes and no MaxAge eviction; optimistic concurrency via KV revision (compare-and-set) on every PUT. |
| `pubsub-state` | `KVBucketNamePubSubState` | Salesforce Pub/Sub CDC consumer state: per-channel replay cursors keyed `pubsub-replay.<channel>`. No MaxAge — a quiet channel must never lose its cursor (that would force a silent fallback to LATEST and an event gap). |

Key layout, TTLs, freshness states, and resolver chains are documented in
`docs/agent-guidance/salesforce-cache.md`.

## Indexer and FGA-sync publishing

This service publishes to the indexer and FGA-sync on the write path. Subjects
are in `pkg/constants/subjects.go`:

| Direction | Subjects |
| --- | --- |
| Indexer | `lfx.index.b2b_org`, `lfx.index.b2b_org_settings`, `lfx.index.project_membership`, `lfx.index.key_contact` |
| FGA-sync | `lfx.fga-sync.update_access`, `lfx.fga-sync.delete_access`; key-contact relation grants/revokes use `lfx.fga-sync.member_put` / `lfx.fga-sync.member_remove` (constants imported from the `lfx-v2-fga-sync` package) |

Publishing goes through `port.MemberPublisher`
(`internal/infrastructure/nats/publisher.go`). Creates/updates are
fire-and-forget and swallow publish errors (logged with
`publish_failed_for_backfill_repair=true`, recoverable via
`POST /admin/reindex`); deletes propagate publish errors. On a settings PUT,
FGA-sync is published before the indexer. The same publish helpers
(`internal/service/messaging.go`) are reused by the Salesforce Pub/Sub CDC
consumer and the backfill runner. Message shapes are owned by the
local contracts `docs/fga-contract.md` and `docs/indexer-contract.md`
(upstream: `lfx-v2-fga-sync` and `lfx-v2-indexer-service`); keep them in sync
with builder changes.
