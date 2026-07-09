<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Salesforce-backed cache and ProjectResolver

This service has no PostgreSQL and no sync job. Membership reads and the
reindex backfill fetch from Salesforce SOQL and cache results in NATS
JetStream KV (`membership-cache`). b2b_org and single-object reads use the
sObject conditional-GET cache (`member-service-cache`), and authoritative
b2b_org access-control state lives in `org-settings`.

## NATS KV cache

### `membership-cache` bucket

All records share the `membership-cache` bucket. Keys are namespaced by a type
prefix to avoid collisions.

| Key pattern | Contents | Soft TTL |
| --- | --- | --- |
| `tier.{uid}` | `CachedValue[*model.MembershipTier]` | 6 h stale / 23 h expire |
| `membership.{uid}` | `CachedValue[*model.ProjectMembership]` | 6 h stale / 23 h expire |
| `key-contacts.{membership_uid}` | `CachedValue[[]*model.KeyContact]` | 6 h stale / 23 h expire |
| `project-sfid.{project_uid}` | `CachedValue[string]` (Salesforce Project__c.Id) | 6 h stale / 23 h expire |
| `project-uid.{slug}` | `CachedValue[string]` (v2 project UUID) | 6 h stale / 23 h expire |
| `soql.memberships-by-project.{params...}.{batch}` | `MembershipBatchCacheEntry` for paged SOQL membership results | 6 h stale / 23 h expire |

Prefixes are defined as the dot-delimited `keyPrefix*` constants in
`internal/infrastructure/nats/storage.go`. The NATS bucket itself has a
24-hour `MaxAge` (hard eviction), which is always later than the soft
`expires_at` timestamp inside each envelope.

### `member-service-cache` bucket

This bucket stores raw Salesforce sObject REST API responses with HTTP
conditional request metadata. It is initialized by the NATS client and chart,
and is used by `SObjectClient`. The live `B2BOrgReader` is built on
`SObjectClient` (so `GET /b2b_orgs/{uid}` reads through this cache); the
membership read path still uses the SOQL-backed `MemberReader`. It does
not use `CachedValue` soft TTLs; freshness is governed by Salesforce `ETag`
and `Last-Modified` revalidation. The bucket TTL is a 7-day backstop for quiet
records, and the client rewrites unchanged entries after `304 Not Modified` to
reset that TTL. The Salesforce Pub/Sub CDC consumer invalidates entries here
(`port.CacheInvalidator`) when a change event arrives, before re-fetching and
re-publishing.

| Key pattern | Contents |
| --- | --- |
| `b2b_org_v2.{uid}` | Salesforce Account sObject cache entry (full `FetchB2BOrg` field list) |
| `b2b_org_flat.{uid}` | Salesforce Account sObject cache entry (narrow `FetchAccount` field list) |
| `b2b_org_parent_brief.{uid}` | Salesforce Account sObject cache entry (3-field parent-detail lookup) |
| `b2b_org.{uid}` | Legacy pre-LFXV2-2654 key; evicted only, never read after the versioned `b2b_org_v2` rollout |
| `project_membership.{uid}` | Salesforce Asset sObject cache entry |
| `key_contact.{uid}` | Salesforce Project_Role__c sObject cache entry |
| `membership_tier.{uid}` | Salesforce Product2 sObject cache entry |

### `org-settings` bucket

This bucket holds authoritative b2b_org access-control state (writers,
auditors, pending invites) — it is not a cache. Keys are `org-settings.{uid}`
→ raw `model.B2BOrgSettings` JSON (no `CachedValue` envelope, no soft TTL).
There is no MaxAge eviction; every PUT uses the KV revision for optimistic
concurrency (compare-and-set), so a concurrent modification returns `409
Conflict`. Read and write helpers live in
`internal/infrastructure/nats/b2b_org_settings.go`.

### `pubsub-state` bucket

Holds Salesforce Pub/Sub CDC consumer state — not a cache. Per-channel replay
cursors are keyed `pubsub-replay.<channel>` (see
`internal/infrastructure/salesforce/pubsub/pubsub_replay.go`). No MaxAge: a
quiet channel must never lose its cursor to eviction, which would force a
silent fallback to LATEST and a gap in delivered events.

### Cache freshness states

Defined in `internal/infrastructure/nats/cache.go`:

| Status | Meaning | Caller behaviour |
| --- | --- | --- |
| `CacheStatusFresh` | Within stale threshold | Serve immediately. |
| `CacheStatusStale` | Past stale threshold, not yet expired | Serve immediately; trigger background refresh goroutine. |
| `CacheStatusExpired` | Past expiry threshold | Do not serve; fetch synchronously from Salesforce. |
| `CacheStatusMiss` | Key not present in bucket | Fetch synchronously from Salesforce. |

## ProjectResolver

`internal/infrastructure/project/resolver.go` implements `port.ProjectResolver`.
It is the bridge between the v2 project UUID world and the Salesforce
`Project__c.Id` world.

### Why it exists

Every project-scoped SOQL query (membership reads and the reindex backfill)
requires a Salesforce `Project__c.Id` in its `WHERE` clause. Callers carry v2
UUIDs. Without `ProjectResolver`, such a query would bind a UUID Salesforce
does not store and silently return zero rows.

### Resolution chain: `SFIDFromUID`

```text
SFIDFromUID(ctx, projectUID)
    |
    +-- 1. KV cache lookup: project-sfid.{uid}
    |        Fresh/Stale -> return cached SFID
    |
    +-- 2. NATS RPC -> project-service (lfx.projects-api.get_slug)
    |        returns slug string
    |
    +-- 3. KV cache write: project-uid.{slug} -> uid  (side-effect)
    |
    +-- 4. SOQL query -> Salesforce
    |        SELECT Id FROM Project__c WHERE Slug__c = '<slug>'
    |
    +-- 5. KV cache write: project-sfid.{uid} -> sfid
            return sfid
```

### Resolution chain: `UIDFromSlug`

```text
UIDFromSlug(ctx, slug)
    |
    +-- 1. KV cache lookup: project-uid.{slug}
    |        Fresh/Stale -> return cached UID
    |
    +-- 2. NATS RPC -> project-service (lfx.projects-api.slug_to_uid)
    |        returns uid string
    |
    +-- 3. KV cache write: project-uid.{slug} -> uid
            return uid
```

### Registration

`NewProjectResolver` in `internal/infrastructure/project/resolver.go` wires
together `*nats.ProjectRPC`, `*salesforce.ProjectRepo`, and `*nats.Storage`.
The resolver is constructed in `cmd/member-api/service/providers.go` and passed
to `salesforce.NewMemberReader`.
