---
name: member-service-security-review
description: >
  Security and privacy review for lfx-v2-member-service pull requests. Use when
  a PR touches a handler, the Goa design, auth, a KV bucket or cache path, the
  Salesforce adapters, an emitted indexer or FGA message, a NATS subject, an
  outbound email, logging, an error path, config or the chart — and, always and
  regardless of what else the diff touches, when it adds or changes ANY
  literal value that could describe a real person: test data, a fixture, a
  mock or seed record, a Goa `dsl.Example()`, a chart value, a doc or contract
  example, a code comment, or a generated artifact. A test-only, docs-only or
  generated-only diff is in scope on those grounds alone. Applies a diff-aware,
  high-confidence, low-false-positive methodology (adapted from Anthropic's
  claude-code-security-review) to this service's durable threat anchors, plus a
  personal-data pass whose confidence rules deliberately differ from the rest of
  the review. Discovers the concrete guards from the code at review time; this
  skill carries the method, not an inventory.
---

<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Member Service Security Review

This service is the member-facing read and write path over Salesforce, and the
records it handles are **about people**: key contacts carry a first name, a last
name and an email; organization settings carry an email, an LFID username and an
avatar for every principal; one route carries an email address **in the URL
path**. It publishes those facts onward to the search index and to the
authorization system, and it holds some of them in NATS buckets that carry no
TTL and have no upstream to rebuild them from. Authorization is decided entirely
by the Heimdall RuleSet in this repo's chart, not by Go code. Those facts set the
stakes for every judgment here.

Reviewer scope and the general signal bar are owned by `copilot-code-reviewer`
(`.github/skills/copilot-code-reviewer/SKILL.md`); the line-level method is owned
by `member-service-code-review`
(`.github/skills/member-service-code-review/SKILL.md`). This skill owns the
security anchors and the personal-data pass. Where the *Personal data in this
diff* section below conflicts with a rule in either of those files, **this
section wins** — that is the whole reason it exists.

## Methodology

Run a focused, **diff-aware** review, not a whole-repo audit:

1. **Only new risk.** Assess what this PR introduces or weakens. Do not
   relitigate pre-existing issues the diff does not touch.
2. **Assume hostile input, report only what is real.** Flag only
   high-confidence, concretely exploitable findings: if you cannot trace a path
   from an attacker-controlled input to a sensitive sink, it is not a reportable
   *security* finding. **This rule does not reach personal data.** A real
   person's data committed to this repository has no attacker and no sink — it
   is already published by the act of merging — and the *Personal data in this
   diff* section governs it instead. Do not let a dataflow test silence it.
3. **Three passes.**
   - *Context*: discover, from the code and the repo docs at review time, the
     guards this service relies on around the diff (the SOQL quoting helpers,
     `pkg/redaction`, the boundary error mapper, the chart's RuleSet, the
     conditional-GET cache envelopes, revision-conditional KV writes). Never
     assume a guard exists; find it.
   - *Comparative*: does the change deviate from the guard patterns the
     surrounding code establishes? This service has several near-identical
     resource flows, so the nearest sibling is usually one grep away.
   - *Assessment*: trace each input to its sink and confirm a guard sits on the
     path the data actually takes, not three functions away.
4. **Confidence-gate every finding** (1-10, report only >= 8, matching the
   >=80% gate in the **High confidence only** bullet of
   `.github/skills/copilot-code-reviewer/SKILL.md`) — **except** the
   "is this a real person or a synthetic placeholder?" judgment, which is
   explicitly exempted below and must be raised while unsure.
5. **Evidence, not vibes.** Each finding names the file and function, what the
   attacker controls, the boundary crossed, the concrete impact, and the fix.

## Personal data in this diff

**This repository is public and every commit is permanent.** Real personal data
committed here is a privacy incident, not a test detail.

**Treat as personal data (PII):** names; email addresses (personal, corporate,
LFID-linked); phone numbers; postal addresses; government or national IDs;
financial data and person-linked invoice/subscription/order/membership IDs;
authentication material (passwords, API keys, JWTs, session cookies, MFA seeds,
private keys); precise geolocation and raw client IPs (LFX treats every raw
client IP as personal data); photo, avatar or signature images of an individual;
date of birth and other uniquely identifying dates; biometric identifiers (GDPR
Art. 9 — no exception); health information (GDPR Art. 9 / HIPAA — no exception);
and linked pseudonyms — LFID, GitHub/Discord/Slack handles, Auth0 `sub`,
Snowflake login, user/member/persona UID.

**Not PII — do not flag:** aggregated counts; structural IDs (project slug,
meeting UID, committee UID); role aliases not tied to an individual (`conduct@`,
`events@`, `support@`, `security@`); DCO `Signed-off-by:` and `Co-authored-by:`
trailers, and CODE_OF_CONDUCT / SECURITY.md contact addresses, which are
consented publication; and any diff that REMOVES personal data — a deletion is
the fix, never a finding.

**Where it lands does not change the severity.** A real person's data is
Critical whether it appears in a log line, an error string, a response field, a
cache key, a persisted row, a message payload, an outbound request body, a
model prompt, a generated artifact, a chart value, a doc, a code comment, or a
test fixture. Test-only, docs-only, example-only and generated-artifact-only
diffs are fully in scope. Any "we do not flag test files, Markdown or docs"
carve-out elsewhere in this reviewer's instructions does not apply to personal
data.

**Severity gradient — precision matters, or this gate loses trust.** A real
named individual is Critical. A synthetic local part on a real mail domain is a
low-severity defect (use a reserved domain), not a leak. A role alias is not a
finding at all.

**Signal discipline is overridden for this one judgment.** The reviewer's
high-confidence / prefer-silence floor does NOT apply to "is this a real person
or a synthetic placeholder?" — that question is inherently sub-threshold, and
suppressing it is exactly how real addresses reached `main`. When you are
UNSURE, raise it as Critical/High, say plainly that you are unsure, and name
what would resolve it. Do NOT suppress it, do NOT file it as a suppressed
comment, and do NOT fold it into a summary or overview. Treat an address as
real unless the local part is clearly synthetic AND the domain is reserved
(`example.com`, `example.org`, `*.example`, `.test`, `.invalid`).

**Never reproduce the value.** Report by category and location only — "a real
corporate email address at `path/to/file.ts:42`". Replace any quoted value with
`<redacted>`. Putting the value in a PR comment leaks it a second time.

## Personal data in this service

Everything above is shared verbatim with the other LFX repositories that
carry it, and is maintained as one text — do not edit it here. What follows is
specific to `lfx-v2-member-service`: where this repo's personal data actually
lives, and which of this repo's own rules would otherwise silence a finding
about it.

**One reading note before that, because the block compresses two rules that look
like they collide.** *Severity gradient* calls a synthetic local part on a real
mail domain a low-severity defect; *Signal discipline is overridden* says to
treat an address as real unless the local part is clearly synthetic **and** the
domain is reserved. They govern different steps, and read in order they agree:

- **Is the local part clearly synthetic?** If no, you cannot tell whether this is
  a person — raise it at Critical/High and say you are unsure. That is the
  override, and it is the whole point of it.
- **If yes**, you have already answered the person question, and the gradient
  takes over: on a reserved domain there is nothing to say at all; on a real mail
  domain it is a low-severity defect, worth one comment asking for a reserved
  domain.

So the "and the domain is reserved" half decides whether you can stay *silent*,
not whether something is Critical. A clearly synthetic local part never reaches
Critical on the strength of its domain alone.

**One correction to the block's reserved list, on a point of fact.** It names
`example.com`, `example.org`, `*.example`, `.test` and `.invalid`, and omits
`example.net`, which RFC 2606 §3 reserves alongside the other two `example.`
domains. Treat `example.net` as reserved. Applied literally the list would send
a synthetic local part on a genuinely reserved domain to the `[nit]` "use a
reserved domain" comment — a false positive of exactly the kind the gradient
exists to prevent. The omission is being fixed in the shared text; until it is,
this sentence governs here.

**This section is preventive.** As of writing there is no real person's data on
`main`: a case-insensitive census finds no LF-corporate, contractor, or major
consumer mail domain anywhere in the tree, and every email literal has a
synthetic local part. Be precise about what that does *not* claim — a couple of
dozen of those literals sit on placeholder-style domains that are real
registrations rather than reserved ones (`acme.com`, `company.com`, `x.com`,
across a handful of `internal/service/` tests, `ARCHITECTURE.md`, and a doc
comment in `pkg/redaction/redaction.go`). By the gradient those are the
low-severity band, not nothing, and they are known and accepted rather than a
remediation item. Do not read this section as a list of defects, and do not open
findings against them; it exists so the first *real* one does not merge.

### Five rules in this repo would otherwise bury a personal-data finding

Clear all five. Each one silences the class on its own, so clearing four leaves
it suppressed.

1. **The dataflow framing.** The *Durable threat anchors* section below opens by
   saying that an untraceable path from attacker-controlled input to a sensitive
   sink is not a reportable security finding. Committed personal data has neither.
   It is still a finding — see *Where it lands does not change the severity*.
2. **The "does it authenticate?" test.** The *Secrets in the diff* anchor asks
   whether a value grants access, not whether it is shaped like a token. A real
   person's name or address authenticates nowhere and is a finding anyway. That
   test scopes *credentials*; it does not scope personal data.
3. **The confidence floor**, in the **High confidence only** bullet of
   `copilot-code-reviewer/SKILL.md` — "Comment only when you have HIGH
   CONFIDENCE (>=80%)", and, two sentences later in the same bullet, "prefer
   silence over a speculative or hedged comment". Both are overridden above for
   the real-person judgment, and only for it.
4. **"Never duplicate the deterministic pipeline"** — the bullet of that name in
   `copilot-code-reviewer/SKILL.md`. The secret scanning it refers to does
   **not** cover this: `.gitleaks.toml:10-20` allowlists `.*_test\.go$` and
   `^gen/.*\.go$` outright, and gitleaks detects credentials, not people. So
   every Go test file and every generated Go file in this repo is invisible to
   it, and no check anywhere looks for a person's name or address. "CI has it"
   is never a reason to stay silent here.

5. **The unreadable-surface rule** — the silence clause in
   `copilot-code-reviewer/SKILL.md`'s **Your knowledge sources** section, and
   **Know your limits** in `member-service-code-review/SKILL.md`. Both say that a
   finding you cannot confirm must not be raised at all — neither as a defect
   nor as a question put to the author to resolve on your behalf. This one
   silences the class *independently of the floor*, and it forbids the remedy the
   override prescribes: whether a committed literal belongs to a real person is
   never resolvable from any repository, so "raise it and say you are unsure"
   is exactly the hedged question that clause bars. Both files now carve it
   out, and so does this skill's own per-fact step 4 — the rule governs
   unreadable *system* facts, not the identity of a value in the diff.

### Where personal data lives in this repo

Named so you recognise a sink when the diff touches one — not as an inventory to
verify against. The code is the authority for the shape any of these has today.

- **The HTTP contract.** `cmd/member-api/design/type.go` and
  `cmd/member-api/design/membership.go`: key-contact first name, last name and
  email; organization-settings principals carrying email, LFID username and
  avatar URL.
- **Email as a path parameter.** `cmd/member-api/design/membership.go:347` and
  `:401` put a principal's email address in the URL of the settings-user PUT and
  DELETE. It therefore reaches request lines, access logs and proxy logs by
  design. That is a settled decision, not a finding — but it means anything that
  logs or echoes a request line in those flows is republishing an address.
- **Generated fan-out, served to anonymous callers.** A single `dsl.Example()`
  in the design replicates into six committed files —
  `gen/http/openapi.json`, `gen/http/openapi.yaml`, `gen/http/openapi3.json`,
  `gen/http/openapi3.yaml`, `gen/http/cli/membership/cli.go` and
  `gen/http/membership_service/client/cli.go`. The occurrence count varies with
  the value — one sampled example produced 99 across those six files, others
  produce more — but the six-file fan-out itself is invariant. Those documents
  are not merely committed:
  `cmd/member-api/kodata/gen/http/openapi{,3}.{json,yaml}` are tracked symlinks
  into `gen/http/`, `Dockerfile:37,40` copies that tree into the image's
  `KO_DATA_PATH`, `charts/lfx-v2-member-service/templates/httproute.yaml:37-39`
  forwards `/_memberships/`, and
  `charts/lfx-v2-member-service/templates/ruleset.yaml:11-28` serves
  `/_memberships/openapi{,3}.{json,yaml}` with `anonymous_authenticator` and
  `allow_all`. A personal value in a design example is therefore committed six
  times **and answered to any anonymous caller by the running service**. The
  severity of the value still follows the gradient — a synthetic local part
  stays low — but treat the *reach* as maximal, and review a design-file diff
  for example values even when the generated diff is too large to read.
- **Domain models.** `internal/domain/model/key_contact.go`,
  `b2b_org_settings.go`, `b2b_org.go`, `workspace.go`, `member.go`.
- **Authoritative, no-TTL persistence.** The `org-settings` bucket
  (`pkg/constants/storage.go:22-25`) stores emails, usernames and avatars, and
  the `key-contact-grants` bucket (`:69-80`) stores a granted username per key
  contact. Neither carries a TTL and neither has an upstream to rebuild it from,
  so a write there is durable retention of personal data, not a cache entry that
  ages out. Distinguish them from the evictable Salesforce caches before
  reasoning about what a change retains.
- **The indexer contract, including its tag space.** `docs/indexer-contract.md`
  puts `first_name`, `last_name`, `email`, `emails[]` and `username` into the
  key-contact document (`:199-225`) and into `fulltext`, `name_and_aliases` and
  `sort_name` (`:250-259`); the org-settings document carries a `members[]`
  array of username, email, name and avatar (`:284-306`). The tag space is the
  part that is easy to miss: `:307-322` emits `writer:{username}`,
  `auditor:{username}` and `member:{username}` tags, and `:323-355` documents
  query patterns that enumerate by them. A linked pseudonym in a tag is not just
  stored — it becomes queryable. A diff that widens a document's fields or its
  tags widens what a search caller can retrieve about a person.
- **FGA relations carry raw LFIDs** as subject values in the messages published
  on `lfx.fga-sync.*`.
- **A plain-text email over NATS.** `pkg/constants/subjects.go:26-28`: the
  `lfx.auth-service.email_to_username` request body *is* the address, in the
  clear.
- **The caller's bearer in every outbound envelope.**
  `internal/domain/model/member_message.go:37-50` copies the incoming
  authorization header into the message headers, falling back to the service
  account. Anything that logs, persists or echoes a whole envelope leaks a live
  token.
- **Outbound transactional email**, in
  `internal/infrastructure/email/org_role_assigned.go` and the invite path.
- **Logging, redacted by hand.** `pkg/redaction` offers `Redact` and
  `RedactEmail` (which keeps the domain and masks the local part), and it is
  applied at only a handful of call sites. There is known unredacted drift — for
  example `internal/infrastructure/salesforce/contact_repo.go:82-121` logs a raw
  email, first name and last name and interpolates the raw address into three
  error strings. That drift is **not** a finding against an author who did not
  touch it, and it is not a pattern to copy: a *new* site that emits a raw
  address, name or username is a finding.
- **Errors, where the type decides escape.**
  `cmd/member-api/service/error.go:18-58`
  passes the error through to the caller for every classified domain error
  (`NotFound`, `Validation`, `Conflict`, `ServiceUnavailable`,
  `PreconditionFailed`, `NotImplemented`); only the unclassified default at
  `:56-57` collapses to a generic message. So whether an error string containing
  an address reaches the client is decided by which `pkgerrors` type wraps it.
  An error message built from personal data and returned as a classified domain
  error is an exposure, not just an untidy log line.
- **Mock records that ship in the production binary.**
  `internal/infrastructure/mock/`
  is **not** test-only. `cmd/member-api/service/providers.go:25` imports it from
  non-test code and selects it at runtime on `REPOSITORY_SOURCE`, so it compiles
  into the shipped binary; `mock/membership.go` seeds records with
  `FirstName`, `LastName` and `Email`. A `.go` file with no `_test` suffix reads
  as production code to every reviewer, and it is this repo's largest
  person-shaped fixture surface. Treat `internal/infrastructure/mock/*.go` and
  `internal/infrastructure/auth/mock.go` as fixtures for the purpose of this
  pass, and as production code for every other purpose.
- **Test literals, everywhere and inline.** There is no `testdata/` directory,
  no golden files and no JSON or YAML fixture tree in this repo — test data is
  inline Go literals across the `*_test.go` files. Do not route this pass on a
  fixture *path*; a directory-shaped rule returns clean here for entirely the
  wrong reason. The containers that actually matter are: `**/*_test.go`,
  `internal/infrastructure/mock/*.go`, `internal/infrastructure/auth/mock.go`,
  `cmd/member-api/design/{membership,type}.go`,
  `gen/http/openapi{,3}.{json,yaml}`, `gen/http/cli/membership/cli.go`,
  `gen/http/membership_service/client/cli.go`, `charts/**/values.yaml` and
  `docs/**/*.md`.

### The authoring habit that produces this defect

`internal/infrastructure/salesforce/b2b_org_reader_test.go:25-46` is a
fully-populated Salesforce `Account` sObject response body captured from a live
query, and `:21-23` records a real Account SFID with a comment saying so.

**It is not a finding.** An `Account` is an organization: its fields are a
company name, a switchboard number, a website, an industry and a domain, and
this particular one is the repository owner's own organization. Do not flag it, and
do not ask for it to be replaced.

Cite it instead as the *habit*: a fixture built by pasting a live API response.
On an `Account` that is harmless. Run the same habit against a `Contact`, a
`Project_Role__c`, or an org-settings principal — objects whose fields are a
person — and it produces exactly the incident this pass exists to prevent. When
a diff adds a fixture that looks captured rather than hand-written, and the
object it describes is a person, that is the moment to ask.

### Do not flag, specifically here

- **`B2BOrg.Phone`** (`internal/domain/model/b2b_org.go:27-28`, `:140-141`) is
  `Account.Phone` — an organization's switchboard number, not a person's phone.
  It is not PII.
- **Membership pricing, tiers and amounts** are org-linked commercial data, not
  personal financial data. The PII taxonomy's "financial data" entry means
  person-linked records.
- **The opaque principal identifier in structured logs.** The repo's logging
  standard asks for it as a stable field and established call sites emit it; it
  is how a request is traced without naming a person. (Note the tension with the
  taxonomy above, which counts an LFID as a linked pseudonym: a principal in a
  structured log field is settled practice here and not a finding, while an LFID
  newly placed into a *published* surface — an index document, a tag, a response
  body, a doc — is judged on its merits.)
- **Organization domains.** A company's mail or web domain appearing as an
  organization attribute is not a personal address, and several Salesforce tests
  legitimately carry real ones.

### What this lens does not reach

Raising a finding is not the same as routing a PR to a human. This repo's
escalation surface (`.github/skills/escalation-guidelines/SKILL.md`) frames
personal data as a *runtime emission* — something that could reach logs, traces,
indexed documents or error responses. A fixture, a design example, a chart value
or a doc that commits a real person's data emits through none of those; it is
published by the merge itself. So a personal-data finding raised under this skill
is **not**, today, a thing the needs-human gate is guaranteed to escalate on that
ground.

That is a known gap, recorded here so it is not read as an oversight by the next
auditor. Closing it changes what escalates to a human, which is the gate owner's
decision and not this skill's to make. Until it is closed, weight the inline
finding accordingly: it is the only signal in the pipeline that carries this
class.

### Audit-log exception

The estate's data-privacy reference allows a narrow exception for writing
personal data to a dedicated audit sink under stated conditions. **It is not
satisfiable in this service**: there is no dedicated audit log here. Do not
accept "it is for audit" as a justification for a new personal-data write on
this repo, and do not propose building an audit sink as the fix for a finding.

Note also what that exception is *about*. It is written for log output. The
personal data this service retains is not log output — it is persisted state in
the `org-settings` and `key-contact-grants` buckets, which is a different
question with a different answer, and the logging exception does not govern it.

## Per-fact data-exposure pass

When the diff adds or changes a field on a response payload, on an emitted
indexer or FGA message, on a NATS reply, or on a stored KV value — or adds a new
read or write path — run this structured pass on top of the methodology above:

1. **Fact inventory.** For every field the diff adds or changes, record its grain
   (per-person, per-organization, aggregate), whose data it is, and whether it is
   personal data under the taxonomy above.
2. **Gate of record, per fact.** For each protected fact, find *which code path
   actually enforces access*. In this service that is almost never Go: it is the
   route's rule in `charts/lfx-v2-member-service/templates/ruleset.yaml`, and the
   relation and object it templates out of the request path. For a nested
   resource it is also the parent-ownership re-check that answers a mismatch as
   "not found". For an indexer document it is the `IndexingConfig` access control
   the contract declares; for a NATS reply it is nothing at all — those subjects
   are unauthenticated by design.
3. **Sibling-path parity.** Find the equivalent path for the same or an analogous
   entity — the settings read versus the settings write, the HTTP read versus the
   NATS reply that returns the same record, the API delete versus the CDC delete
   handler — and compare enforcement level path by path. A newer, less-travelled
   path that is weaker than its sibling — same data, lower gate — is the single
   highest-value finding this pass exists to catch.
4. **Verdict per fact.** Enforced and matching sibling parity; a gap (no
   enforcement, or weaker than a sibling serving the same data); or unverifiable
   here because enforcement lives in a repo you cannot read — the OpenFGA model,
   the Heimdall platform defaults, the indexer's own access evaluation. Where the
   *enforcement* is unverifiable, the silence rule in `copilot-code-reviewer`'s
   *Your knowledge sources* applies: do not raise it at all. That rule reaches
   this step's access question only. It does **not** reach whether a value is a
   real person's data — that question is never resolvable from any repository, so
   silencing it for unverifiability would silence it permanently, and both that
   skill and this one carve it out explicitly.

Skip this pass only when the diff adds no field to any of those surfaces and no
new or changed read/write path.

## Durable threat anchors

These are the boundaries that make a diff security-relevant in this service.
They describe its shape, not its current line-level guards; verify the concrete
mechanism in the code each time, and only report what you can trace. If you
cannot trace a path from attacker-controlled input to a sensitive sink, it is
not a reportable security finding — **except** for personal data, which the
*Personal data in this diff* section governs on its own terms.

- **Secrets in the diff.** A hardcoded credential — one that would actually
  authenticate somewhere — is a finding wherever it appears, including tests,
  fixtures, chart values, and workflow files, and even when the code path that
  reads it is dead. Obvious placeholders and sentinels are not: this repo's
  tests carry values like `fake-token-for-tests`, and `pkg/constants` defines a
  fixed service-account bearer string that is an identifier rather than a
  secret. The question is whether the value grants access, not whether it is
  shaped like a token — a test that scopes *credentials* only, and never
  personal data. Salesforce credentials reach this service only as
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
- **PII in logs and errors.** Member and contact emails, names and usernames
  identify people, and this service handles them constantly — one resource even
  carries an email in the URL path, so it reaches request lines and access logs
  by design. A redaction helper exists and is applied by hand; it is not
  automatic, and some existing sites log raw addresses. Those are known drift,
  not a template to copy: a *new* log or error that emits a raw email, personal
  name, or credential is a finding, and error strings returned to clients count,
  since an error that echoes an address leaks it just as effectively. The
  opaque principal identifier is the deliberate exception — the repo's logging
  standard asks for it as a stable structured field, and established call sites
  emit it; it is how a request is traced without naming a person.
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

- Denial of service, resource exhaustion, or "add rate limiting" raised in the
  abstract, and race or timing issues you cannot trace to a concrete path.
- Mere lack of hardening or defense-in-depth with no concrete vulnerability.
- Outdated third-party dependencies; a *new* dependency's risk belongs to the
  architecture lens instead.
- Log spoofing, regex-DoS, and missing audit logs.
- SSRF that only controls a path; it counts when the attacker controls host or
  protocol.
- Unguessability as authorization, in either direction: an authorization finding
  rests on a missing server-side rule, never on whether an identifier can be
  guessed — but validating an identifier's format against the contract that
  defines it remains a legitimate correctness concern.
- Anything recorded in `docs/reviews/knowledge-base/known-false-positives.md`,
  read as the file itself states it rather than as any summary of it.
- Anything the deterministic pipeline owns — with the exception named above:
  the pipeline does **not** cover personal data, so it never justifies silence
  on that class.

**This list does not exclude test files, Markdown, docs, or generated
artifacts.** No entry here reaches personal data.

## Reporting

For each finding give the file and function, what the attacker controls, the
boundary crossed, the concrete impact on this service (whose data, which
relation, what an unauthenticated caller gains), and the fix. If the diff does
not touch an anchor above, do not invent a finding for it.

For a personal-data finding, the shape is different and is set by the *Personal
data in this diff* section: give the **category and location only**, never the
value; state the severity from the gradient, using the posting vocabulary
`copilot-code-reviewer` defines — a real named individual is `[critical]`; an
unresolved "is this a person?" is `[critical]` or `[high]`; and the gradient's
low-severity band (a clearly synthetic local part on a non-reserved domain) is
`[nit]`, which by design does not block (the conductor gives an
*unaddressed* nit no row at all; an addressed one is still recorded as
`fixed`). Say plainly in such a comment that it is a hygiene fix, so the
non-blocking severity reads as deliberate rather than as a downgrade; and
where you are
unsure whether a value describes a real person, say so explicitly and name what
would resolve it rather than resolving it toward silence. Such a finding is
filed as its own inline comment and never folded into the summary — this skill
does not publish anything itself, so give the finding that shape and let the
reviewer skill, which owns posting, carry it there.
