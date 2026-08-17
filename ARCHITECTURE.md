<!--
Copyright The Linux Foundation and each contributor to LFX.
SPDX-License-Identifier: CC-BY-4.0
-->

# V2 Member Service Architecture

This document describes the current architecture of the LFX v2 Member Service and the proposed
implementation plan for graduating it to full v2 platform idioms: OpenFGA fine-grained
authorization, OpenSearch indexing via the Indexer Service, and a clean v2 API surface.

## Current State

The v2 member service is currently a **read/write Salesforce B2B proxy** with a NATS KV caching
layer that **also publishes indexer and FGA-sync messages on the write path**. The PostgreSQL
replica dependency from the v1 platform has been fully removed. The service now:

1. Queries Salesforce via SOQL (the `salesforce` infrastructure package) for membership reads and
   the reindex backfill, and via the sObject REST API (conditional GET) for `b2b_org` and other
   single-object reads.
2. Caches SOQL responses in `membership-cache` (stale-while-revalidate) and sObject responses in
   `member-service-cache` (HTTP conditional-GET), to reduce Salesforce round-trips.
3. Exposes single-object reads, write endpoints for `b2b_org` (create/update) and key contacts
   (create/update/delete), full-replace b2b_org access-control settings, and per-principal
   settings-user endpoints (`/b2b_orgs/{uid}/settings/users[/{email}]`) — with `If-Match`
   optimistic concurrency on mutations.
4. Publishes indexer (`lfx.index.*`) and FGA-sync (`lfx.fga-sync.*`) messages on writes, and
   exposes `POST /admin/reindex` to backfill those downstream indexes from Salesforce.
5. Stores authoritative b2b_org access-control state (writers, auditors, pending invites) in the
   `org-settings` KV bucket.
6. Handles inbound NATS RPC to resolve a v2 project UID to its Salesforce `Project__c.Id`
   (`lfx.member.project-id-map.lookup`) and to validate a `b2b_org` id
   (`lfx.member.b2b_org_lookup`). The earlier SFID ↔ UUID translation RPCs were removed
   in LFXV2-2049: the canonical `uid` for Salesforce-backed entities is now the 18-char SFID
   itself (`pkg/sfuuid` only normalizes 15↔18-char forms).
7. Resolves project UIDs ↔ slugs via NATS RPC calls to the project-service.
8. Runs a Salesforce Pub/Sub CDC consumer (`RUN_MODE=consumer`, single replica) that
   invalidates the sObject cache and re-publishes indexer + FGA-sync messages on
   `Account`/`Asset`/`Project_Role__c` change events, persisting its replay cursor in the
   `pubsub-state` KV bucket.

### Current API layout

The service exposes resource-rooted endpoints. The authoritative surface is the Goa design
(`cmd/member-api/design/membership.go`) and the deployed Heimdall ruleset
(`charts/lfx-v2-member-service/templates/ruleset.yaml`):

| Method | Path | Description | FGA check |
|--------|------|-------------|-----------|
| GET | `/b2b_orgs/{uid}` | Get a b2b_org (sObject cache) | `auditor` on `b2b_org:{uid}` |
| POST | `/b2b_orgs` | Create a b2b_org (machine callers) | `member` on `team:{globalOrgAdminTeamUID}` |
| PUT | `/b2b_orgs/{uid}` | Update a b2b_org | `writer` on `b2b_org:{uid}` |
| GET | `/b2b_orgs/{uid}/settings` | Get org access-control settings | `auditor` on `b2b_org:{uid}` |
| PUT | `/b2b_orgs/{uid}/settings` | Full-replace org writers/auditors | `writer` on `b2b_org:{uid}` |
| POST | `/b2b_orgs/{uid}/settings/users` | Add a single settings user (per-principal) | `writer` on `b2b_org:{uid}` |
| PUT | `/b2b_orgs/{uid}/settings/users/{email}` | Change a settings user's role | `writer` on `b2b_org:{uid}` |
| DELETE | `/b2b_orgs/{uid}/settings/users/{email}` | Remove a settings user | `writer` on `b2b_org:{uid}` |
| GET | `/project_memberships/{uid}` | Get a membership | `auditor` on `project_membership:{uid}` |
| GET | `/project_memberships/{m_uid}/key_contacts/{uid}` | Get a key contact | `auditor` on `project_membership:{m_uid}` |
| POST | `/project_memberships/{m_uid}/key_contacts` | Create a key contact | `writer` on `project_membership:{m_uid}` |
| PUT | `/project_memberships/{m_uid}/key_contacts/{uid}` | Update a key contact | `writer` on `project_membership:{m_uid}` |
| DELETE | `/project_memberships/{m_uid}/key_contacts/{uid}` | Delete a key contact | `writer` on `project_membership:{m_uid}` |
| POST | `/admin/reindex` | Trigger an indexer/FGA backfill | `member` on `team:{globalOrgAdminTeamUID}` |

Authorization is checked by the Heimdall API gateway using the `b2b_org` and `project_membership`
OpenFGA types per object. The earlier project-scoped drill-down paths and the interim `lfProjectUID`
detour have been removed. Note that key contacts are still nested under their parent membership path
(`/project_memberships/{m_uid}/key_contacts/...`) rather than the root `/key_contacts/{uid}` paths
described in the Target Architecture below — the root key-contact surface is not yet implemented.

### Current NATS KV bucket layout

NATS KV (bucket `membership-cache`) serves as a stale-while-revalidate cache in front of
Salesforce. There are no lookup-index keys; collection results are stored as paginated SOQL
result batches. All key types below live in the single `membership-cache` bucket, namespaced
by type prefix.

| Key pattern | Value | TTL |
|-------------|-------|-----|
| `tier.{tier_uid}` | `CachedValue[*model.MembershipTier]` | 6 h stale / 23 h expire / 24 h bucket TTL |
| `membership.{membership_uid}` | `CachedValue[*model.ProjectMembership]` | 6 h stale / 23 h expire / 24 h bucket TTL |
| `key-contacts.{membership_uid}` | `CachedValue[[]*model.KeyContact]` | 6 h stale / 23 h expire / 24 h bucket TTL |
| `soql.memberships-by-project.{base64(sfid)}.{base64(sort)}[.{base64(tierSFID)}][.{base64(search:term)}].{batch_index_or_iterator}` | `CachedValue[*MembershipBatchCacheEntry]` with compressed `model.ProjectMembership` records + next-batch iterator | 6 h stale / 23 h expire / 24 h bucket TTL |
| `project-sfid.{project_uid}` | `CachedValue[string]` with Salesforce `Project__c.Id` | 6 h stale / 23 h expire / 24 h bucket TTL |
| `project-uid.{slug}` | `CachedValue[string]` with v2 project UID | 6 h stale / 23 h expire / 24 h bucket TTL |

(Prefixes are dot-delimited, per the `keyPrefix*` constants in
`internal/infrastructure/nats/storage.go`.) The membership SOQL path writes results into KV split
across one or more batch entries (written tail-first so every batch's next-iterator is valid before
the head is committed). The earlier `soql.b2b-orgs` batch cache and the B2B org search endpoint it
backed have been removed.

The code and chart also initialize `member-service-cache`, a 7-day NATS KV bucket used by
`SObjectClient` for Salesforce sObject conditional-GET entries (`SObjectCacheEntry`). The
`GET /b2b_orgs/{uid}` read path uses this sObject cache today; membership reads still use the
SOQL-backed readers above.

The chart and client also initialize `org-settings`, an authoritative (no-TTL) NATS KV bucket that
holds b2b_org access-control state (`org-settings.{uid}` → `model.B2BOrgSettings` JSON). Every
settings PUT uses the KV revision for optimistic concurrency (compare-and-set).

A fourth bucket, `pubsub-state` (no TTL), holds the Salesforce Pub/Sub CDC consumer's replay
cursors (`pubsub-replay.<channel>`); a quiet channel must never lose its cursor to eviction.

### Current domain model (post-detour)

The domain model has already been updated to reflect the target architecture names (LFXV2-1358,
shipped in v0.5.x). Note that these renames are **target-state**, not interim:

```
model/
  B2BOrg            — Salesforce Account (renamed from Member; B2BOrgInput for mutations)
  ProjectMembership — Asset record; now carries B2BOrgUID (derived from AccountSFID)
  KeyContact        — Project_Role__c record (renamed from ProjectKeyContact); carries B2BOrgUID
  KeyContactInput   — Mutable fields for create / update
```

The `MembershipTier` type (Product2) remains in the domain model but will not have a dedicated
API endpoint in the v2 surface — tiers are a Query Service pseudotype only.

---

## Target Architecture

The goal of the next phase is to graduate the member service to full v2 platform idioms:

- **`b2b_org`**: a first-class OpenFGA type representing a Salesforce Account (B2B company), with
  its own `owner` and `writer` relations, replacing the implicit company identity embedded in
  every membership and key contact object.
- **`project_membership`**: promoted to a root-level API type (`/project_memberships/{uid}`) with
  its own indexer pipeline and FGA relations to its parent `b2b_org` and `project`.
- **`key_contact`**: promoted to a root-level API type (`/key_contacts/{uid}`) with FGA relations
  to both its parent `project` and parent `b2b_org`.
- **sObject API + conditional GET caching replaces SOQL-backed KV**: individual object reads use
  the Salesforce sObject REST API with `ETag`/`If-None-Match` and `Last-Modified`/`If-Modified-Since`
  conditional request headers, with responses cached in NATS KV by UID only. SOQL is retained
  exclusively for triggerable backfill/reindex operations.
- **Query Service handles all collection serving**: list and search operations across all three
  types are served entirely by the Query Service (OpenSearch), not by this service. The service
  exposes only single-object GET, write (POST/PUT/DELETE), and a backfill trigger endpoint.
- **Indexer + FGA Sync pipelines**: all three types are published to OpenSearch and OpenFGA on
  every write and on every PubSub CDC event.
- **Salesforce PubSub CDC**: real-time change propagation from Salesforce drives cache
  invalidation and downstream index updates for all externally-initiated changes.

```mermaid
flowchart TD
    %%{init: {"flowchart": {"defaultRenderer": "elk"}} }%%
    UI["LFX Self Service UI"]
    QuerySvc["Query Service"]
    Consumers["Other v2 Consumers"]
    subgraph svc["LFX v2 Member Service"]
        API["Member API (Goa / HTTP)\n/b2b_orgs\n/project_memberships\n/key_contacts"]
        RPC["NATS RPC\n(project ID map)"]
        SObj["sObject Client\n(conditional GET cache in NATS KV)"]
        API --> SObj
        SOQL["SOQL Client\n(backfill only)"]
        PubSub["PubSub Consumer"]
        PUB["Change Event Publisher\nlfx.index.*\nlfx.fga-sync.*"]
    end
    KV["NATS KV\n(sObject conditional GET cache)"]
    Indexer["Indexer Service"]
    FGASync["FGA Sync Service → OpenFGA"]
    OpenSearch["OpenSearch"]
    SF["Salesforce (B2B)"]
    UI --> API
    UI --> QuerySvc
    QuerySvc --> OpenSearch
    PubSub -- "denormalization\nfetches" --> SObj
    SObj -- "set/get cache by UID" --> KV
    PUB --> Indexer
    PUB --> FGASync
    PUB --> Consumers
    Indexer --> OpenSearch
    Indexer -- "notify" --> Consumers
    SOQL --> PUB
    PubSub --> PUB
    API --> PUB
    SObj -- "sObject REST (ETag/Last-Modified)" --> SF
    SOQL -- "SOQL (backfill)" --> SF
    PubSub <-- "PubSub CDC" --> SF
```

### Key design decisions

- **sObject API + conditional GETs for object reads.** Single-object reads use
  `GET /services/data/vXX.0/sobjects/{Type}/{Id}`. Salesforce conditional-GET behaviour differs by
  object type: `Account` serves both `ETag` and `Last-Modified` response headers, so we send
  `If-None-Match` and `If-Modified-Since` on subsequent fetches. All other types (`Asset`,
  `Project_Role__c`, `Product2`) do not serve either header, so we use the `SystemModstamp` field
  (preferred over `LastModifiedDate`, which does not advance on system-initiated changes) as a
  surrogate timestamp and send only `If-Modified-Since`. A `304 Not Modified` response confirms
  the cached value is still valid without re-fetching the body. If 304 responses do not count
  against Salesforce API rate limits, we can revalidate on every object fetch; otherwise we can
  emulate `max-age` semantics (re-use a cached object without revalidation for N seconds).
- **Service-generated ETags for LFX API consumers.** Because Salesforce does not serve ETags on
  most object types, and because `Last-Modified` timestamps can be trivially manufactured by
  clients (offering weak lost-update protection), the service generates its own ETags to send to
  LFX API consumers. Each LFX ETag is a **20-byte truncated, base64url-encoded SHA-256 hash of
  the serialised domain model object** (not the raw Salesforce payload). This guarantees that any
  change visible in our domain model — regardless of the underlying sObject type — produces a
  different ETag, and that consumers must perform a GET before a conditional PUT.
- **NATS KV as sObject cache only.** The lookup-index keys (e.g.
  `lookup/project/{project_id}/{membership_uid}`) are removed entirely. The Query Service
  (OpenSearch) is the authoritative source for filtered collections. NATS KV stores one entry per
  object UID holding the serialised domain object, the Salesforce-side conditional-write token
  (either the SFDC ETag for `Account`, or the `SystemModstamp` string for all other types), and
  the service-generated LFX ETag. A potential `max-age` revalidation timestamp may also be stored.
- **SOQL is retained for backfill only.** Full-table SOQL queries are expensive
  and cannot easily support incremental pagination in a live-serving context.
  They shall be used exclusively by the triggerable reindex job, which may be
  invoked by an operator or by a startup flag.
- **No collection endpoints on this service.** The service does not implement list or search
  endpoints. All collection access is via the Query Service. The service's API surface is:
  single-object GET (served from sObject cache), write mutations, and a backfill trigger.
- **No backward-compatible project-scoped paths.** The old
  `/projects/{uid}/memberships/...` and `/projects/{uid}/memberships/{uid}/key_contacts/...`
  endpoints are removed. Consumers must migrate to the root paths and/or the Query Service.

---

## V2 Entity Model

### OpenFGA types

These types participate in the OpenFGA relationship model and are subject to fine-grained,
per-object access control. They are served from the root of the LFX API path.

#### `b2b_org`

Represents a Salesforce Account (B2B company). The UID is the canonical 18-char Salesforce Account
SFID (normalized via `pkg/sfuuid.Normalize18`).

There is no self-service user permission for creating new `b2b_org` objects. They are created
exclusively by machine users (EasyCLA, LFX Enrollment) whose OAuth client-ID principals are
members of a global `org_admin` team. That team is written as a **direct relation** on every
`b2b_org` object (not via a root-level singleton), providing `writer` access to all org admins
without requiring a hierarchical root object analogous to the `project` root.

```plain
type b2b_org
  relations
    # global_org_admin holds the platform-wide org admin team. Written to every
    # b2b_org at creation time by the member service. There is no "root" b2b_org;
    # the team relation is stamped directly on each object instance.
    define global_org_admin: [team#member]
    define owner: [user]
    define writer: [user] or owner or global_org_admin
    define auditor: [user, team#member] or writer
```

- **`global_org_admin`**: holds the platform-wide org-admin team. Set at creation; never
  managed per-resource. Provides `writer` (and transitively `auditor`) access to all org admins.
  Intentionally scoped to `writer`, not `owner`, so machine users do not acquire any future
  owner-only gates (e.g. org transfer or deletion).
- **`owner`**: individual human users who are owners of this org. Owners inherit `writer` access
  and are reserved for future owner-only operations.
- **`writer`**: individual users, owners, or global org-admin team members who can write
  memberships and key contacts on behalf of this org.
- **`auditor`**: individual users or team members with read access to all data associated with
  this org, including sensitive membership and contact details.

**FGA Sync payload (on create/update):**

```json
{
  "object_type": "b2b_org",
  "operation": "update_access",
  "data": {
    "uid": "<b2b_org_uid>",
    "relations": {
      "owner": ["<owner_username>"]
    },
    "references": {
      "global_org_admin": ["team:<global_org_admin_team_uid>"]
    }
  }
}
```

**Indexer payload:**

```json
{
  "action": "created",
  "headers": { "authorization": "Bearer <token>" },
  "data": {
    "uid": "{{ uid }}",
    "sfid": "{{ sfid }}",
    "name": "Acme Corp",
    "domain": "acme.com",
    "logo_url": "https://..."
  },
  "tags": [],
  "indexing_config": {
    "object_id": "{{ uid }}",
    "public": false,
    "access_check_object": "b2b_org:{{ uid }}",
    "access_check_relation": "auditor",
    "history_check_object": "b2b_org:{{ uid }}",
    "history_check_relation": "auditor",
    "sort_name": "{{ name }}",
    "name_and_aliases": ["{{ name }}", "{{ domain }}"],
    "fulltext": "{{ name }} {{ domain }}"
  }
}
```

---

#### `project_membership`

Represents a Salesforce Asset record: one active (or expired) membership term for a `b2b_org`
within a `project`. The UID is the canonical 18-char Salesforce Asset SFID (normalized via `pkg/sfuuid.Normalize18`).

Access is derived from the caller's relationship to the parent `b2b_org` (for org-scoped access)
and/or to the parent `project` (for project-scoped read access, e.g. LF staff). This service
exposes no API write path for `project_membership` — all mutations originate in Salesforce (via
the Sales team) and propagate to this service through PubSub CDC or the backfill trigger.

```plain
type project_membership
  relations
    define b2b_org: [b2b_org]
    define project: [project]
    define writer: writer from b2b_org
    define auditor: writer or auditor from b2b_org or auditor from project
    define key_contact: [user]
```

`charts/lfx-platform/files/model.fga` in `lfx-v2-helm` is authoritative; transcriptions here can
drift. Note that `key_contact` currently grants nothing: the only relations that consume it live on
`b2b_org` and traverse `b2b_org.membership`, and this service never writes those tuples.

- **`b2b_org`**: reference to the owning org. Set at creation, never changed.
- **`project`**: reference to the project this membership belongs to. Set at creation, never
  changed.
- **`auditor`**: inherits `auditor` from the `b2b_org` (org auditors, owners, global org admins)
  **or** from the `project` (project auditors, writers, owners).

**FGA Sync payload (on create/update):**

```json
{
  "object_type": "project_membership",
  "operation": "update_access",
  "data": {
    "uid": "<membership_uid>",
    "references": {
      "b2b_org": ["b2b_org:<b2b_org_uid>"],
      "project": ["project:<project_uid>"]
    }
  }
}
```

**Indexer payload:**

```json
{
  "action": "created",
  "headers": { "authorization": "Bearer <token>" },
  "data": {
    "uid": "{{ uid }}",
    "b2b_org_uid": "{{ b2b_org_uid }}",
    "project_uid": "{{ project_uid }}",
    "tier_uid": "{{ tier_uid }}",
    "status": "Active",
    "year": "2025",
    "tier": "Gold",
    "membership_type": "Corporate",
    "start_date": "2025-01-01",
    "end_date": "2025-12-31",
    "company_name": "Acme Corp",
    "company_logo_url": "https://...",
    "company_domain": "acme.com",
    "tier_name": "Gold Corporate Membership"
  },
  "tags": ["status", "tier", "membership_type", "year"],
  "indexing_config": {
    "object_id": "{{ uid }}",
    "public": false,
    "access_check_object": "project_membership:{{ uid }}",
    "access_check_relation": "auditor",
    "history_check_object": "project_membership:{{ uid }}",
    "history_check_relation": "auditor",
    "sort_name": "{{ company_name }}",
    "name_and_aliases": ["{{ company_name }}", "{{ company_domain }}"],
    "parent_refs": ["b2b_org:{{ b2b_org_uid }}", "project:{{ project_uid }}"],
    "fulltext": "{{ company_name }} {{ tier_name }} {{ status }} {{ year }}"
  }
}
```

---

#### `key_contact`

Represents a Salesforce `Project_Role__c` record: a named contact role assigned to a `b2b_org`
for a specific `project` membership. The UID is the canonical 18-char Salesforce `Project_Role__c`
SFID (normalized via `pkg/sfuuid.Normalize18`).

Write access is granted to `b2b_org` writers (owners and the global org-admin team) and to
project-level writers (LF staff managing contacts on behalf of members).

```plain
type key_contact
  relations
    define b2b_org: [b2b_org]
    define project: [project]
    define writer: writer from b2b_org or writer from project
    define auditor: auditor from b2b_org or auditor from project
```

- **`b2b_org`**: reference to the owning org. Set at creation from the membership's `b2b_org_uid`.
- **`project`**: reference to the project. Set at creation from the membership's `project_uid`.
  Allows project auditors (staff) to read all key contacts for a project without being granted
  per-org access.
- **`writer`**: inherits `writer` from the `b2b_org` (org owners, global org admins) **or** from
  the `project` (LF staff). This differs from `project_membership`, where only org-side writers
  can modify records.
- **`auditor`**: inherits from `b2b_org` **or** `project` (same pattern as `project_membership`).

**FGA Sync payload (on create/update):**

```json
{
  "object_type": "key_contact",
  "operation": "update_access",
  "data": {
    "uid": "<key_contact_uid>",
    "references": {
      "b2b_org": ["b2b_org:<b2b_org_uid>"],
      "project": ["project:<project_uid>"]
    }
  }
}
```

**Indexer payload:**

```json
{
  "action": "created",
  "headers": { "authorization": "Bearer <token>" },
  "data": {
    "uid": "{{ uid }}",
    "b2b_org_uid": "{{ b2b_org_uid }}",
    "project_uid": "{{ project_uid }}",
    "membership_uid": "{{ membership_uid }}",
    "role": "Voting Representative",
    "status": "Active",
    "board_member": false,
    "primary_contact": false,
    "first_name": "Jane",
    "last_name": "Doe",
    "title": "CTO",
    "email": "jane.doe@acme.com",
    "company_name": "Acme Corp",
    "company_logo_url": "https://...",
    "company_domain": "acme.com"
  },
  "tags": ["role", "status", "board_member", "primary_contact"],
  "indexing_config": {
    "object_id": "{{ uid }}",
    "public": false,
    "access_check_object": "key_contact:{{ uid }}",
    "access_check_relation": "auditor",
    "history_check_object": "key_contact:{{ uid }}",
    "history_check_relation": "auditor",
    "sort_name": "{{ last_name }} {{ first_name }}",
    "name_and_aliases": ["{{ first_name }} {{ last_name }}", "{{ email }}"],
    "parent_refs": [
      "b2b_org:{{ b2b_org_uid }}",
      "project:{{ project_uid }}",
      "project_membership:{{ membership_uid }}"
    ],
    "contacts": [
      {
        "lfx_principal": "{{ uid }}",
        "name": "{{ first_name }} {{ last_name }}",
        "emails": ["{{ email }}"]
      }
    ],
    "fulltext": "{{ first_name }} {{ last_name }} {{ email }} {{ role }} {{ company_name }}"
  }
}
```

---

### Pseudotypes (Indexer / Query Service only)

These types are indexed into OpenSearch and queryable via the Query Service. They do not appear in
the OpenFGA model and carry no per-object permission tuples; access is mediated by the OpenFGA
relations on the root types above.

| Pseudotype | Description | API path | Permission anchor |
|------------|-------------|----------|-------------------|
| `membership_tier` | A Salesforce Product2 record (membership product offered under a project). | N/A (Query Service) | `auditor` on `project:{project_uid}` |

`membership_tier` does not require its own FGA type: different tiers on a project do not have
different admins. Tier access is always derived from the parent project relation.

---

### Entity attribute sets

Following the platform's [entity design guidance](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/docs/entity-design.md),
the entity model uses a single attribute set per root type. All attributes are served from the
single root endpoint and guarded by the `auditor` relation for reads and `writer` for mutations.

If future business requirements introduce attributes that need finer-grained permission boundaries
(e.g., formation details on a `b2b_org` writable only by `owner`), a new endpoint and attribute
set should be introduced following the platform's split-attribute pattern.

---

## Target API Layout

The service exposes only **single-object reads, write mutations, and a backfill trigger**. All
list and search operations are delegated to the Query Service (OpenSearch). There are no
backward-compatible aliases for the old project-scoped paths.

### `b2b_org`

| Method | Path | Description | FGA check |
|--------|------|-------------|-----------|
| GET | `/b2b_orgs/{uid}` | Get a b2b org (sObject cache) | `auditor` on `b2b_org:{uid}` |
| POST | `/b2b_orgs` | Create a b2b org (machine users only) | `member` of `global_org_admin` team |
| PUT | `/b2b_orgs/{uid}` | Update a b2b org | `writer` on `b2b_org:{uid}` |

> **Note:** There is no self-service user permission for `POST /b2b_orgs`. The create endpoint
> checks only the caller's membership in the global org-admin team (populated at deploy time with
> the EasyCLA and LFX Enrollment client-ID principals). This is unlike projects, where any user
> with a `writer` relation to a parent project can create a child project.
>
> List/search `b2b_org`: **Query Service** — `auditor` on `b2b_org:{uid}`.

### `project_membership`

| Method | Path | Description | FGA check |
|--------|------|-------------|-----------|
| GET | `/project_memberships/{uid}` | Get a membership (sObject cache) | `auditor` on `project_membership:{uid}` |

> **Note:** Membership lifecycle operations (creation, cancellation, auto-renewal changes, etc.)
> are managed by the LF Sales team directly in Salesforce. This service exposes no write
> endpoints for `project_membership`; all mutations arrive via Salesforce PubSub CDC or the
> backfill trigger.
>
> List/search `project_membership`: **Query Service** — `auditor` on `project_membership:{uid}`,
> filterable by `parent_refs` (e.g. `b2b_org:{uid}` or `project:{uid}`).

### `key_contact`

| Method | Path | Description | FGA check |
|--------|------|-------------|-----------|
| GET | `/key_contacts/{uid}` | Get a key contact (sObject cache) | `auditor` on `key_contact:{uid}` |
| POST | `/key_contacts` | Create a key contact | `writer` on `b2b_org:{b2b_org_uid}` (from payload) |
| PUT | `/key_contacts/{uid}` | Update a key contact | `writer` on `key_contact:{uid}` |
| DELETE | `/key_contacts/{uid}` | Delete a key contact | `writer` on `key_contact:{uid}` |

> List/search `key_contact`: **Query Service** — `auditor` on `key_contact:{uid}`, filterable by
> `parent_refs` (e.g. `b2b_org:{uid}`, `project:{uid}`, or `project_membership:{uid}`).

### Backfill trigger

| Method | Path | Description | FGA check |
|--------|------|-------------|-----------|
| POST | `/admin/reindex` | Trigger an indexer backfill run | `member` of `global_org_admin` team |

See the [Indexer Backfill](#indexer-backfill) section for details.

---

## OpenFGA Model Fragment

> **Superseded — historical proposal.** This block records the model additions originally
> requested by this service. It no longer matches what is deployed: the live model has since
> gained `parent`/`child`/`membership` on `b2b_org`, never adopted a standalone `key_contact`
> type, and defines `project_membership.auditor` as `writer or auditor from b2b_org or auditor
> from project`. Read `charts/lfx-platform/files/model.fga` in `lfx-v2-helm` for the current
> model, and the `project_membership` section above for the accurate transcription.

The following block shows the additions to the platform-wide OpenFGA model
(`lfx-v2-helm/charts/lfx-platform/templates/openfga/model.yaml`) required by this service.
The `project` and `team` types are already defined; only the new types are shown.

```plain
type b2b_org
  relations
    # global_org_admin holds the platform-wide org admin team. This relation is
    # written to every b2b_org at creation time by the member service. There is
    # no "root" b2b_org; the team is added as a direct relation on each object.
    # Placed in writer (not owner) to reserve owner for future owner-only gates.
    define global_org_admin: [team#member]
    define owner: [user]
    define writer: [user] or owner or global_org_admin
    define auditor: [user, team#member] or writer

type project_membership
  relations
    define b2b_org: [b2b_org]
    define project: [project]
    # No writer relation: membership lifecycle is managed in Salesforce by the
    # Sales team. Mutations reach this service only via PubSub CDC or backfill.
    define auditor: auditor from b2b_org or auditor from project

type key_contact
  relations
    define b2b_org: [b2b_org]
    define project: [project]
    define writer: writer from b2b_org or writer from project
    define auditor: auditor from b2b_org or auditor from project
```

The stub `member` type currently in the model should be removed after confirming no existing
tuples reference it (or after a migration sweep).

---

## Salesforce Integration

### sObject API + conditional GET caching (target read path)

This section describes the target read path. Current live HTTP provider wiring
still uses the SOQL-backed readers described in [Current State](#current-state);
the `SObjectClient` and `member-service-cache` support are present in the repo
but are not yet the source of collection responses.

Individual object reads use the Salesforce sObject REST API. The conditional-GET strategy differs
by object type because Salesforce only serves `ETag` and `Last-Modified` response headers on
`Account` GET responses; all other types (`Asset`, `Project_Role__c`, `Product2`) return neither.

| sObject type | Conditional GET headers sent | Cache revalidation token stored |
|---|---|---|
| `Account` (→ `b2b_org`) | `If-None-Match: "<sfdc_etag>"` + `If-Modified-Since: "<last_modified>"` | SFDC ETag string |
| `Asset` (→ `project_membership`) | `If-Modified-Since: "<system_modstamp>"` | `SystemModstamp` string |
| `Project_Role__c` (→ `key_contact`) | `If-Modified-Since: "<system_modstamp>"` | `SystemModstamp` string |
| `Product2` (→ `membership_tier`) | `If-Modified-Since: "<system_modstamp>"` | `SystemModstamp` string |

`SystemModstamp` is preferred over `LastModifiedDate` because it advances on any system-level
change (automated processes, formula recalculations, etc.), whereas `LastModifiedDate` only
advances on direct user or API edits. Using `SystemModstamp` prevents false cache hits from
system-initiated changes.

- A `200 OK` response returns the updated body; the NATS KV entry is refreshed with the new body,
  revalidation token, and freshly computed LFX ETag.
- A `304 Not Modified` response confirms the cached value is still valid; no KV write is needed.

#### NATS KV cache envelope

The cache stores one entry per v2 UID. Each entry is a JSON envelope:

```json
{
  "sfdc_token":  "<sfdc_etag_or_system_modstamp>",
  "lfx_etag":    "<20-byte base64url-encoded SHA-256 of domain model>",
  "data":        { /* serialised domain model object */ }
}
```

- **`sfdc_token`**: the Salesforce-side revalidation token. For `Account` this is the SFDC ETag
  string (e.g. `"946-HLG3U"`); for all other types it is the `SystemModstamp` value (e.g.
  `"2025-04-01T12:34:56.000Z"`). Used on the next conditional GET to Salesforce, and forwarded
  as the appropriate conditional-write header on mutating requests (see below).
- **`lfx_etag`**: the service-generated ETag returned to LFX API consumers on `GET` responses.
  Computed as a 20-byte truncated, base64url-encoded SHA-256 hash of the serialised domain model
  object — not the raw Salesforce payload. Changes whenever the domain model representation
  changes, regardless of which Salesforce fields changed.
- **`data`**: the full serialised domain model object.

| Bucket | Key |
|--------|-----|
| `member-service-cache` | `b2b_org.{uid}` |
| `member-service-cache` | `project_membership.{uid}` |
| `member-service-cache` | `key_contact.{uid}` |
| `member-service-cache` | `membership_tier.{uid}` |

No lookup-index keys are stored. Collection access is entirely the Query Service's concern.

#### Conditional writes (PUT / DELETE)

`PUT` and `DELETE` endpoints require the caller to supply an `If-Match` header containing the
LFX ETag most recently returned by this service on a `GET`. Because Salesforce does not serve
ETags on most object types, the service cannot simply forward the client's `If-Match` header to
Salesforce. Instead it performs an explicit read-compare-write cycle:

```mermaid
sequenceDiagram
    participant C  as LFX API Consumer
    participant S  as Member Service
    participant KV as NATS KV Cache
    participant SF as Salesforce sObject API

    C->>S: PUT /key_contacts/{uid}<br/>If-Match: "<lfx_etag>"

    S->>KV: Get key_contact.{uid}
    KV-->>S: {sfdc_token, lfx_etag, data}

    alt LFX ETag mismatch
        S-->>C: 412 Precondition Failed<br/>(stale read — consumer must re-GET)
    else LFX ETag matches
        S->>SF: GET /sobjects/Project_Role__c/{sfid}<br/>If-Modified-Since: "<sfdc_token>"
        alt 304 Not Modified (SF record unchanged)
            SF-->>S: 304 Not Modified
            S->>SF: PATCH /sobjects/Project_Role__c/{sfid}<br/>If-Unmodified-Since: "<sfdc_token>"
            alt 412 from Salesforce (concurrent external write)
                SF-->>S: 412 Precondition Failed
                S-->>C: 412 Precondition Failed<br/>(concurrent external write detected)
            else Write accepted
                SF-->>S: 200 OK (updated body)
                S->>KV: Put key_contact.{uid}<br/>{new sfdc_token, new lfx_etag, new data}
                S-->>C: 200 OK + ETag: "<new_lfx_etag>"
            end
        else 200 OK (SF record changed since our cache)
            SF-->>S: 200 OK (updated body)
            note over S: Domain model has changed since<br/>consumer last read — their edit<br/>may now be invalid.
            S->>KV: Put key_contact.{uid}<br/>{new sfdc_token, new lfx_etag, new data}
            S-->>C: 412 Precondition Failed<br/>(record changed externally — consumer must re-GET)
        end
    end
```

For `Account` (→ `b2b_org`), the flow is identical except:
- The Salesforce conditional GET sends `If-None-Match: "<sfdc_token>"` instead of `If-Modified-Since`.
- The Salesforce conditional write sends `If-Match: "<sfdc_token>"` instead of `If-Unmodified-Since`.

**Read-your-writes:** after any successful write to Salesforce, the service must immediately
update the corresponding KV entry (with the new `sfdc_token`, freshly computed `lfx_etag`, and
updated domain model body) before returning the HTTP response to the caller. This ensures
subsequent GET requests are coherent without a round-trip to Salesforce.

### Salesforce PubSub CDC (real-time invalidation)

The PubSub consumer subscribes to Salesforce Change Data Capture channels for `Account`,
`Asset`, and `Project_Role__c`. On each event it:

1. Re-fetches the affected sObject via the sObject API unconditionally (without `If-None-Match`
   or `If-Modified-Since`), since the CDC event implies staleness.
2. Refreshes the NATS KV entry with the new body, updated `sfdc_token`, and recomputed
   `lfx_etag`.
3. Publishes Indexer and FGA Sync messages so downstream indexes stay current.

### SOQL (backfill only)

SOQL is retained for the **triggerable reindex** path only. The service does not use SOQL for
live request serving. Each type requires its own query with the joins necessary to produce the
full denormalized payload for that type — the exact field set and join structure is determined
by the v2 entity contract for that type, not the SOQL queries currently in use. For example:

- **`b2b_org`**: a new type with no prior query; selects from `Account` with whatever related
  objects are needed to populate the v2 `B2BOrg` fields.
- **`project_membership`**: selects from `Asset` joined to `Account`, `Product2`, and
  `Project__c` — but the fields projected and the relationship structure must match the v2
  `ProjectMembership` domain model, which may differ from the old PostgreSQL query.
- **`key_contact`**: selects from `Project_Role__c` joined to `Contact`, `Alternate_Email__c`,
  `Asset`, `Account`, and `Project__c` — again shaped to match the v2 `KeyContact` domain
  model exactly.

Each query pages through results via SOQL query locators and re-publishes Indexer messages for
all records of the requested type.

---

## Indexer Backfill

The backfill mechanism re-publishes Indexer and FGA Sync messages for all records of one or more
types, without affecting the live NATS KV cache. It is triggered via the `POST /admin/reindex`
endpoint.

### Request body

```json
{
  "type": "project_membership"
}
```

- **`type`**: required, exactly one of `b2b_org`, `project_membership`, `key_contact`,
  `b2b_org_settings` (there is no all-types shortcut). One of `since` (incremental floor),
  `since`+`until` (a bounded inclusive `[since, until]` window; `until` requires `since`),
  `items` (targeted UIDs of that one type), or `cdc_repair` (drain the CDC quota-repair queue
  for that type) may be layered on top — see [Backfill / Reindex](./docs/backfill-reindex.md)
  for the full request model, batching, and the quota guard.

> **Quota guard.** The full/filtered paths are gated on `ADMIN_REINDEX_QUOTA_THRESHOLD`
> (default `0.80`): a synchronous handler check returns HTTP `503` at/above threshold and a
> mid-run passive check stops the run. **Targeted (`items`) is exempt** (bounded). Targeted
> `project_membership`/`key_contact` batch-fetch in one SOQL query; `b2b_org` stays per-item.
> Large reindexes that trip the guard must be windowed via `since`/`until`.

### Denormalization contract

The v2 denormalized entity payload (the `data` object in every Indexer message) must be
**identical** regardless of how it was assembled:

- **Live path**: multiple individual sObject fetches (one per related object) triggered by a
  write or PubSub CDC event.
- **Backfill path**: a single SOQL query with multi-table joins (e.g. `Project_Role__c` joined
  to `Contact`, `Alternate_Email__c`, `Asset`, `Account`, `Product2`).

Field names, types, null handling, and value transformations must produce byte-for-byte
equivalent JSON for the same underlying Salesforce record on both paths. Divergence here causes
silent inconsistencies between the live index and a post-backfill index that are very difficult
to detect in production.

**This contract must be enforced by tests.** For each type, there should be a test that:

1. Constructs the expected denormalized struct from a fixture set of raw Salesforce field values.
2. Runs the same fixture data through both the sObject assembly path and the SOQL join-row
   mapping path.
3. Asserts that both produce an identical domain object (and therefore an identical Indexer
   payload).

These tests live alongside the respective infrastructure adapters and must be kept in sync
whenever field mappings change on either path.

### Behaviour

1. For each requested type, the service runs the appropriate SOQL query to page through all
   Salesforce records of that type (applying any scope restriction, e.g. active-only).
2. For each record, it constructs the full denormalized domain object via the SOQL join-row
   mapping path (see denormalization contract above) and publishes both an `updated` Indexer
   message and an FGA Sync `update_access` message. Both operations are idempotent; the
   backfill is safe to run multiple times.
3. Progress is logged via structured log lines; the endpoint returns `202 Accepted` immediately
   and runs the backfill asynchronously.

> The backfill does **not** modify the NATS KV sObject cache. It is purely an Indexer/FGA
> re-publication pass. The sObject cache is external (NATS KV persists independently of the
> service) so restarting the service has no effect on cached entries. To force re-validation,
> operators must delete the relevant KV entries manually (or purge the bucket); the next GET
> request for each affected object will then re-fetch from Salesforce and repopulate the cache.

---

## Heimdall RuleSet Changes

The Heimdall RuleSet must be updated to:

1. **Remove all project-scoped rules** for the old drill-down paths.
2. **Add rules for the new root paths** (`/b2b_orgs/...`, `/project_memberships/...`,
   `/key_contacts/...`).
3. **The `POST /b2b_orgs` and `POST /admin/reindex` rules** check team membership against the
   global org-admin team rather than a per-resource object. The team UID is injected as a static
   chart value (`app.globalOrgAdminTeamUID`).
4. **No write rules for `project_memberships`.** The only Heimdall rule needed for this path is
   a `GET` read rule. Membership lifecycle (creation, cancellation, auto-renewal) is handled by
   the LF Sales team in Salesforce; no direct-write HTTP rules are required.

Example rule sketch for `POST /b2b_orgs` (team membership check):

```yaml
- id: "rule:lfx:lfx-v2-member-service:b2b_orgs:create"
  match:
    methods: [POST]
    routes:
      - path: /b2b_orgs
  execute:
    - authenticator: oidc
    - authorizer: openfga_check
      config:
        values:
          relation: member
          object: "team:{{ .Values.app.globalOrgAdminTeamUID }}"
    - finalizer: create_jwt
```

Example rule for `PUT /key_contacts/{uid}` (per-resource object check):

```yaml
- id: "rule:lfx:lfx-v2-member-service:key-contacts:update"
  match:
    methods: [PUT]
    routes:
      - path: /key_contacts/:uid
  execute:
    - authenticator: oidc
    - authorizer: openfga_check
      config:
        values:
          relation: writer
          object: "key_contact:{{- .Request.URL.Captures.uid -}}"
    - finalizer: create_jwt
```

---

## Domain Model Changes

### Rename: `member` → `b2b_org`

The internal domain concept currently called `member` (which already represents a Salesforce
Account / B2B company) should be renamed to `b2b_org` throughout:

- `model.Member` → `model.B2BOrg`
- `model.ProjectMembership` → `model.ProjectMembership` (unchanged — the type name is already
  accurate for the v2 entity)
- `model.ProjectKeyContact` → `model.KeyContact` (drop the `Project` prefix; project scope is
  carried as a field)

New fields required on existing models:

```go
// B2BOrg — the Salesforce Account, new top-level type
type B2BOrg struct {
    UID     string    `json:"uid"`         // canonical 18-char Salesforce Account SFID
    SFID    string    `json:"-"`           // raw Salesforce Account.Id (internal only)
    Name    string    `json:"name"`
    Domain  string    `json:"domain,omitempty"`
    LogoURL string    `json:"logo_url,omitempty"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

// ProjectMembership — add B2BOrgUID (currently only AccountSFID is stored internally)
type ProjectMembership struct {
    // ... existing fields ...
    B2BOrgUID string `json:"b2b_org_uid"` // NEW: exposed in API response and indexer payload
}

// KeyContact — add B2BOrgUID (currently derived but not surfaced on the struct)
type KeyContact struct {
    // ... (renamed from ProjectKeyContact; existing fields unchanged) ...
    B2BOrgUID string `json:"b2b_org_uid"` // NEW: exposed in API response and indexer payload
}
```

`B2BOrgUID` equals the 18-char Account SFID normalized via `sfuuid.Normalize18(accountSFID)` — the SOQL and
sObject queries already fetch the Account SFID, so no additional Salesforce round-trips are
needed.

### New port: `B2BOrgReader`

```go
type B2BOrgReader interface {
    GetB2BOrg(ctx context.Context, uid string) (*model.B2BOrg, error)
}
```

### New port: `B2BOrgWriter`

```go
type B2BOrgWriter interface {
    CreateB2BOrg(ctx context.Context, sfid string) (*model.B2BOrg, error)
    UpdateB2BOrg(ctx context.Context, uid string, input model.B2BOrgInput) (*model.B2BOrg, error)
}
```

### New port: `EventPublisher`

```go
type EventPublisher interface {
    PublishIndexerEvent(ctx context.Context, objectType, action string, data any, cfg *indexertypes.IndexingConfig) error
    PublishFGASyncEvent(ctx context.Context, msg fgasync.GenericFGAMessage) error
}
```

### Ports to remove in the target state

- `MembershipSourceReader` (SOQL-backed) — replaced by `sObjectReader` (ETag/Last-Modified-aware) for live
  serving, and a `BackfillRunner` for reindex operations.
- All NATS KV lookup-index writers — removed; the Query Service is the only collection index.
- No `MembershipWriter` port is introduced — `project_membership` records are written exclusively
  in Salesforce by the LF Sales team. There is no API write path for this type; mutations reach
  this service only via PubSub CDC or the backfill trigger.

---

## Detour Cleanup

The LFXV2-1382 detour (v0.5.x) introduced a mix of target-architecture building blocks and
non-idiomatic interim code. The building blocks are already in place and should be kept; the
interim code must be removed as part of the normal implementation sequence.

### Keep (already target-architecture)

The following additions from LFXV2-1382 align with the target architecture and should be
retained or refined in subsequent tickets:

- **`model.B2BOrg` / `model.B2BOrgInput`** — first-class entity types (LFXV2-1358 scope).
- **`model.ProjectKeyContact` → `model.KeyContact` rename** and **`B2BOrgUID` on
  `ProjectMembership` and `KeyContact`** — correct target-state names.
- **`port.B2BOrgReader.GetB2BOrg`** (single-object read), **`port.B2BOrgWriter`**,
  **`port.EventPublisher`** — target-architecture ports.
- **`convertSOQLToB2BOrg` / `soqlAccount` struct** — needed for the backfill SOQL path
  (LFXV2-1365).
- **`B2BOrgResponse` Goa type**, **`B2BOrgUIDAttribute`**, **`convertB2BOrgToResponse`** —
  correct response types (will be refined in LFXV2-1359).
- **`project_slug` / `b2b_org_uid` on Goa response types**, removal of `membership_type` field.
- **`member-service-cache` NATS KV bucket** — the sObject conditional GET cache bucket
  (LFXV2-1357 scope).
- **sObject client** (`sobject_client.go`, `sobject_cache.go`, `sobject_readers.go`) — the
  sObject REST + conditional GET implementation (LFXV2-1357 scope).

### Remove (non-idiomatic detour, replaced by target architecture)

> **Status: done.** All of the detour items listed below have been removed from the codebase
> (verified against HEAD). The list is retained for historical traceability.

The following additions from LFXV2-1382 did **not** align with the target architecture and have
been removed. Primary owners were LFXV2-1359 (API + handlers) and LFXV2-1366 (Helm chart).

**Goa API design (`cmd/member-api/design/`):**

- `list-b2b-orgs` and `list-b2b-org-memberships` Goa methods — replaced by the Query Service.
- `B2BOrgSearchNameAttribute` helper — used only by the removed methods.

**Service layer (`cmd/member-api/service/`):**

- `ListB2bOrgs` and `ListB2bOrgMemberships` service handlers.

**Domain ports (`internal/domain/port/`):**

- `port.B2BOrgReader.SearchB2BOrgs` — remove; only `GetB2BOrg` is retained.
- `port.MemberReader.ListMembershipsForB2BOrg` — remove; collection access is Query Service only.

**Salesforce infrastructure (`internal/infrastructure/salesforce/`):**

- SOQL search implementation in `salesforce/b2b_org_reader.go` — rewrite to sObject-backed
  `GetB2BOrg` only; remove `SearchB2BOrgs` SOQL path.
- `fetchAndCacheAllBatchesByAccount`, `resolveProjectUIDsForB2BPage` in `member_reader.go` —
  batch pagination helpers for the detour list endpoint; remove.
- `AccountRepo.FetchFirstAccountBatch` and `buildAccountsSOQL` — detour-only SOQL helpers;
  repurpose `soqlAccount` / `convertSOQLToB2BOrg` for backfill only and remove the batch
  fetcher.

**Domain model (`internal/domain/model/`):**

- `B2BOrgFilters`, `B2BOrgPage` in `model/b2b_org.go` — pagination types for the removed
  search endpoint; remove.

**NATS storage (`internal/infrastructure/nats/`):**

- `B2BOrgBatchCacheEntry`, `GetB2BOrgBatch`, `PutB2BOrgBatch` in `storage.go` — SOQL batch
  cache for the detour search endpoint; remove.
- `fetchAndCacheAllBatchesByAccount` cache helpers in `member_reader.go` — remove.

**Helm chart (`charts/lfx-v2-member-service/`):**

- Heimdall rules with static `lfProjectUID` check (detour workaround in `ruleset.yaml`).
- HTTPRoute entries for `/b2b_orgs` (search) and `/b2b_orgs/{uid}/memberships` in
  `httproute.yaml`.
- `openfga.lfProjectUID` value in `values.yaml` (no longer needed after rule removal).

---

## Implementation Plan

### Step 1: OpenFGA model update (lfx-v2-helm) — *In Review*

> **Status:** PR open on lfx-v2-helm (see LFXV2-1356).

- Add `b2b_org`, `project_membership`, and `key_contact` type definitions to
  `charts/lfx-platform/templates/openfga/model.yaml`.
- Bump the model **major** version (type additions/deletions require a major bump per the
  in-file versioning guidelines).
- Remove the stub `member` type (after confirming no existing tuples reference it).

### Step 2: sObject client and conditional GET cache — *Partially Done*

> **Status:** The `SObjectClient`, `sobject_cache.go`, and `sobject_readers.go`
> are already in the codebase, and the `member-service-cache` NATS KV bucket is
> declared in the Helm chart. Current live HTTP provider wiring still uses
> SOQL-backed readers, and key-contact writes still invalidate
> `membership-cache` rather than updating `member-service-cache` envelopes.

- Implement a `sObjectClient` that wraps the Salesforce sObject REST API with conditional GETs:
  `If-None-Match`/`If-Modified-Since` for `Account`; `If-Modified-Since` (derived from
  `SystemModstamp`) for all other types.
- Store `{sfdc_token, lfx_etag, data}` envelopes in a `member-service-cache` NATS KV bucket (one
  key per v2 UID; see cache envelope definition in the Salesforce Integration section).
- Implement `sObjectReader` adapters for `B2BOrg`, `ProjectMembership`, `KeyContact`, and
  `MembershipTier` that call `sObjectClient` and deserialise into the domain model types.
- Done: the conditional-write path (read cache → compare LFX ETag → conditional sObject GET →
  conditional sObject PATCH/DELETE with the best available Salesforce precondition header,
  `If-Match` for `Account` / `If-Unmodified-Since` for others) is implemented for the active
  b2b_org and key-contact mutations, and `If-Match` returns `412` on a stale ETag.
- Remaining: move the remaining live provider wiring (membership reads) off SOQL-backed readers
  where the target single-object API needs sObject cache semantics.

### Step 3: Rename internal types and add `b2b_org_uid` — *Done*

> **Status:** Merged in LFXV2-1358 (PR #25). `model.B2BOrg`, `model.KeyContact` rename,
> `B2BOrgUID` fields, and new ports are already in the codebase.

- Rename `model.Member` → `model.B2BOrg`; update all callsites.
- Rename `model.ProjectKeyContact` → `model.KeyContact`; update all callsites.
- Add `B2BOrgUID` field to `ProjectMembership` and `KeyContact`; populate from `AccountSFID`
  via `sfuuid.Normalize18` in the Salesforce infrastructure layer.

### Step 4: Root API paths (Goa design) — includes detour cleanup — *Mostly Done*

> **Status:** The detour methods and handlers are removed, and resource-rooted `b2b_org`
> (GET/POST/PUT + settings GET/PUT), `project_membership` (GET), and key-contact
> (GET/POST/PUT/DELETE) methods plus `POST /admin/reindex` are in the Goa design and
> implemented. Remaining gap: key contacts are still nested under
> `/project_memberships/{membership_uid}/key_contacts/...` rather than the root
> `/key_contacts/{uid}` paths in the Target API Layout.
>
> See the [Detour Cleanup](#detour-cleanup) section for the full list of items removed as
> part of this step.

- Add `b2b_org` (GET/POST/PUT) and `key_contact` (GET/POST/PUT/DELETE) service methods to the
  Goa design at the root paths described in the Target API Layout section above.
- Add `project_membership` **GET only** — no write methods (lifecycle is managed in Salesforce
  by the Sales team; mutations arrive via PubSub CDC or backfill, not the HTTP API).
- Remove all project-scoped drill-down methods entirely (no aliases, no 410 stubs).
- Remove the detour `list-b2b-orgs` and `list-b2b-org-memberships` Goa methods.
- Regenerate the Goa server code (`make apigen`).
- Remove detour service handlers (`ListB2bOrgs`, `ListB2bOrgMemberships`) and associated
  ports, SOQL infrastructure, and NATS batch cache entries (see Detour Cleanup section).

### Step 5: FGA Sync integration — *Done*

> **Status:** Implemented. The service publishes `lfx.fga-sync.update_access` via
> `port.MemberPublisher` for `b2b_org`, b2b_org settings, and key contacts.
> `lfx.fga-sync.delete_access` is published for `b2b_org` and `project_membership`, on genuine
> Salesforce CDC deletes only (LFXV2-3034). Key contacts are revoked by a targeted
> `member_remove` rather than `delete_access`, because that path knows the single person to
> revoke; b2b_org settings have no FGA type of their own and resolve against the parent org.
> The b2b_org create message includes the `global_org_admin` reference;
> HTTP updates omit it (the CDC consumer always sets it — see `docs/fga-contract.md`). All FGA
> publication is asynchronous — no FGA path uses NATS request/reply, and the publisher API
> exposes no synchronous selector, so success means the local client accepted the message onto
> the connection, not that the broker received it or that OpenFGA converged. Publishes are
> fire-and-forget on the write path (recoverable via `POST /admin/reindex`). On the **API**
> delete path publish errors propagate, and the API key-contact delete also flushes the
> connection, which is the only path that confirms the broker received the revocation. On the
> **CDC** delete path they also propagate — `MemberPublisher`'s delete policy (see
> `internal/domain/port/event_publisher.go`) requires this for every delete, CDC included, and
> `/admin/reindex` cannot repair a dropped `delete_access`: a genuinely deleted record reindexes
> as `outcomeNotFound`, which clears any repair marker without re-emitting `delete_access`.
> `dispatchEntity` (`internal/service/cdc_consumer.go`) already logs a propagated error and
> continues to the next record ID rather than aborting the event, so this carries no
> batch-stranding risk. Indexer publication is unaffected and keeps its own delivery selection.
> A failed `delete_access` publish also writes a durable marker to the CDC repair KV bucket
> under `ReindexTypeB2BOrgDeleteAccess`/`ReindexTypeProjectMembershipDeleteAccess` so an operator
> can find and manually re-purge it — deliberately not an automated retry, since `/admin/reindex`'s
> targeted repair re-fetches and re-upserts the live record, which cannot repair a purge.
> See `docs/fga-contract.md` for the full delivery semantics.

- On every create / update / delete of a `b2b_org` or `key_contact` (via the HTTP API), and on
  every `project_membership` change received via PubSub CDC or backfill, publish a FGA Sync
  message via the `EventPublisher` port using the payloads defined in the entity model section
  above.
- The FGA Sync message for `b2b_org` creation must always include the `global_org_admin`
  reference (team UID loaded from config at startup).
- On delete, publish a `delete_access` message to remove all FGA tuples for the object —
  but only for a **genuine** deletion. The CDC consumer also routes records that are merely
  *absent* from the periodic query to its delete handler for index convergence; that path is
  excluded, because a live org whose membership has lapsed is absent too, and purging it would
  revoke a real customer's administrators. Index convergence is safe on both paths because a
  tombstoned document is rebuilt by `POST /admin/reindex`, whereas a revoked grant is recovered
  only by an operator re-applying the org's settings.
- A `delete_access` on a deleted object does **not** leave it with zero tuples. fga-sync
  declines to delete any tuple whose subject begins with `team:`, so the staff-team reader grant
  from LFXV2-2937 survives. It confers access to an object that no longer resolves, so it is
  inert — but an audit asserting zero remaining tuples will report a correct implementation as
  broken.

### Step 6: Indexer integration — *Done*

> **Status:** Implemented for the HTTP write path, backfill, and PubSub CDC (Step 7).
> The service publishes indexer messages via `port.MemberPublisher`.

- On every create / update / delete and on every PubSub CDC event, publish
  an Indexer message via the publisher port.
- NATS subjects (`pkg/constants/subjects.go`): `lfx.index.b2b_org`,
  `lfx.index.b2b_org_settings`, `lfx.index.project_membership`, `lfx.index.key_contact`.

### Step 7: PubSub CDC consumer — *Done*

> **Status:** Implemented. `internal/service/cdc_consumer.go` consumes normalized CDC events
> from the Salesforce Pub/Sub gRPC adapter (`internal/infrastructure/salesforce/pubsub/`),
> running as a separate single-replica Deployment (`RUN_MODE=consumer`, Recreate strategy).
> Replay cursors persist in the `pubsub-state` KV bucket. The channel defaults to
> `/data/ChangeEvents` and is overridable via `SF_CDC_CHANNEL`.

- Subscribe to `AccountChangeEvent`, `AssetChangeEvent`, and `Project_Role__cChangeEvent` CDC
  channels.
- On each event: invalidate the sObject cache, re-fetch the affected record,
  and publish Indexer + FGA Sync messages (deletes publish a delete indexer event without
  re-fetching).

### Step 8: Indexer backfill endpoint — *Done*

> **Status:** Implemented. `internal/service/backfill_runner.go` runs SOQL paged queries per
> requested type and re-publishes Indexer and FGA Sync messages. `POST /admin/reindex` is gated by
> the global org-admin team check, runs asynchronously, and returns `202 Accepted` with a `run_id`
> for log correlation. The payload requires a single `type` (one of `b2b_org`,
> `project_membership`, `key_contact`, `b2b_org_settings` — no all-types shortcut), and layers on
> `since` (RFC 3339 incremental), `items` (targeted UIDs of that type, max 100), `cdc_repair`
> (drain the CDC quota-repair queue for that type — see [Backfill / Reindex](./docs/backfill-reindex.md)),
> or `dry_run`.

- The `BackfillRunner` runs SOQL paged queries for each requested type and publishes Indexer and
  FGA Sync messages for every record.
- The `POST /admin/reindex` Goa method enforces the global org-admin team membership check.
- It runs as an asynchronous goroutine and returns `202 Accepted` immediately with a run ID for
  log correlation.

### Step 9: Heimdall RuleSet update — includes detour cleanup — *Done*

> **Status:** Implemented. `ruleset.yaml` carries per-object `b2b_org` / `project_membership`
> rules, the team-membership checks for `POST /b2b_orgs` and `POST /admin/reindex`, and no
> longer references `lfProjectUID`. The detour HTTPRoute entries and the `openfga.lfProjectUID`
> value are removed.
>
> See the [Detour Cleanup](#detour-cleanup) section for the Helm chart items removed as part of
> this step.

- Update `charts/lfx-v2-member-service/templates/ruleset.yaml`: remove all project-scoped
  rules and the interim detour rules (static `lfProjectUID` checks), add rules for the new
  root paths and the team-membership-based create/reindex rules.
- Remove detour HTTPRoute entries for `/b2b_orgs` search and `/b2b_orgs/{uid}/memberships`.
- Remove `openfga.lfProjectUID` from `values.yaml`.

---

## Environment Variable Reference

| Variable | Description | Required |
|----------|-------------|----------|
| `SF_INSTANCE_URL` | Salesforce instance URL | Yes |
| `SF_CLIENT_ID` | Salesforce connected app client ID | Yes |
| `SF_CLIENT_SECRET` | Salesforce connected app client secret | Conditional (not required for JWT bearer flow) |
| `SF_API_VERSION` | Salesforce API version (default: `v63.0`) | No |
| `SF_PUBSUB_ENDPOINT` | Salesforce PubSub gRPC endpoint (e.g. `api.pubsub.salesforce.com:7443`) | Consumer mode only |
| `SF_ORG_ID` | Salesforce org ID for the Pub/Sub tenant | Consumer mode only |
| `SF_CDC_CHANNEL` | CDC channel to subscribe to (default `/data/ChangeEvents`) | No |
| `RUN_MODE` | `server` (default, HTTP API) or `consumer` (CDC consumer) | No |
| `GLOBAL_ORG_ADMIN_TEAM_UID` | v2 UID of the global org-admin team; written as `global_org_admin` on every `b2b_org` at creation | Yes |
| `NATS_URL` | NATS server URL | Yes |

---

## Lessons from the V1 Implementation

The v1 `member-management` service has been running this Salesforce integration in production for
several years. The following patterns and failure modes remain relevant.

### Read-your-writes via the sObject cache

After a successful Salesforce write, the service must immediately update the NATS KV sObject
cache entry (with the new ETag, Last-Modified, and body returned by the write API) before returning the HTTP
response to the caller. Because the v2 UID is derived deterministically from the SFDC ID
returned synchronously by the write API (`sfuuid.Normalize18`), no temporary ID indirection is
needed — unlike the v1 `temp_sfdc_id` shadow-row pattern.

### Write-path race conditions and retry logic

**Require `If-Match` on mutating requests.** `PUT` and `DELETE` endpoints must reject requests
that do not carry an `If-Match` header containing the LFX ETag most recently returned by this
service on a `GET`. Because Salesforce does not serve ETags on most sObject types, the service
cannot simply forward the client header to Salesforce. Instead it performs the read-compare-write
cycle described in the [Conditional writes](#conditional-writes-put--delete) section above.

The two-tier design provides strong lost-update protection:
- **LFX ETag check (tier 1):** detects cases where the consumer's view of the domain model is
  stale — even if the relevant Salesforce fields have not changed, if our domain model has a
  different representation the client must re-read first.
- **Salesforce precondition (tier 2):** detects concurrent external writes (e.g. a Salesforce
  admin or another service) that occurred between the service's cache read and its sObject write.
  `If-Match` (SFDC ETag) is used for `Account`; `If-Unmodified-Since` (derived from
  `SystemModstamp`) is used for all other types.

Concurrent duplicate creation attempts (e.g. two simultaneous `POST /key_contacts` for the same
contact) are a separate concern and must be guarded with either a NATS KV compare-and-set (CAS)
lock or an application-level distributed lock keyed on the contact email + membership UID.
Salesforce may also return exclusive-lock contention errors on concurrent writes to related
records; treat these as retryable with exponential backoff and jitter.

### Post-write event failure handling

If the NATS KV write, Indexer publish, or FGA Sync publish fails after a
successful Salesforce write, the service logs the failure with enough context
(object type, UID, SFID, `publish_failed_for_backfill_repair=true`) for the
`POST /admin/reindex` backfill to repair the downstream indexes, and does not
fail the HTTP response: the Salesforce record is durably written; only the
cache and indexes are temporarily stale. This is the current behaviour for
b2b_org, b2b_org settings, and key-contact writes (create/update are
fire-and-forget; deletes propagate the publish error).

### Key contact denormalization cost

Constructing a fully-denormalized `KeyContact` (or `ProjectMembership`) requires data from
multiple Salesforce objects: `Project_Role__c`, `Contact`, `Alternate_Email__c`, `Asset`,
`Account`, and `Product2`. For PubSub CDC events and backfill, this multi-object fetch is
unavoidable. The sObject cache amortises this cost for live single-object GET
requests: a cache hit requires only one NATS KV read and zero Salesforce
calls. Membership reads still use the SOQL-backed readers.

### Salesforce session management

Prefer **OAuth 2.0 JWT Bearer flow** (private key + client ID); retain
username/password as a fallback configuration pattern. Refresh tokens
proactively before expiry rather than reactively on 401. Share the token source
across goroutines with a mutex or a `golang.org/x/oauth2` token-source
abstraction.

---

## Open Questions

1. **Global org-admin team UID.** Confirm the v2 UID of the global org-admin team and how it is
   provisioned (manually via `lfx-v2-mockdata`, or automatically at deploy time).
2. **`b2b_org` create trigger.** Confirm whether `b2b_org` objects are created on first PubSub
   CDC event (async, driven by Salesforce) or eagerly via a `POST /b2b_orgs` call by EasyCLA /
   LFX Enrollment. The FGA and Indexer payloads are the same either way; the HTTP endpoint is
   only needed for the latter.
3. **Salesforce PubSub CDC channels.** Confirm the exact CDC channel names for `Account`,
   `Asset`, and `Project_Role__c` objects on the B2B org and whether custom CDC is enabled.
4. **`b2b_org` owner assignment.** Confirm whether `owner` relations on `b2b_org` are managed
   by this service (via a sub-collection endpoint) or directly by EasyCLA / LFX Enrollment via
   FGA Sync.
5. **Backfill scope.** Determine whether the default backfill covers all historical membership
   records or only active ones, and whether scope can be narrowed by project or org via query
   parameters on the trigger endpoint.
6. **`membership_tier` indexing.** Confirm whether `membership_tier` (Product2) records need
   their own backfill path, or whether tier data is sufficiently denormalized onto
   `project_membership` objects for all consumer use-cases.
