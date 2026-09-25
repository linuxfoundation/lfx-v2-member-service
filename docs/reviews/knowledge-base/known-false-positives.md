<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Known false positives — applied LAST in every review pass

Findings that match any pattern below MUST be dropped, regardless of which source (KB
pattern file, code-reviewer rule, or bot) produced them. This list is the floor — even a
quotable pattern match does not survive if it matches a known false positive.

Used by the `/member-service-learnings-reviewer` skill (Step 4) and as a
filter-discipline reference for `/lfx-skills:lfx-general-code-review`.

---

## Tooling already enforces it

### License-header / license-scan findings

**Pattern matched:** a finding that a `.go`/`.yaml`/`.md`/shell file is missing the
license header, or that a dependency carries an unrecognized license.

**Why false:** license headers are enforced by the repo's header check, and the
`github-license-compliance[bot]` runs on `go.mod`. When a new transitive dep trips it, the
fix is an allowlist entry, not a code review finding. (PR #36 `go.mod`: prabodhcs — "All
flagged packages ... carry standard MIT/BSD/Apache-2.0 licenses ... These should be added
to the allowlist in the license policy.") If the header check / scan passes, the bot
misread it.

**Source:** repo header check; `github-license-compliance[bot]` (33 inline comments across
the corpus, all tooling, none code-review signal).

### gofmt / formatting nits

**Pattern matched:** "run gofmt on this file", struct-literal alignment, import ordering.

**Why false:** `make fmt` / `gofmt` and `golangci-lint` are run by `/member-service-preflight`
and CI; surfacing a formatting-only issue in a review is duplicate signal. (Seen PR #19
`membership_service_test.go:315`.)

---

## Removed-architecture assumptions (the v1-sync era)

The service's first iteration (PRs #2-#10) had a PostgreSQL store, a `membership_syncer`
full-sync job, an `internal/consumer/` v1-objects KV consumer, and indexer-publish wiring.
**All of that was removed** (PR #11 "Remove ARCH-345 additions") and the directories
`internal/consumer/` and `internal/infrastructure/postgres/` no longer exist. Do NOT
resurrect findings against that era.

### PostgreSQL / sync-job / lookup-index findings

**Pattern matched:** any finding about `internal/infrastructure/postgres/**`, a
`membership_syncer`, "stale lookup keys never cleaned up", or full-sync deletion semantics.

**Why false:** those files were removed in PR #11. The current service is a Salesforce-backed
read/write proxy with NATS KV caches — no Postgres, no sync job. (Reads served from SOQL
+ `membership-cache`; see `docs/agent-guidance/salesforce-cache.md`.)

### "This service does not publish FGA/indexer" overcorrection

**Pattern matched:** a finding asserting the service must NOT publish FGA-sync or indexer
messages, citing CLAUDE.md's "does NOT publish FGA or indexer messages" line.

**Why false (now):** that statement was true for the read-only proxy era only (the repo's
former conventions-review agent, since retired, dropped it from its own KFP list). As
of `origin/main` (PRs #36-#44, plus the CDC consumer) the service DOES publish FGA-sync
and indexer messages for b2b-org, b2b-org settings, and key-contacts (see
`internal/service/**`, `pkg/constants/subjects.go`,
`docs/fga-contract.md`). Findings about FGA/indexer message *construction* are valid (see
`fga-and-indexer.md`). Only drop a finding that says publishing should not exist at all.

---

## Documented shape, not a defect

Salvaged from the retired conventions-review agent (2026-09-25): the two suppressions it
carried that are not restated anywhere else in the review surface.

### `REPOSITORY_SOURCE=mock` is not an offline mode

**Pattern matched:** "mock mode still dials NATS / initialises the Salesforce client at startup;
make `REPOSITORY_SOURCE=mock` fully offline", or a test/doc faulted for needing NATS under
mock mode.

**Why false:** documented behaviour. `REPOSITORY_SOURCE=mock` swaps the membership and B2B
readers for in-memory mocks, but `main.go` still initialises NATS/Salesforce for the
project-id-map RPC handler and key-contact writer wiring. Not a defect unless the change
itself claims to make mock mode offline.

**Source:** `docs/agent-guidance/salesforce-integration.md` ("Mock repository still starts
shared dependencies").

### Target Architecture in `ARCHITECTURE.md` is not current behaviour

**Pattern matched:** "does not match `ARCHITECTURE.md`" where the cited text sits under
"Target Architecture" — root `/key_contacts/{uid}` routes, "ports to remove in the target
state", target-era type or relation names.

**Why false:** `ARCHITECTURE.md` deliberately holds both "Current State" and "Target
Architecture"; the target sections describe a migration not yet shipped. Divergence from
the target is a finding only when the change explicitly implements that part of the
migration — and then the docs/contracts must move in the same change
(`docs-and-comments-drift.md`).

**Source:** `ARCHITECTURE.md` §Current State / §Target Architecture;
`.github/copilot-instructions.md` states the same caveat for PR-side review.

---

## Intentional, maintainer-endorsed design decisions

### `/debug/vars` unauthenticated

**Pattern matched:** "`debug-vars` endpoint is unauthenticated and exposes expvar; add JWT
security."

**Why false:** intentional and documented. PR #22 `cmd/member-api/design/membership.go:439` —
emsearcy: "This service sits behind the Traefik API gateway and is not directly reachable
via any public ingress ... `/debug/vars` is only accessible via `kubectl port-forward` ...
Adding JWT auth would make it harder to use in practice." `/debug/vars` is also not exposed
by the HTTPRoute.

### `/debug/vars` `text/plain` content type

**Pattern matched:** "`debug-vars` is described as JSON but returns `text/plain`; use
`application/json`."

**Why false:** intentional. PR #22 `membership.go:450` — emsearcy: with `application/json`
the Goa encoder base64-encodes the `[]byte`; `text/plain` makes the `textEncoder` write the
already-valid JSON bytes directly. A comment explaining this is in the design.

### Fire-and-forget publish on the write path

**Pattern matched:** "indexer/FGA publish uses core NATS (fire-and-forget); switch to
JetStream publish-with-ack for durability."

**Why false:** documented write-path policy. PR #36 `internal/infrastructure/nats/publisher.go:79` —
prabodhcs: "`sync=false` fire-and-forget is the write-path policy ... publish failures are
swallowed and logged ... so a backfill job can recover missed events." The recovery path is
`POST /admin/reindex`. Upgrading to publish-with-ack is a tracked follow-up, not a review
finding. (Still flag a publish that swallows the error with NO log — that's
`observability-and-resilience/swallowed-error-in-log`.)

### Self-heal create skips event publication

**Pattern matched:** "self-heal/idempotent create returns the existing record without
publishing — indexer/FGA stay stale."

**Why false (by design):** the record is already in the indexer/FGA from the original
create; re-publishing on every idempotent retry is unnecessary. PR #37
`key_contact_validation.go:50` / `membership_service.go:347` — accepted with a clarifying
comment pointing to `/admin/reindex` for the rare missed-publish recovery.

### ETag based on full-object hash (not LastModifiedDate)

**Pattern matched:** "`LFXEtag` hashes the whole object; base the ETag on
`LastModifiedDate`/`UpdatedAt` instead."

**Why false:** intentional trade-off. PR #36 `pkg/etag/etag.go:23` — prabodhcs: "full-object
hash ensures the ETag changes whenever any field changes, not just LastModifiedDate."
Timestamp-based ETag is a tracked follow-up.

---

## Deferred-by-ticket, not a fresh finding

**Pattern matched:** input-hygiene gaps the team explicitly deferred to a follow-up ticket —
email-format validation on `B2BOrgUser`, `InvitedAs` enum/cross-field validation, RFC 7232
ETag quoting (`W/` prefix, quoted values), and breaking `AssembleKeyContact` into smaller
functions.

**Why false (conditional):** these were raised (dealako/mauricio, PRs #36, #37, #42) and
explicitly deferred to follow-up tickets with the maintainers' agreement — they are not
in-scope correctness defects for a new change unless the new change is the follow-up that
closes them. If a PR claims to implement that follow-up, the finding becomes valid again.

---

## How to add a new entry

When the team explicitly decides a recurring bot/reviewer finding is not relevant for this
repo:

1. Add an entry here with **Pattern matched**, **Why false**, and (where useful) a
   **Source** (PR # + reviewer quote or doc path).
2. If the pattern previously lived in a category `*.md`, remove it there — don't keep a
   pattern in both files.
3. Re-audit this file whenever the architecture shifts. This service has already pivoted
   once (v1-sync era → Salesforce proxy → FGA/indexer publisher); KFPs tied to a past era
   must be retired when the era ends. If this file grows past ~30 entries, the KB is being
   too permissive.
