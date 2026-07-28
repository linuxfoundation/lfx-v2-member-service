---
name: member-service-code-review
description: >
  How to judge the implementation of an lfx-v2-member-service pull request: the
  grounding technique for reading a hunk in its real context, the general
  quality dimensions (correctness, error handling, logging, tests, concurrency,
  readability, code truthfulness), how to hold the diff to the repo's documented
  standards for this Goa + NATS + Salesforce Go service, the member-service
  specifics worth a second look, and the security anchors that make a diff
  security-relevant here. Use on every PR that changes code, however small; this
  is the reviewer's line-level lens.
---

<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Member Service Code Review

Reviewer scope and the signal bar are owned by the `copilot-code-reviewer` skill
(`.github/skills/copilot-code-reviewer/SKILL.md`); this skill assumes those and
owns the line-level method. Read enough surrounding code to judge each hunk in
its real context — for a handler change, the design → handler → use case →
adapter path it sits on; for a storage or cache change, the reader and the
writer that call it and the message publishes that follow the write.

What follows states invariants, not an inventory: where it names a package,
route, bucket, or contract, the code is the authority for the shape that thing
has today.

A diff alone is not enough. For each non-trivial hunk, read the **whole changed
function**, not just the diff lines, and grep for **callers and sibling
implementations** of the same pattern to confirm the change matches how the repo
already does it. This service has several near-identical resource flows —
organizations, organization settings and their per-principal users, project
memberships, key contacts, organization workspaces and their project
associations — whose readers, writers, publishers and cache paths mirror each
other, so the nearest sibling is usually one grep away and is the fastest way to
tell a deliberate deviation from an omission. Drift from the sibling is a reason
to look harder, not a finding in itself: what you report is whatever concrete
problem the comparison turns up.

## The house standards

The repo defines its own standards; hold the diff to them, and name the
documented source in any standards finding. Read the parts relevant to the diff
before judging, every run, because the standards belong to the repo and move
with it. They live in:

- **The owned contract docs under `docs/`** — the ones whose header declares
  that they are updated in the same PR as the behavior they describe. Today that
  is `fga-contract.md` and `indexer-contract.md` for what this service emits,
  `cdc-consumer.md` for the Salesforce change-data-capture path, and
  `backfill-reindex.md` for the admin reindex and repair paths; each was written
  against the code it documents. That header is what makes a doc a reliable
  pointer target here, so look for it rather than trusting this list — `docs/`
  also holds owned docs that do not carry it.
- **`docs/agent-guidance/`** — the Salesforce integration and cache guidance:
  how records are fetched, how the two cache tiers differ, and how project
  identifiers are resolved before a query is issued. Its cache-freshness and
  resolver material is current; its bucket inventory is not —
  `salesforce-cache.md` documents a subset of the buckets declared in
  `pkg/constants/storage.go`, and `docs/service-helm-chart.md` repeats the same
  short list. Take bucket facts from the constants.
- **`.claude/skills/member-service-dev/SKILL.md`** and its `references/` — the
  repo-local development conventions: the generated-code boundary, logging, the
  domain-error family and its Goa mapping, context keys, NATS and KV rules, and
  the test conventions. Its *conventions* are current; several of its
  *inventories* (bucket lists, subject lists, endpoint lists) are not — prefer
  the constants and the design.
- **`docs/reviews/knowledge-base/`** — the empirical patterns this repo's PRs
  have actually been bitten by, organized by area, with
  `known-false-positives.md` recording what the team has already rejected. Use
  the category files as a checklist of known shapes and the false-positive file
  as a floor: a finding that matches something the team has explicitly rejected
  does not get posted, however well you can argue it. The knowledge base is a
  snapshot taken at an earlier point on `main`, so treat it as a floor of
  known-good patterns and a rejection list, never as an inventory of what
  matters — whole areas of the service postdate it.
- **`CLAUDE.md`, `README.md` and `ARCHITECTURE.md`** — good evidence of intent
  and the fastest orientation, and demonstrably drifted in their details. They
  disagree with each other and with the code about how many KV buckets exist,
  which endpoints exist, which NATS subjects exist, and what the error types are
  called; `ARCHITECTURE.md` in particular interleaves current state with a
  target design, including routes that were never shipped. Whole parts of the
  live HTTP surface appear in none of their endpoint tables, so a reviewer who
  treats one of those tables as the surface will confidently flag working code
  as out of scope. Derive the HTTP surface from `cmd/member-api/design/` read
  together with `charts/lfx-v2-member-service/templates/ruleset.yaml`, the
  buckets from `pkg/constants/storage.go`, the subjects from `pkg/constants/`
  read together with the adapter that publishes or calls each one, and the error
  mapping from the boundary mapper in `cmd/member-api/service/`.

Enforcement runs in both directions, with one distinction this repo needs. A
diff that changes behavior an owned contract doc describes without updating that
doc is a finding — doc-versus-code drift is the highest-recurrence review
finding in this repo's history, and these contracts are read by other teams.
Pre-existing drift in the general prose is a different thing: it is a reason to
distrust that prose as evidence, not a finding to file against an author who did
not touch it. When a document disagrees with the code, the code is what runs;
say so, and cite the code rather than the document. If a documented convention
is wrong for this specific change, say so explicitly and explain the trade,
rather than silently waiving or silently enforcing it.

## Quality dimensions

Run these on the changed code, scaled to the size of the change:

- **Correctness**: does it do what it claims? Watch a `context.Context` dropped
  or replaced with a background context on a request path, an error swallowed
  and turned into a `nil` return, boundary conditions on batched queries and
  key-prefix scans, and multi-step writes where a later failure leaves the
  earlier step committed.
- **Error handling**: the shape here is layered, and demanding one shape
  everywhere produces false findings. Adapters wrap and return ordinary errors;
  the use cases and the NATS handlers construct the typed domain errors in
  `pkg/errors`; a single mapper at the Goa boundary translates those into HTTP
  statuses and collapses everything it does not recognize into a generic
  internal error. So: a failure that has to be *classified* — one the boundary
  turns into a status, or that a caller distinguishes with `errors.Is` or
  `errors.As` — uses the typed domain errors, not a parallel sentinel family and
  not a bare formatted error. An ordinary wrapped error inside an adapter,
  consumed internally, is the normal shape here and is not a finding on its own.
  Wrap so the cause survives where a caller distinguishes it; matching on error
  text where a typed error exists is a finding, but where the upstream client
  offers no typed form the repo does inspect the string — one Salesforce path
  parses the response body for its error code, another matches a named code
  constant — so judge a new one against its nearest sibling rather than treating
  string inspection as a defect in itself. Raw Salesforce or NATS errors must
  not reach the client. A new domain error case the boundary mapper does
  not handle silently becomes a 500, and a status the Goa design does not
  declare for that method cannot be returned at all.
- **Logging**: structured logging through `log/slog` and this repo's logging
  package, with the context-carrying variants on request paths so trace and
  request attributes survive. Startup and wiring failures in the composition
  root deliberately abort the process instead — that is not a finding. What is a
  finding is a new log line that drops the context where its siblings carry it,
  or that emits something the *Security anchors* section says must not be
  logged.
- **Tests**: new or changed behavior has tests that assert real behavior, not
  that a mock was called. The repo's shape is table-driven tests co-located with
  the code, depending on the port interfaces in `internal/domain/port/` and
  reusing the fakes in `internal/infrastructure/mock/` rather than adding
  parallel ones. Coverage across the repo is uneven and the documented
  aspiration of a test per function is not met, so do not treat a bare absence
  of tests as a finding; missing tests on contract-bearing, cache-invalidation,
  CDC-dispatch or security-sensitive code is worth flagging.
- **Concurrency**: CDC delivery is at-least-once and the cursor is committed
  after processing, so a handler that is not safe to run twice — one that
  double-creates, double-counts, or is order-dependent — is a real defect rather
  than a theoretical one. The cache layer also refreshes stale entries on
  background goroutines, so look for a goroutine with no lifetime bound, a
  shared map or slice written from several of them, and a refresh that reuses a
  request context that is about to be cancelled. Note that CI runs the tests
  without the race detector, so nothing here is caught for you.
- **Readability and structure**: the change reads like the surrounding code and
  sits in the same layer as the flow it extends; names say what a thing is or
  does; duplicated logic that wants a shared helper is a finding when it traps
  the next editor.
- **Code truthfulness**: comments, doc-comments, and contract docs match what the
  code actually does. A stale comment on a constant, a contract doc describing a
  field the code no longer emits, or a TODO dressed as done is a finding. The PR
  description is not in scope here — "the description says X but the code does
  Y" is a class of comment the team has already rejected.

## Member-service specifics worth a second look

- **The generated-code boundary.** `gen/` is Goa output and the Salesforce
  Pub/Sub stubs are protobuf output; both are committed so ordinary builds need
  no generator. A hand-edit in either is a finding, and it is one nothing else
  will catch: CI regenerates the Goa output before it builds, so a hand edit is
  overwritten in the pipeline while the wrong bytes stay in the repository. An
  API change belongs in `cmd/member-api/design/` followed by `make apigen`, with
  the regenerated output committed alongside the design change; a design change
  with no regenerated output is a mismatch worth raising. The inverse needs a
  moment's thought first: regenerated output with no design change is expected
  when the pinned generator moves, so check whether the PR bumps it before
  treating it as a hand edit.
- **A new endpoint is four coordinated edits, not one.** The Goa design, the
  regenerated output, the handler, and a matching rule in the chart's RuleSet. A
  secured method that ships without its rule is the highest-value finding in this
  area, because authorization for this service lives entirely in that RuleSet.
  The reverse direction needs care: the chart's HTTPRoute already exposes at
  least one path prefix that has no backing method and no rule, and what an
  unmatched route does is decided by platform configuration owned outside this
  repo — so do not build a finding on "every routed path must have a rule", and
  do not assert that such a path is either unprotected or safely denied.
- **Project identifiers are not interchangeable.** The HTTP surface carries LFX
  v2 project UUIDs; Salesforce queries need a Salesforce project identifier, and
  a query keyed on a v2 UUID does not error — it silently returns zero rows.
  Project-scoped queries resolve the v2 identifier through the project resolver
  before they reach Salesforce. A new query path that skips that resolution, or
  that caches a resolution under the wrong key, produces an empty result that
  looks like a legitimate absence.
- **Most entity identifiers are Salesforce IDs with two written forms.** The
  Salesforce-backed resources carry Salesforce IDs normalized to their canonical
  long form, and the normalization helper is where that conversion belongs.
  Resources this service mints itself — the organization workspaces and their
  project associations among them — carry generated v2 UUIDs and never pass
  through it; `pkg/sfuuid`'s package comment still claims every non-project uid
  is a Salesforce ID, and the workspace write path is the counter-example, so
  read the sibling flow to see which kind of identifier a path carries before
  judging it. Where it is a Salesforce ID, normalization at the handler is
  best-effort and passes the raw value through on failure — the real validation
  gate is inside the readers, before the identifier reaches an outbound request
  path, and a new path that consumes one without passing through that gate is
  where an unvalidated value escapes.
- **Buckets and keys are production data structures.** Bucket names are
  deliberately not punctuated uniformly, and key prefixes mirror them; a diff
  that "normalizes" either silently orphans deployed data. Most keys are
  dot-delimited and at least one long-standing key space is not, so a rule about
  delimiters is not one worth enforcing — what matters is that a key derivation
  change keeps existing keys resolvable and cannot let a caller-supplied value
  contain the delimiter. Adding a TTL or an expiry to a bucket that holds
  authoritative state is data loss, not tuning; `pkg/constants/storage.go`
  records which buckets those are.
- **The cache has states, not just hits and misses.** Cached entries carry their
  own freshness alongside the bucket-level backstop, and a read serves, serves
  and refreshes in the background, or refetches synchronously depending on which
  state the entry is in; a second tier revalidates against Salesforce
  conditionally rather than on a timer. A new cached path that collapses those
  states, or that writes an entry without the freshness metadata its readers
  expect, breaks the read path in ways that only show under load.
- **Salesforce round-trips are a budgeted resource.** API consumption is
  observed from response headers and guarded: the CDC path skips work and queues
  it durably for repair when consumption is high, and the admin reindex path
  refuses and stops mid-run at its own threshold. A change that adds an
  unbatched per-record fetch to a loop, that bypasses a guard, or that drops a
  skipped record without queueing it for repair spends that budget on someone
  else's behalf. `docs/cdc-consumer.md` and `docs/backfill-reindex.md` are the
  authority for the current guards.
- **The CDC consumer is single-active by deployment, not by code.** There is no
  application-level lease: at-most-one processing rests on the consumer running
  as its own single-replica deployment with a non-overlapping update strategy,
  because the replay cursor is one unsharded value. A change that raises the
  replica count, switches the strategy, or introduces a second processor breaks
  that invariant with no local symptom. In the same area, change types are
  matched by exact equality rather than by suffix, deliberately, because one
  change type ends with the same word as another and must route the other way.
- **Write-path publish policy is deliberate.** Creates and updates publish
  without waiting and log rather than fail when the publish does not land, with
  the admin reindex path as the recovery route; deletes propagate the failure.
  Do not raise the fire-and-forget itself — that is settled. A publish that
  swallows the error with no log at all is still a real finding, because it
  removes the only signal the recovery path has. Ordering between the emissions
  on a settings write is also load-bearing rather than incidental; take it from
  the sibling flow — the settings writer's own publish helper in
  `internal/service/` publishes the FGA message before the indexer message and
  says why — and treat an inversion as a finding.
- **Optimistic concurrency.** Mutating paths are conditional: a caller-supplied
  precondition is compared against a service-computed version and a stale value
  is refused, and at least one path uses the store's own revision for a
  compare-and-set with a different refusal status. Which shape a given endpoint
  carries — and whether it carries one at all, since creation paths generally do
  not — is a per-endpoint decision recorded in the Goa design and the use case;
  read the sibling endpoint before concluding a new one is missing a guard. What
  is always a finding is a read-modify-write that drops the version it read, or a
  conflict that never reaches the caller as one.
- **Subjects, buckets, headers and context keys have one name each, not one
  home.** Bucket names, headers and the context keys — typed rather than bare
  strings — live in `pkg/constants`. Subjects are split: the ones this service
  owns are in `pkg/constants`, some come from the peer service's own module, and
  the project-service RPC subjects are named in the adapter that calls them,
  as are the KV key prefixes. What bites is an unnamed literal at a call site,
  because that is how a rename becomes a silent production break — so judge
  where a new name belongs against how its nearest sibling gets its name, not
  against a single required location.
- **Chart and code move together.** The chart declares the Heimdall RuleSet, the
  KV buckets, the deployment shape and the environment the service reads. A new
  endpoint whose route has no rule is unauthorized or unreachable in a cluster; a
  new bucket or environment variable the chart does not create is a runtime
  failure. No check in CI reasons about what this chart declares, and the
  coupling is invisible in the Go diff, which is exactly why it gets missed.
  Deployed values, the Salesforce secret, and the platform-wide OpenFGA model
  live in other repos; the chart consumes them and does not define them.
- **Fail-closed defaults are load-bearing.** Some chart values default to a
  sentinel or to disabled precisely so an unconfigured deployment refuses rather
  than grants — an administrative team identifier that can never match a real
  one, a consumer that ships switched off. Their existence is intentional and is
  not a finding; a diff that replaces one with an empty string, a real-looking
  value, or an enabled default outside a deliberate configuration change is.
- **Load-bearing constants.** A changed constant is a behavior change even when the
  code compiles: timeouts, cache freshness windows, quota thresholds, batch
  sizes, retry counts, subject and bucket names, audience and issuer defaults.
  When the diff moves one, ask what its blast radius is and whether the change
  is intentional. The finding is a blast radius the change does not account for,
  not the absence of a sentence explaining it — a correct new value needs no
  rationale to be correct.

## Security anchors

These are the boundaries that make a diff security-relevant in this service.
They describe its shape, not its current line-level guards; verify the concrete
mechanism in the code each time, and only report what you can trace. If you
cannot trace a path from attacker-controlled input to a sensitive sink, it is
not a reportable security finding.

- **Secrets in the diff.** A hardcoded credential — one that would actually
  authenticate somewhere — is a finding wherever it appears, including tests,
  fixtures, chart values, and workflow files, and even when the code path that
  reads it is dead. Obvious placeholders and sentinels are not: this repo's
  tests carry values like `fake-token-for-tests`, and `pkg/constants` defines a
  fixed service-account bearer string that is an identifier rather than a
  secret. The question is whether the value grants access, not whether it is
  shaped like a token. Salesforce credentials reach this service only as
  environment variables sourced from a secret the chart does not create; a diff
  that inlines one, logs one, or moves one into a values file is a finding.
- **Authorization is not in the Go code.** Coarse and fine-grained access are
  both decided by the Heimdall rules in this repo's chart, which template the
  authorized object out of the request path. Finding no permission check in a new
  handler tells you nothing and must not be reported as an unprotected endpoint.
  What is a finding is a secured method with no rule, a rule loosened so an
  unauthenticated caller reaches a handler that does not itself establish
  identity, or a rule whose object or relation does not match the resource the
  handler actually touches. The relation a route requires is not uniform across
  routes; read the sibling rule rather than assuming.
- **What the in-process JWT check proves.** The token the service validates is
  the one Heimdall mints, not the caller's identity-provider token: verifying it
  proves the request traversed Heimdall and yields a principal claim, and the
  service refuses a token without one. Do not describe it as validating the
  user's login. Two switches deliberately weaken this for local development — one
  that collapses the chart's authorization checks and one that substitutes a
  fixed principal for token validation. Their existence in the templates is
  intentional; a diff that changes their defaults, sets them in a values file, or
  widens what they reach turns off a guard for a deployed cluster, and that is
  worth saying plainly.
- **SOQL is assembled by escaping, not by parameter binding.** There is no
  parameterized-query facility: externally-sourced values are escaped by one of
  the query helpers in the Salesforce adapter before they are interpolated into
  a query template, and a new interpolation that skips them is an injection
  finding. Three details are easy to get wrong in the other direction and are
  not defects: a pattern escaped for a `LIKE` clause must not be escaped again by
  the general helper, date-time literals are deliberately emitted unquoted
  because SOQL rejects them quoted, and an empty set renders as a valid
  always-false predicate rather than an error. Ground a finding here in the
  helper the sibling query uses; do not claim injection is structurally
  impossible in this service, because nothing enforces the convention.
- **Outbound envelopes carry the caller's bearer token.** The messages this
  service publishes to the indexer and to fga-sync propagate the incoming
  authorization header so downstream services act as the caller. Anything that
  logs, persists, or echoes a whole envelope leaks a live token; the publisher's
  current habit of logging only subject and size is load-bearing, not incidental.
- **PII in logs and errors.** Member and contact emails, names, usernames and
  principals are PII, and this service handles them constantly — one resource
  even carries an email in the URL path, so it reaches request lines and access
  logs by design. A redaction helper exists and is applied by hand; it is not
  automatic, and some existing sites log raw addresses. Those are known drift,
  not a template to copy: a *new* log or error that emits a raw email, name,
  principal, or credential is a finding, and error strings returned to clients
  count, since an error that echoes an address leaks it just as effectively.
- **Existence masking on nested resources.** A nested resource is authorized
  against its parent, so the code re-verifies that the child actually belongs to
  the parent named in the path and answers a mismatch as "not found" rather than
  "forbidden", to avoid confirming that a record exists. That re-check does not
  live at a uniform layer — some flows do it in the handler, others in the use
  case — so a rule about where it belongs is wrong in one direction or the other.
  What must hold is that every nested read or mutation has one somewhere on its
  path, and that it does not leak existence.
- **NATS handlers are unauthenticated by design.** The request/reply subjects
  this service answers extract no principal and perform no authorization; they
  trust the bus. Asking for authorization there is unactionable. The rule that
  bites is narrower and real: a handler must not return data that the HTTP
  surface gates behind an authorization relation, so a diff that widens a reply
  payload deserves the question of what the HTTP path would have required to
  return the same thing.
- **What the response exposes.** When the diff adds or changes a field on a
  response, ask whose data it is and which rule gates it. Server-derived fields
  should not be client-writable, and a newer or less travelled read path that
  returns the same data behind a weaker rule than its sibling path is the
  highest-value finding in this area.
- **Cross-service trust.** Replies from other services' subjects, and the events
  this service consumes, are untrusted input too. A reply parsed without checking
  the error shape, or an upstream failure mapped to a validation error so it
  reads to the caller as their fault, hides real outages and can invert an
  authorization outcome.
- **Deployment posture in the chart.** A chart diff that widens exposure — a new
  route, a broadened rule, a relaxed pod security setting, a port or probe that
  reaches something previously internal — is in scope, and no check in CI
  reasons about what it exposes. The chart's *existing* posture is not a finding
  to file against an author who did not touch it.

## What not to flag

- Anything the deterministic pipeline owns: lint nits, import ordering, license
  headers on the file types it scans, anything the compiler catches. Formatting
  is likewise not a finding, though not because CI checks it.
- The chart version and appVersion. They are placeholders replaced at release
  time from the git tag and carry an in-file instruction not to increment them;
  "bump the chart version" is always wrong here.
- Anything recorded in `docs/reviews/knowledge-base/known-false-positives.md` —
  it is a floor that overrides any other match. It covers, among others, the
  fire-and-forget publish policy on the write path, the debug variables endpoint
  being unauthenticated and served as plain text, self-heal creates that skip
  publication, the whole-object hash used as a version token, and a set of
  validation items the team has deferred to specific tickets. Read the file
  rather than trusting this summary of it.
- Bucket counts, endpoint tables, and subject lists in the general prose that
  disagree with the code, on a PR that did not touch them. Use the code and move
  on; the drift matters when *this* change creates it in an owned contract doc.
- Denial of service, resource exhaustion, or "add rate limiting" raised in the
  abstract, and race or timing issues you cannot trace to a concrete path. A
  traced defect — a goroutine outliving its context, a shared map written from
  several of them — belongs under the concurrency dimension above.
- Outdated third-party dependencies; a *new* dependency's risk belongs to the
  architecture lens instead.
- Advice that could be pasted into any review with no defect behind it — a bare
  "add a nil check", "add a test", "rename this", or "extract a helper".
- Unguessability as authorization, in either direction: an authorization finding
  rests on a missing server-side rule, never on whether an identifier can be
  guessed — but validating an identifier's format against the contract that
  defines it remains a legitimate correctness concern.

## Judgment calls

- **Point at the working pattern.** When the diff violates a pattern, cite the
  sibling resource in this repo that does it correctly rather than describing an
  abstract ideal.
- **Do not propose rewrites of a sound approach**, and do not suggest change for
  its own sake; working, readable code needs no improvement.
- **Know your limits.** Distinguish "this is wrong" from "this might be a problem
  depending on context"; only the first is worth an author's attention. When a
  judgment depends on something you cannot see — the OpenFGA model, the Heimdall
  platform defaults, a deployed chart value, a peer service's payload, the
  Salesforce org's own configuration — you cannot confirm it, so say nothing: do
  not assert the defect, and do not ask the author to verify it for you.
