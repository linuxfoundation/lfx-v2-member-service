# Claude Development Guide for LFX V2 Member Service

This guide provides essential information for Claude instances working with the LFX V2 Member Service codebase. It includes build commands, architecture patterns, and key technical decisions.

> **Central LFX skills:**
> - `lfx-skills:lfx` for cross-repo tasks, "where does X live" questions, owner/peer repo routing, or missing checkouts.
> - `lfx-skills:lfx-platform-architecture` for platform composition, V2 service classes, write/read/access-check flows, NATS/KV ownership, and handoff points across FGA, indexer, query, Heimdall, OpenFGA, Helm, or ArgoCD.
> - For the post-commit review lifecycle, launch the central pre-PR reviewers through the Agent tool after every commit: `lfx-skills:lfx-general-code-reviewer` for general code review, `lfx-skills:lfx-member-service-code-reviewer` for repo-specific member-service conventions/contracts, and `lfx-skills:lfx-member-service-learnings-reviewer` for empirical-pattern matching against `docs/reviews/knowledge-base/`.
> - **Local skills:**
>   - `member-service-dev` auto-attaches on Go and service paths (`**/*.go`, `cmd/**`, `internal/**`, `pkg/**`, `gen/**`, `Makefile`) and owns Go conventions, Goa boundaries, NATS/KV cache and RPC rules, tests, formatting, and the Salesforce-integration callout.
>   - `member-add-endpoint` is the entry point for adding or changing any membership HTTP endpoint (Goa design, regen, handler, tests, Heimdall ruleset update).
>   - Before opening a PR, run the repo-local cycle in order: `/member-service-pr-readiness` for branch/commit shape, then `/member-service-preflight` for mechanical Go validation.
> - Repo-local docs own concrete subjects, payloads, contracts, chart values, and domain behavior. If the plugin is missing, install with `/plugin marketplace add linuxfoundation/lfx-skills` then `/plugin install lfx-skills@lfx-skills`.

## Project Overview

The LFX V2 Member Service is a RESTful API service that provides membership data for the Linux Foundation's LFX platform. It exposes endpoints for querying project-scoped tiers, memberships, and key contacts, as well as write endpoints (POST/PUT/DELETE) for managing key contacts. Data is sourced directly from Salesforce via SOQL queries, with a per-record NATS Key-Value cache to minimise round-trips.

The same binary also runs as a **CDC consumer** (`RUN_MODE=consumer`) that subscribes to Salesforce Change Data Capture events via the Pub/Sub gRPC API and keeps the OpenSearch index and FGA tuples in sync in near-real-time. The consumer runs as a separate Kubernetes Deployment (replicas:1, Recreate strategy) so at most one pod processes CDC events at any time.

### Key Technologies

- **Language**: Go 1.24+
- **API Framework**: Goa v3 (code generation framework)
- **Messaging**: NATS with JetStream for KV caching and RPC
- **Storage**: Eight NATS Key-Value buckets — `membership-cache`, `org-settings`, `member-service-cache`, `pubsub-state`, `cdc-repair`, `org-workspaces`, `org_workspace_projects`, `key-contact-grants`
- **Primary data source**: Salesforce REST API (SOQL queries via `github.com/k-capehart/go-salesforce/v3`)
- **CDC**: Salesforce Pub/Sub gRPC API + Apache Avro decoding (`github.com/linkedin/goavro/v2`)
- **Authentication**: JWT with Heimdall middleware
- **Authorization**: OpenFGA for fine-grained access control
- **Container**: Chainguard distroless images
- **Orchestration**: Kubernetes with Helm charts

## Architecture Overview

The service follows **Clean Architecture** principles with clear separation of concerns. There is no sync job and no PostgreSQL dependency — all membership data is fetched on demand from Salesforce and cached in NATS KV.

```text
cmd/member-api/               # Presentation Layer (HTTP entry point)
├── design/                  # Goa API design specifications
│   ├── membership.go        # API endpoints definition (project-scoped routes)
│   └── type.go              # Goa type definitions (MembershipTier, ProjectMembership, ProjectKeyContact)
├── service/                 # Service handlers (implements Goa interfaces)
│   ├── membership_service.go  # Main service handler with endpoint logic
│   ├── providers.go         # Dependency initialization (NATS, Salesforce, auth)
│   └── error.go             # Error mapping helpers
├── http.go                  # HTTP server setup and middleware
└── main.go                  # Application entry point

gen/                         # Generated code (DO NOT EDIT MANUALLY)
├── membership_service/      # Generated service interfaces and endpoints
└── http/membership_service/ # Generated HTTP server/client code

internal/
├── domain/                  # Domain layer
│   ├── auth.go              # Authenticator interface
│   ├── model/               # Domain entities
│   │   ├── membership.go    # ProjectMembership (MembershipTier in member.go, KeyContact in key_contact.go)
│   │   ├── list_params.go   # ListParams with filter support
│   │   └── cdc_event.go     # CDCEvent, CDCChangeType (transport-agnostic CDC types)
│   └── port/                # Repository interfaces (driven ports)
│       ├── member_reader.go  # MemberReader interface (main read port)
│       ├── project_resolver.go  # ProjectResolver interface (UID ↔ slug ↔ SFID)
│       ├── cache_invalidator.go # CacheInvalidator port (evict sObject cache entries)
│       └── cdc.go           # CDCSubscriber, ReplayStore (CDC driven ports)
├── infrastructure/          # Infrastructure layer
│   ├── auth/                # JWT authentication (Heimdall)
│   ├── mock/                # Mock repository for testing
│   ├── nats/                # NATS KV cache and project RPC client
│   │   ├── cache.go         # CachedValue[T], CacheStatus, TTLConfig
│   │   ├── client.go        # NATSClient with KV bucket initialisation
│   │   ├── config.go        # NATS configuration
│   │   ├── project_id_map_handler.go  # RPC handler for lfx.member.project-id-map.lookup
│   │   ├── b2b_org_lookup_handler.go  # RPC handler for lfx.member.b2b_org_lookup
│   │   ├── project_rpc.go   # NATS RPC calls to the project-service
│   │   └── storage.go       # KV cache Get/Put helpers for each record type
│   ├── project/             # ProjectResolver implementation
│   │   └── resolver.go      # Chains NATS RPC → SOQL → KV cache
│   └── salesforce/          # Salesforce SOQL client and repositories
│       ├── config.go        # Config struct and ConfigFromEnv()
│       ├── helpers.go       # parseSOQLTime, parseSOQLDateTime, quoteSOQL
│       ├── cache_invalidator.go # CacheInvalidator: evicts sObject cache entries (CDC use)
│       ├── key_contact_repo.go  # Single and batched key-contact queries by Asset/SFID
│       ├── member_reader.go # MemberReader: Salesforce-first + KV cache
│       ├── member_repo.go   # FetchAllMembers, FetchMemberBySFID
│       ├── membership_repo.go  # FetchMembershipsByProjectSFID, etc.
│       ├── models.go        # Salesforce SOQL result types
│       ├── project_repo.go  # FetchSFIDBySlug, FetchProjectByPCCID, etc.
│       ├── soql.go          # QueryInto[T], QuerySingle[T], QueryOptional[T]
│       └── pubsub/          # Salesforce Pub/Sub gRPC + Avro CDC adapter
│           ├── pubsub_client.go   # Client: satisfies port.CDCSubscriber; manages gRPC stream
│           ├── pubsub_events.go   # Avro decoding → model.CDCEvent normalisation
│           ├── pubsub_replay.go   # ReplayStore: NATS KV cursor persistence (port.ReplayStore)
│           └── proto/             # Generated gRPC stubs (DO NOT EDIT — use make protoc-gen)
├── middleware/              # HTTP middleware
│   ├── authorization.go     # Extracts Authorization header to context
│   └── request_id.go        # Request ID propagation
└── service/                 # Business logic / use case orchestration
    ├── member_reader.go     # MemberReaderOrchestrator
    └── cdc_consumer.go      # CDCConsumer: dispatches CDCEvents to entity handlers

pkg/
└── constants/               # Shared constants (HTTP headers, NATS buckets, etc.)

charts/                      # Helm chart for Kubernetes deployment
└── lfx-v2-member-service/
```

### Data Flow

```text
HTTP Request
    │
    ▼
MembershipService (Goa handler)
    │
    ▼
MemberReaderOrchestrator
    │
    ▼
salesforce.MemberReader (implements port.MemberReader)
    │
    ├── 1. Check NATS KV cache (membership-cache bucket)
    │        CacheStatusFresh  → return cached value
    │        CacheStatusStale  → return cached value + trigger background refresh
    │        CacheStatusExpired/Miss → proceed to Salesforce
    │
    ├── 2. ProjectResolver.SFIDFromUID (for project-scoped queries)
    │        └── NATS RPC → project-service (get_slug)
    │        └── SOQL query → Salesforce (Project__c WHERE Slug__c = ?)
    │        └── KV cache write (project-sfid.{uid})
    │
    ├── 3. SOQL query → Salesforce REST API
    │
    └── 4. KV cache write → return to caller
```

### Key Design Principles

1. **Salesforce as source of truth**: No PostgreSQL, no sync job. Every record is fetched from Salesforce on cache miss.
2. **Type-prefixed KV keys**: All SOQL-cached data lives in `membership-cache` with type-prefixed keys (e.g., `tier.`, `membership.`, `key-contacts.`, `project-sfid.`, `project-uid.`); sObject conditional-GET entries live in `member-service-cache`.
3. **Stale-while-revalidate**: `CachedValue[T]` envelopes carry `stale_at` and `expires_at` timestamps. Stale entries are served immediately while a background goroutine refreshes from Salesforce.
4. **Database Independence**: Repository interfaces allow switching storage backends.
5. **Testability**: Each layer can be tested in isolation using mocks.
6. **Separation of Concerns**: Clear boundaries between layers.

## API Endpoints

### Project membership

| Method | Path                         | Description              | OpenFGA Check                           |
|--------|------------------------------|--------------------------|-----------------------------------------|
| GET    | `/project_memberships/{uid}` | Get a project membership | `auditor` on `project_membership:{uid}` |

### Key contact endpoints (nested under project_membership)

Key contacts are nested under their membership. GET/PUT/DELETE return 404 (not 403) when the fetched contact's `membership_uid` doesn't match the path — avoids leaking record existence.

| Method | Path                                                       | Description          | OpenFGA Check                                      |
|--------|------------------------------------------------------------|----------------------|----------------------------------------------------|
| GET    | `/project_memberships/{membership_uid}/key_contacts/{uid}` | Get a key contact    | `auditor` on `project_membership:{membership_uid}` |
| POST   | `/project_memberships/{membership_uid}/key_contacts`       | Create a key contact | `writer` on `project_membership:{membership_uid}`  |
| PUT    | `/project_memberships/{membership_uid}/key_contacts/{uid}` | Update a key contact | `writer` on `project_membership:{membership_uid}`  |
| DELETE | `/project_memberships/{membership_uid}/key_contacts/{uid}` | Remove a key contact | `writer` on `project_membership:{membership_uid}`  |

### B2B org write endpoints

| Method | Path                       | Description                                         | OpenFGA Check                              |
|--------|----------------------------|-----------------------------------------------------|--------------------------------------------|
| POST   | `/b2b_orgs`                | Create a B2B org from a Salesforce Account SFID     | `member` on `team:{globalOrgAdminTeamUID}` |
| PUT    | `/b2b_orgs/{uid}`          | Partial update of a B2B org                         | `writer` on `b2b_org:{uid}`                |
| GET    | `/b2b_orgs/{uid}`          | Get a B2B org                                       | `auditor` on `b2b_org:{uid}`               |
| GET    | `/b2b_orgs/{uid}/settings`                    | Get org access-control settings (writers, auditors) | `auditor` on `b2b_org:{uid}`               |
| PUT    | `/b2b_orgs/{uid}/settings`                    | Full-replace org writers and/or auditors            | `writer` on `b2b_org:{uid}`                |
| POST   | `/b2b_orgs/{uid}/settings/users`              | Add a principal (invite or accept immediately)      | `writer` on `b2b_org:{uid}`                |
| PUT    | `/b2b_orgs/{uid}/settings/users/{email}`      | Change a principal's role                           | `writer` on `b2b_org:{uid}`                |
| DELETE | `/b2b_orgs/{uid}/settings/users/{email}`      | Remove a principal                                  | `writer` on `b2b_org:{uid}`                |

**Settings semantics:** `nil` writers/auditors = keep existing; explicit `[]` = clear all. Entries with a `username` are `accepted` (FGA tuple emitted); without username are `pending` (no FGA tuple). The legacy `owner` relation is retired — use `writer` instead. Settings are stored in the `org-settings` NATS KV bucket (authoritative, no MaxAge TTL), separate from the Salesforce-backed `membership-cache` bucket.

**Settings publish on PUT:** every successful `PUT /b2b_orgs/{uid}/settings` emits two fire-and-forget messages in order:
1. `lfx.fga-sync.update_access` (ObjectType=`b2b_org`) — FGA tuple sync for writers/auditors
2. `lfx.index.b2b_org_settings` — OpenSearch settings doc keyed by org UID (`ActionCreated` on first write, `ActionUpdated` thereafter)

FGA is published before the indexer so access tuples land before the doc is searchable. Errors on either publish are swallowed with `publish_failed_for_backfill_repair=true`; recovery is a re-PUT of the settings. The `lfx.index.b2b_org_settings` doc is **not** published from the backfill runner — it is created on demand by the first PUT that adds a writer or auditor.

### Admin

| Method | Path             | Description                                                              | OpenFGA Check                              |
|--------|------------------|--------------------------------------------------------------------------|--------------------------------------------|
| POST   | `/admin/reindex` | Trigger a full or incremental reindex of cached entities into OpenSearch | `member` on `team:{globalOrgAdminTeamUID}` |

Returns HTTP 202 with `{ "run_id": "<uuid>" }` for full/since/targeted runs, or `{ "run_id": "<uuid>", "selected_count": <n> }` for `cdc_repair` runs. The `run_id` is for log correlation only — search slog for `run_id=<uuid>` to track progress. Requires a single `type` (one of `b2b_org`, `project_membership`, `key_contact`, `b2b_org_settings` — no all-types shortcut), and layers on one of `since` (RFC 3339 with explicit zone for incremental), `until` (RFC 3339 inclusive upper bound; **requires `since`**, rejects `since > until`; together they define a bounded `[since, until]` window sized to fit quota), `items` (array of `{uid}` objects, all of `type`, max 100, for targeted surgical reindex), or `cdc_repair` (bool; drains the CDC quota-repair queue for `type` — see [Quota-Repair Drain](./docs/backfill-reindex.md#cdc-quota-repair-drain), `b2b_org_settings` unsupported); plus `dry_run` (count only, no publish; not applicable to `cdc_repair`).

> **Batching + quota guard (LFXV2-2787).** Targeted (`items`) `project_membership` / `key_contact` reindex batch-fetches via the CDC batch ports (~1 SOQL batch vs ~3–5 SF calls/item); `b2b_org` stays per-item. The **full/filtered** paths are quota-gated (`ADMIN_REINDEX_QUOTA_THRESHOLD`, default `0.80`): a synchronous handler gate returns HTTP `503` at/above threshold, and a mid-run passive check stops the run (`stopped_early=true`). **Targeted is exempt** (bounded surgical tool). Stop-and-rerun converges only for bounded runs — window large reindexes via `since`/`until`. See [docs/backfill-reindex.md](./docs/backfill-reindex.md#quota-guard--windowed-reindex).

> **Operational note — `key_contact` is high-volume (~300k records in prod).** Reindex only the active window by passing a `since` ~2 years back (e.g. `{"type":"key_contact","since":"2024-06-01T00:00:00Z"}`) rather than a full key_contact reindex. A full pass takes hours and is likely to be interrupted by pod eviction. The `key_contact` `since` filter checks `Project_Role__c.LastModifiedDate` only (Contact/Asset field changes are not captured).

### Utility

| Method | Path                                 | Description        | OpenFGA Check |
|--------|--------------------------------------|--------------------|---------------|
| GET    | `/readyz`                            | Readiness probe    | None          |
| GET    | `/livez`                             | Liveness probe     | None          |
| GET    | `/_memberships/openapi*.{json,yaml}` | OpenAPI spec files | None          |

> **Note:** The surface is resource-rooted (`/b2b_orgs/...`, `/project_memberships/...`, `/admin/reindex`). There are no `/projects/{project_id}/...` drill-down routes and no `/members/*` or `/memberships/*` routes — requests to those paths are unmatched and return `404`.

> **Optimistic concurrency:** mutations (`PUT /b2b_orgs/{uid}`, `PUT /b2b_orgs/{uid}/settings`, the per-principal `POST`/`PUT`/`DELETE` settings-user endpoints, and `PUT`/`DELETE` key contacts) take an `If-Match` header carrying the current resource version (`POST` key-contact creates do not). A stale value returns `412 Precondition Failed`.

> **Internal filtering:** SOQL-level membership filtering (`MembershipFilters` in `internal/domain/model/list_params.go`, e.g. tier UID / product) is applied internally on the Salesforce read path. It is not exposed as an HTTP `filter` query parameter on the current resource-rooted surface.

## Development Workflow

### Common Development Tasks

#### 1. Generate API Code (REQUIRED after design changes)

```bash
make apigen
# or directly:
goa gen github.com/linuxfoundation/lfx-v2-member-service/cmd/member-api/design -o .
```

#### 2. Build the Service

```bash
make build
```

#### 3. Run Tests

```bash
make test              # Run unit tests (verbose, race detector, writes coverage.out)
```

#### 4. Run the Service Locally

```bash
# Basic run with Salesforce and NATS
export NATS_URL=nats://localhost:4222
export SF_INSTANCE_URL=https://linuxfoundation.my.salesforce.com
export SF_CLIENT_ID=<client-id>
export SF_CLIENT_SECRET=<client-secret>
make run

# With debug logging
export LOG_LEVEL=debug
make run

# With mock auth (bypasses Heimdall JWT validation)
export JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=test-user
make run
```

#### 5. Lint and Format Code

```bash
make fmt    # Format code
make lint   # Run golangci-lint
```

## Work cycle — post-commit and pre-PR reviews

> **CRITICAL — while the branch is pre-PR, post-commit review is mandatory.** After every commit on the local branch, launch `lfx-skills:lfx-general-code-reviewer`, `lfx-skills:lfx-member-service-code-reviewer`, AND `lfx-skills:lfx-member-service-learnings-reviewer` via the Agent tool with `run_in_background: true` — then keep working while they run. If Claude displays plugin agents without the `lfx-skills:` namespace, use the equivalent displayed reviewer names. Before opening a PR, every running review must return clean (or remaining findings explicitly documented as trade-offs), the **full-branch sweep** must run clean if the branch has more than one commit (`branch` arg), AND `/member-service-pr-readiness` must clear every Critical finding before `/member-service-preflight` runs.
>
> **Once the PR is open, do NOT invoke the central reviewers on iteration commits.** CodeRabbit + Copilot auto-trigger on every push and own the audit surface from that point. The central reviewers are pre-PR insurance only.

### Post-commit (pre-PR phase, after every commit, asynchronous)

1. **Commit your work.** `git commit -s -S`. Do not wait for any prior review to finish.
2. **Immediately launch all three reviewer subagents in parallel.** Use `subagent_type: lfx-skills:lfx-general-code-reviewer`, `run_in_background: true`; `subagent_type: lfx-skills:lfx-member-service-code-reviewer`, `run_in_background: true`; and `subagent_type: lfx-skills:lfx-member-service-learnings-reviewer`, `run_in_background: true`.
3. **Post-commit mode prompt for each reviewer (exact):** `target repo: lfx-v2-member-service\n\nReview the latest commit.` Append `extra: <focus>` on a new line only when there is a priority hint to add. Do NOT pass `branch` here. If this work cycle is launched from the LFX workspace parent, the `target repo:` line is required so both reviewers operate in this repo.
4. **Keep working.** Start the next commit while the reviewers run. Do not block on them.
5. **When the reviews return:** roll every Critical finding and every reasonable Important finding into the next commit.

### Pre-PR (drain the queue, sweep cumulative state, then open)

When the work is done and no more code commits are planned:

1. **Wait for every running review to complete.**
2. **If any returned review flags Critical or reasonable Important:** add a fix commit, launch both reviewers again on the new state, wait, and loop until clean or explicitly documented as a trade-off.
3. **Full-branch sweep — only if the branch has more than one commit.** Launch `lfx-skills:lfx-general-code-reviewer`, `lfx-skills:lfx-member-service-code-reviewer`, and `lfx-skills:lfx-member-service-learnings-reviewer` with prompt **`target repo: lfx-v2-member-service\nbranch\n\nReview the branch's diff against origin/main.`**. Address any new findings, then re-run the sweep until clean.
4. **Run `/member-service-pr-readiness`** for branch and commit shape only.
5. **Run `/member-service-preflight`** for mechanical Go validation and PR summary.
6. **Only then push and open the PR.**

### Post-PR iteration (responding to bot feedback on an open PR)

1. Wait for CodeRabbit + Copilot to comment after each push.
2. Triage every Critical and reasonable Important finding against current code.
3. Roll fixes into a `fix(review): ...` commit.
4. Push. Repeat until clean.

## Adding New Endpoints (Goa is design-first)

API is defined in `cmd/member-api/design/` (`membership.go` for endpoints, `type.go` for types). `gen/` is generated — do not edit by hand.

1. Update `cmd/member-api/design/membership.go` with new method
2. Run `make apigen` to regenerate `gen/`
3. Implement the new method in `cmd/member-api/service/membership_service.go`
4. Add tests for the new endpoint
5. Update Heimdall ruleset in `charts/lfx-v2-member-service/templates/ruleset.yaml`

## NATS Storage

The service uses five NATS KV buckets.

### `pubsub-state` Bucket

Stores the Salesforce Pub/Sub replay cursor (opaque `[]byte`) per CDC channel. **Authoritative state** — no MaxAge TTL. Key pattern: `pubsub-replay.<channel>` with slashes replaced by underscores (e.g. `pubsub-replay._data_ChangeEvents`).

### `cdc-repair` Bucket

Stores durable pending markers for records the CDC consumer's quota guard skipped while the Salesforce API quota was near-exhausted. **Authoritative state** — no MaxAge TTL, `history: 1`. Key pattern: `pending.{reindex_type}.{sfid}` → `{"skipped_at": "<RFC 3339>"}`. Written by the consumer (`recordSkippedForRepair`); listed and revision-conditionally deleted by `POST /admin/reindex {cdc_repair:true}` (no distributed lock — idempotent reindex + revision-conditional delete is the sole race guard). See [docs/backfill-reindex.md](./docs/backfill-reindex.md#cdc-quota-repair-drain) and [docs/cdc-consumer.md](./docs/cdc-consumer.md#quota-guard).

### `key-contact-grants` Bucket

Stores the durable index of published key-contact FGA grants: `{membership_uid, username}` per key contact UID. **Authoritative state** — no MaxAge TTL, `history: 1`. Key pattern: `key_contact.{sfid}`. It exists because a CDC delete for `Project_Role__c` carries only the key contact's own SFID — by then the Salesforce record is gone, so the parent `MembershipUID` and the granted `username` cannot be recovered from any other source. Written (revision-conditional CAS) by `PublishKeyContactFGA` on every successful `member_put`; read and cleared by the CDC delete handler and the API delete path. (LFXV2-2907)

### `org-settings` Bucket

Stores b2b_org access-control principals (writers, auditors, pending invites). **Authoritative state** — no MaxAge TTL, no soft-TTL envelopes. Key pattern: `org-settings.{orgUID}` → raw JSON `model.B2BOrgSettings`. Optimistic locking via KV revision on every PUT.

### `member-service-cache` Bucket

Stores raw Salesforce sObject REST responses as `SObjectCacheEntry` JSON envelopes (no soft-TTL wrappers). Used for B2B org lookups and other sObject fetches that bypass the SOQL path.

### `membership-cache` Bucket

All records share the `membership-cache` bucket. Keys are namespaced by a type prefix to avoid collisions.

| Key pattern                     | Contents                                         | Soft TTL                |
|---------------------------------|--------------------------------------------------|-------------------------|
| `tier.{uid}`                    | `CachedValue[*model.MembershipTier]`             | 6 h stale / 23 h expire |
| `membership.{uid}`              | `CachedValue[*model.ProjectMembership]`          | 6 h stale / 23 h expire |
| `key-contacts.{membership_uid}` | `CachedValue[[]*model.KeyContact]`               | 6 h stale / 23 h expire |
| `project-sfid.{project_uid}`    | `CachedValue[string]` (Salesforce Project__c.Id) | 6 h stale / 23 h expire |
| `project-uid.{slug}`            | `CachedValue[string]` (v2 project UUID)          | 6 h stale / 23 h expire |
| `soql.{...}`                    | `CachedValue[...]` (paged SOQL result batches)   | 6 h stale / 23 h expire |

The NATS bucket itself has a 24-hour `MaxAge` (hard eviction), which is always later than the soft `expires_at` timestamp inside each envelope.

### Cache Freshness States

Defined in `internal/infrastructure/nats/cache.go`:

| Status               | Meaning                               | Caller behaviour                                         |
|----------------------|---------------------------------------|----------------------------------------------------------|
| `CacheStatusFresh`   | Within stale threshold                | Serve immediately.                                       |
| `CacheStatusStale`   | Past stale threshold, not yet expired | Serve immediately; trigger background refresh goroutine. |
| `CacheStatusExpired` | Past expiry threshold                 | Do **not** serve; fetch synchronously from Salesforce.   |
| `CacheStatusMiss`    | Key not present in bucket             | Fetch synchronously from Salesforce.                     |

## ProjectResolver

`internal/infrastructure/project/resolver.go` implements `port.ProjectResolver`. It is the bridge between the v2 project UUID world and the Salesforce `Project__c.Id` world.

### Why it exists

Every project-scoped SOQL query requires a Salesforce `Project__c.Id` in its `WHERE` clause. The HTTP API receives a v2 UUID (`project_id` path parameter). Without `ProjectResolver`, all list endpoints would silently return zero results.

### Resolution chain: `SFIDFromUID`

```text
SFIDFromUID(ctx, projectUID)
    │
    ├── 1. KV cache lookup: project-sfid.{uid}
    │        Fresh/Stale → return cached SFID
    │
    ├── 2. NATS RPC → project-service (lfx.projects-api.get_slug)
    │        returns slug string
    │
    ├── 3. KV cache write: project-uid.{slug} → uid  (side-effect)
    │
    ├── 4. SOQL query → Salesforce
    │        SELECT Id FROM Project__c WHERE Slug__c = '<slug>'
    │
    └── 5. KV cache write: project-sfid.{uid} → sfid
            return sfid
```

### Resolution chain: `UIDFromSlug`

```text
UIDFromSlug(ctx, slug)
    │
    ├── 1. KV cache lookup: project-uid.{slug}
    │        Fresh/Stale → return cached UID
    │
    ├── 2. NATS RPC → project-service (lfx.projects-api.slug_to_uid)
    │        returns uid string
    │
    └── 3. KV cache write: project-uid.{slug} → uid
            return uid
```

### Registration

`NewProjectResolver` in `internal/infrastructure/project/resolver.go` wires together `*nats.ProjectRPC`, `*salesforce.ProjectRepo`, and `*nats.Storage`. The resolver is constructed in `cmd/member-api/service/providers.go` and passed to `salesforce.NewMemberReader`.

## NATS RPC Endpoints

The service handles two inbound NATS request/reply subjects that allow other services to resolve identifiers without depending on Salesforce or this service's HTTP layer.

### Project ID Map Lookup (`lfx.member.project-id-map.lookup`)

Implemented in `internal/infrastructure/nats/project_id_map_handler.go`. Resolution chains: KV cache → project-service NATS RPC (get slug) → Salesforce SOQL.

| Field         | Value                              |
|---------------|------------------------------------|
| **Subject**   | `lfx.member.project-id-map.lookup` |
| **Transport** | NATS core request/reply            |
| **Queue group** | `lfx-v2-member-service`          |

**Request:** `{"project_uid": "<v2 project UUID>"}`

**Response — success:** `{"project_sfid": "<Salesforce Project__c.Id>"}`

**Response — error:** `{"error": "<human-readable message>"}`

### B2B Org Lookup (`lfx.member.b2b_org_lookup`)

Implemented in `internal/infrastructure/nats/b2b_org_lookup_handler.go`. Validates that an id resolves to an indexed `b2b_org` via `B2BOrgReader.GetB2BOrg` (sObject cache → Salesforce Account). Returns the canonical 18-char Account SFID on success. Used by consumer services (e.g. committee-service) to validate `organization.id` on write paths (LFXV2-2400).

| Field         | Value                         |
|---------------|-------------------------------|
| **Subject**   | `lfx.member.b2b_org_lookup`   |
| **Transport** | NATS core request/reply       |
| **Queue group** | `lfx-v2-member-service`     |

**Request:** `{"id": "<b2b_org uid or 15/18-char Account SFID>"}`

**Response — success:** `{"id": "<canonical 18-char Account SFID>"}`

**Response — not found:** `{"error": "b2b org not found"}`

**Response — error:** `{"error": "<human-readable message>"}` (e.g. `id is required`, `b2b org lookup failed`)

> **Note:** Since LFXV2-2049, `b2b_org.uid` is the 18-char Account SFID itself — there is no separate UUID translation step. The lookup RPC confirms existence and normalizes SFID form; it does not map CDP UUIDs or other non-SFID identifiers.

## CDC Consumer

Set `RUN_MODE=consumer` to run as a CDC consumer instead of the HTTP API. The consumer subscribes to Salesforce Pub/Sub gRPC, decodes Avro payloads → `model.CDCEvent`, and dispatches to per-entity handlers that invalidate the sObject cache, re-fetch from Salesforce, and publish indexer + FGA messages.

**Non-obvious invariants:**
- **GAP_DELETE**: `dispatchRecordIDs` checks `changeType == CDCChangeDelete || changeType == CDCChangeGapDelete` explicitly — `HasSuffix` was avoided because `UNDELETE` also ends with `"DELETE"` and would incorrectly route to the delete path. Since LFXV2-3034 the delete path withdraws FGA tuples, so the blast radius of getting this wrong is a live restored object losing all its access, not just a spurious index tombstone.
- **Real deletes only purge FGA**: the delete handlers are reached from two call sites — a genuine CDC delete, and `handleAbsentAsDelete` for records missing from the periodic SOQL query. Only the first may publish `delete_access`, because a record can be absent while still existing (a lapsed membership drops an org from the query). The two are separate entry points (`handleAccountDelete`/`handleAccountAbsent`, `handleAssetDelete`/`handleAssetAbsent`) so absence cannot reach the purge without someone changing a call site on purpose. Index convergence still runs on both paths — a tombstone is rebuilt by `/admin/reindex`, a revoked grant is not.
- **A purge never leaves zero tuples**: fga-sync refuses to delete `team:`-subject tuples, so a deleted object keeps the staff-team reader grant from LFXV2-2937. Any test or audit asserting an empty tuple set for a deleted object will fail against a correct implementation.
- **Replay cursor**: written on a fresh `context.Background()` after each event so a SIGTERM does not skip the final commit. Cursor survives pod restarts via `pubsub-state` NATS KV.
- **Early exit → pod restart**: `defer cancel()` in the Run goroutine ensures that if the gRPC stream dies unrecoverably, `<-ctx.Done()` unblocks and the pod exits so Kubernetes restarts it.
- **Liveness probe**: always returns 200 — K8s handles shutdown via SIGTERM, not probe failures.
- **Single active consumer**: `replicas:1` + `strategy:Recreate` in the Deployment — no app-level lease.
- **Proto stubs**: committed to `internal/infrastructure/salesforce/pubsub/proto/` — normal builds never need `protoc`. Use `make protoc-install && make protoc-gen` only when updating the Salesforce proto schema.

## Org Settings Invite Flow

`OrgSettingsWriter.AddPrincipal` calls `UserReader.UsernameByEmail`: if an LFID exists the entry is accepted immediately; otherwise `InviteSender.SendInvite` is called (best-effort — errors logged, entry still persisted as pending). Same email + same role re-sends the invite in place; different role returns Conflict.

`InviteAcceptedService` (`internal/service/invite_accepted.go`) subscribes to `lfx.invite-service.invite_accepted` via `natsinf.SubscribeInviteAccepted` (queue group `"lfx-v2-member-service"`). Events with `resource.type != "b2b_org"` are dropped immediately (no KV access). For org events, `ListSettingsOrgUIDs` scans all org settings; per org, pending entries matching the recipient email are promoted (list-authoritative: email in one list → promote it; email in both → tie-break on `role`; unknown role → skip). Promotes the entries to accepted in-place and republishes FGA + indexer via `OrgSettingsWriter.Update`. Retries up to 3× on CAS Conflict.

## Authentication (JWT / Heimdall)

JWT authentication is implemented via `internal/infrastructure/auth/`:

- **`JWTAuth`**: Real implementation that validates tokens via Heimdall JWKS.
- **`MockJWTAuth`**: Test mock that implements the `domain.Authenticator` interface.

### Configuration

| Variable                                 | Description                                 | Default                                 |
|------------------------------------------|---------------------------------------------|-----------------------------------------|
| `JWKS_URL`                               | Heimdall JWKS endpoint                      | `http://heimdall:4457/.well-known/jwks` |
| `AUDIENCE`                               | JWT audience                                | `lfx-v2-member-service`                 |
| `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` | Mock principal for local dev (bypasses JWT) | `""` (disabled)                         |

When `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` is set, the service skips JWT validation entirely and uses that value as the authenticated principal. **Only use for local development.**

Heimdall validates the OIDC token and forwards a signed JWT (`principal` claim); `JWTAuth()` (the Goa security handler) validates that JWT and stores the principal in context as `constants.PrincipalContextID`.

## Authorization (OpenFGA)

The service enforces fine-grained authorization via the v13 OpenFGA model (defined in `lfx-v2-helm`). Relevant types:

```dsl
type b2b_org
  relations
    define global_org_admin: [team#member]
    define owner: [user]
    define writer: [user] or owner or global_org_admin
    define parent: [b2b_org]
    define child: [b2b_org]
    define membership: [project_membership]
    define key_contact: key_contact from membership
    define auditor: [user, team#member]
                    or writer
                    or auditor from parent
                    or auditor from child
                    or key_contact from membership

type project_membership
  relations
    define b2b_org: [b2b_org]
    define project: [project]
    define writer: writer from b2b_org
    define auditor: writer or auditor from b2b_org or auditor from project
    define key_contact: [user]
```

**Hierarchy cascade:** `auditor` on `b2b_org` propagates transitively through the entire connected org hierarchy via `parent` and `child` tuples. A user with `auditor` on any org can view every other org in the same hierarchy. `writer` does not cascade — edit access stays on the assigned org only.

**Downward cascade to memberships:** `project_membership.auditor` includes `auditor from b2b_org`, so an `auditor` grant on an org also confers auditor on every `project_membership` under it — and, since key-contact routes access-check the parent membership, on every key contact under those. This is why the LF team grant below is written on `b2b_org` only.

**Blanket LF team auditor grants:** the team named by `LF_STAFF_TEAM_NAME` holds `auditor` on **every** `b2b_org`, asserted on every full-sync publish path (see [docs/fga-contract.md](./docs/fga-contract.md)). Combined with the cascade above, LF staff have read access to all orgs, memberships and key contacts. The name is env-invariant, so `charts/lfx-v2-member-service/values.yaml` holds the single authoritative copy and the deploy — not the backfill — is when grants start being written. Contractors are not included — contractor is its own role (LFXV2-3071), not an inheritance of the staff grant. These grants are effectively permanent: fga-sync never deletes a tuple whose subject begins with `team:` (the guard is in the deployed fga-sync, v0.3.1 or later, not this repo's `go.mod` pin), so no service code path can revoke them — only `scripts/revoke-lf-teams-auditor-openfga.sh`.

Authorization checks in Heimdall ruleset (`charts/lfx-v2-member-service/templates/ruleset.yaml`):
- **GET `/b2b_orgs/:uid`** — `auditor` on `b2b_org:{uid}`
- **POST `/b2b_orgs`** — `member` on `team:{globalOrgAdminTeamUID}`
- **PUT `/b2b_orgs/:uid`** — `writer` on `b2b_org:{uid}`
- **GET `/b2b_orgs/:uid/settings`** — `auditor` on `b2b_org:{uid}` (auditor, not writer, so trusted principals can see the pending-invite list)
- **PUT `/b2b_orgs/:uid/settings`** — `writer` on `b2b_org:{uid}`
- **POST `/b2b_orgs/:uid/settings/users` and PUT/DELETE `/b2b_orgs/:uid/settings/users/:email`** — `writer` on `b2b_org:{uid}`
- **GET `/project_memberships/:uid`** — `auditor` on `project_membership:{uid}`
- **GET `/project_memberships/:membership_uid/key_contacts/:uid`** — `auditor` on `project_membership:{membership_uid}`
- **POST/PUT/DELETE `/project_memberships/:membership_uid/key_contacts...`** — `writer` on `project_membership:{membership_uid}` (POST also runs `json_content_type`)
- **POST `/admin/reindex`** — `member` on `team:{globalOrgAdminTeamUID}`
- **GET `/_memberships/openapi*`** — `allow_all` passthrough

When `openfga.enabled` is false (local dev), every rule falls back to `allow_all`. There are no `/projects/{project_id}/*`, `/members/*`, or `/memberships/*` rules.

## Testing Patterns

### Unit Tests

- Mock all external dependencies using the `mock` package in `internal/infrastructure/mock/`
- Use `auth.MockJWTAuth` for authentication mocking
- Table-driven tests for comprehensive coverage
- Each function has exactly ONE corresponding test function with multiple cases
- Unit tests alongside implementation with `*_test.go` suffix

## Environment Variables

### Service Configuration

| Variable                                 | Description                                 | Default                                 | Required |
|------------------------------------------|---------------------------------------------|-----------------------------------------|----------|
| `PORT`                                   | HTTP listen port                            | `8080`                                  | No       |
| `NATS_URL`                               | NATS server URL                             | `nats://localhost:4222`                 | No       |
| `NATS_TIMEOUT`                           | NATS connection timeout                     | `10s`                                   | No       |
| `NATS_MAX_RECONNECT`                     | Max NATS reconnect attempts                 | `3`                                     | No       |
| `NATS_RECONNECT_WAIT`                    | Wait between reconnects                     | `2s`                                    | No       |
| `LOG_LEVEL`                              | Log level (debug/info/warn/error)           | `info`                                  | No       |
| `LOG_ADD_SOURCE`                         | Include source location in logs             | `true`                                  | No       |
| `JWKS_URL`                               | Heimdall JWKS endpoint for JWT verification | `http://heimdall:4457/.well-known/jwks` | No       |
| `AUDIENCE`                               | JWT audience                                | `lfx-v2-member-service`                 | No       |
| `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` | Mock auth for local dev                     | `""`                                    | No       |
| `REPOSITORY_SOURCE`                      | Storage backend (`salesforce` or `mock`)    | `salesforce`                            | No       |
| `RUN_MODE`                               | `consumer` (CDC consumer) / `avatar-backfill` (one-off Job); omit for API | `""` (API mode)           | No       |
| `MESSAGING_SOURCE`                       | NATS messaging backend (`nats` or `mock`)    | `nats`                                  | No       |
| `LFX_SELF_SERVE_BASE_URL`                | Base URL injected as `ReturnURL` in org-settings invite emails | `""`          | No       |
| `LF_STAFF_TEAM_NAME`                     | OpenFGA team name granted blanket `auditor` on every `b2b_org`. Set from `values.yaml` (`lf-staff`), which is the only copy of the name; unset grants nothing. Clearing it stops new grants but already-written tuples survive (fga-sync never deletes a `team:`-subject tuple) | `""` (chart sets `lf-staff`) | No |
| `ADMIN_REINDEX_QUOTA_THRESHOLD`          | Fraction of daily Salesforce REST quota at/above which the backfill quota guard refuses/stops a run: the `cdc_repair` drain (refuses to start / stops mid-page) **and** the full/filtered reindex paths (synchronous HTTP `503` + mid-run stop). Targeted (`items`) is exempt. | `0.80` | No |

### Avatar Backfill Mode (`RUN_MODE=avatar-backfill`)

A one-off Kubernetes Job that re-enriches `b2b_org_settings` writer/auditor avatars from the
auth-service and republishes the indexer doc. It is **not** a separate backfill framework: it builds
a full-mode, avatar-enriching `BackfillRequest` (`Type="b2b_org_settings"`, `EnrichAvatars=true`)
and hands it to the same `Runner` that backs `POST /admin/reindex` — so it reuses the run-id, dry-run,
and full-run lock control plane. Idempotent (an org with no avatar drift is skipped, so it doubles as
the recurring refresh) and tolerant of a bounded number of transient auth-service lookup failures
before the Job exits non-zero. `REPOSITORY_SOURCE=mock` runs it end to end without Salesforce creds.

| Variable                      | Description                                              | Default |
|-------------------------------|----------------------------------------------------------|---------|
| `AVATAR_BACKFILL_DRY_RUN`     | Compute drift without writing (set `false` to persist)   | `true`  |
| `AVATAR_BACKFILL_MISSING_ONLY`| Only enrich principals whose avatar is currently empty   | `false` |
| `AVATAR_BACKFILL_SLEEP`       | Go duration between auth-service lookups (Auth0 rate cap) | `0`     |

### Consumer Mode Variables (only read when `RUN_MODE=consumer`)

| Variable              | Description                                                                 | Default                              | Required |
|-----------------------|-----------------------------------------------------------------------------|--------------------------------------|----------|
| `SF_PUBSUB_ENDPOINT`  | Salesforce Pub/Sub gRPC endpoint                                            | — (fatal if empty)                   | Yes      |
| `SF_ORG_ID`           | Salesforce 18-char Org ID injected as `tenantid` gRPC metadata header      | — (fatal if empty)                   | Yes      |
| `SF_CDC_CHANNEL`      | CDC channel to subscribe to                                                 | `/data/ChangeEvents`                 | No       |
| `CDC_QUOTA_REFRESH_STALE_AFTER` | Go duration; how old a quota reading must be before the quota guard issues an active `/limits` refresh. `0` disables active refresh. | `5m` | No |
| `GLOBAL_ORG_ADMIN_TEAM_UID` | v2 UID of the platform org-admin team (same as API mode)            | `_null`                              | No       |
| `LF_STAFF_TEAM_NAME`  | Blanket `auditor` team name (same as API mode) — the CDC Account upsert path asserts it too | `""` (chart sets `lf-staff`) | No       |

### Salesforce Credentials

Credentials are injected from a pre-existing Kubernetes Secret (see Helm chart `values.yaml` `salesforce.secrets` stanza). At least one complete authentication flow must be configured.

| Variable              | Description                                                                | Required    |
|-----------------------|----------------------------------------------------------------------------|-------------|
| `SF_INSTANCE_URL`     | Salesforce instance URL (e.g. `https://linuxfoundation.my.salesforce.com`) | Yes         |
| `SF_CLIENT_ID`        | Connected-app consumer key                                                 | Yes         |
| `SF_CLIENT_SECRET`    | Consumer secret (username/password or client-credentials flow)             | Conditional |
| `SF_USERNAME`         | Salesforce username (username/password or JWT bearer flow)                 | Conditional |
| `SF_PASSWORD`         | Salesforce password (username/password flow)                               | Conditional |
| `SF_SECURITY_TOKEN`   | Security token appended to password                                        | No          |
| `SF_CONSUMER_RSA_PEM` | PEM-encoded RSA private key (JWT bearer flow)                              | Conditional |
| `SF_API_VERSION`      | Salesforce REST API version                                                | `v63.0`     |

**Authentication flows (one must be satisfiable):**

- **JWT bearer**: `SF_USERNAME` + `SF_CONSUMER_RSA_PEM`
- **Username/password**: `SF_USERNAME` + `SF_PASSWORD` + `SF_CLIENT_SECRET`
- **Client-credentials**: `SF_CLIENT_SECRET` (without `SF_USERNAME`)

## Common Pitfalls and Solutions

### 1. Forgetting to Generate Code

**Problem**: Changes to design files not reflected in implementation.
**Solution**: Always run `make apigen` after modifying design files.

### 2. Zero Results From All List Endpoints

**Problem**: Every project-scoped list returns an empty array.
**Solution**: The `ProjectResolver` failed to translate the v2 project UUID to a Salesforce `Project__c.Id`. Check that the project-service NATS RPC subjects (`lfx.projects-api.get_slug`, `lfx.projects-api.slug_to_uid`) are reachable and that the project slug exists in Salesforce.

### 3. NATS Connection

**Problem**: Service fails to start due to NATS connection.
**Solution**: Ensure NATS is running and `NATS_URL` is correct.

### 4. Salesforce Authentication Failure

**Problem**: Service starts but all reads return errors; logs show `salesforce authentication failed`.
**Solution**: Verify `SF_INSTANCE_URL`, `SF_CLIENT_ID`, and the credentials for your chosen auth flow are all set correctly.

### 5. JWT Validation in Local Dev

**Problem**: Every request returns 401 Unauthorized.
**Solution**: Set `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=local-dev-user`.

## Key Implementation Details

### Service Architecture

The `membershipServicesrvc` struct in `membership_service.go` is the central service handler. It holds:
- `memberReaderOrchestrator`: Use case layer for membership business logic (`MemberReaderOrchestrator`)
- `storage`: Direct storage access (for readyz check, implements `port.MemberReader`)
- `auth`: `domain.Authenticator` for JWT validation

### JWTAuth Security Handler

The `JWTAuth` method is called automatically by Goa for all endpoints with `dsl.Security(JWTAuth)`. It:
1. Calls `auth.ParsePrincipal()` to validate and extract the principal.
2. Stores the principal in context under `constants.PrincipalContextID`.
3. Returns an error if authentication fails (results in HTTP 401).

### Error Handling

Domain errors are mapped to HTTP status codes in `cmd/member-api/service/error.go`:

- `ErrNotFound` → 404
- `ErrInternal` → 500
- `ErrServiceUnavailable` → 503
