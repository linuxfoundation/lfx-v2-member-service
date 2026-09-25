<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Member-service review knowledge base

Empirical review patterns extracted from this repo's merged-PR review history. Each
pattern was flagged by a real reviewer (human maintainer or Copilot) on a real
member-service PR and cleared the promotion gate in the service review-KB research
playbook.

This KB is the **empirical** surface. It does NOT duplicate `/lfx-skills:lfx-general-code-review`
(generic correctness/security/test intuition plus this repo's documented rule/contract
surface, which it reads from `CLAUDE.md` and the repo's docs). It encodes the repo-specific
patterns reviewers have actually flagged here.

Consumed by `/member-service-learnings-reviewer`, which routes by changed-file
path to the category files below, matches each pattern's `**Detect:**` clause, and emits
only findings it can quote (KB-match gate), then drops anything matching
`known-false-positives.md`.

## Methodology

- **Corpus:** all 41 merged PRs (#2-#44, full history), pulled via `gh api` across three
  comment surfaces (inline `pulls/<n>/comments`, review bodies `pulls/<n>/reviews`,
  conversation `issues/<n>/comments`) plus the GraphQL `reviewThreads` resolution state for
  acted-on signal, and `gh pr diff` for file/line context.
- **Gate:** a pattern is promoted only if it clears ALL hard gates (repo-specific not
  generic; mechanically detectable + fixable; currently relevant against `origin/main`;
  not already caught by gofmt/golangci-lint/CI) AND at least one value signal (recurrence
  ≥2 PRs; cost-of-miss for security/data-integrity/contract; acted-on by a code change or
  maintainer endorsement).
- **Currently-relevant baseline:** `origin/main` at PR #44 (`7f6ca55`). The service pivoted
  twice — a v1-sync/PostgreSQL/consumer era (PRs #2-#10), removed in PR #11; then a
  Salesforce-backed proxy; then (PRs #36-#44) an FGA-sync + indexer publisher for b2b-org
  settings and key-contacts. Patterns tied to the removed era are recorded in
  `known-false-positives.md`, not promoted.

## Corpus stats

- **PRs sampled:** 41 merged (#2-#44; #15 and #31 absent — never existed/closed-unmerged).
- **Inline review comments:** 430 — Copilot 174, prabodhcs 95, emsearcy 94,
  `github-license-compliance[bot]` 33 (tooling, not signal), dealako 24, others 10.
- **Review bodies:** 66 — Copilot bot 50, human approvals/requests 16.
- **PR conversation comments:** 12 (all human).
- **Review threads (GraphQL):** 285; resolved-by-author signal — Copilot 89 resolved / 85
  unresolved, emsearcy 13/34, dealako 18/5.
- **CodeRabbit:** NOT active on this repo. Zero `coderabbitai[bot]` comments on any surface
  across all 41 PRs. Signal here is **Copilot + heavy human maintainer review** (emsearcy,
  prabodhcs, dealako). Severity weighting leans on human maintainer comments and acted-on
  threads accordingly.

## Categories

| File | Patterns | Read when |
| --- | --- | --- |
| [`salesforce-and-uuid.md`](salesforce-and-uuid.md) | 5 | `internal/infrastructure/salesforce/**`, `pkg/sfuuid/**`, `internal/infrastructure/project/**`, or any `.go` calling the `sfuuid` helpers / building SOQL / resolving project UID↔SFID |
| [`cache-and-kv.md`](cache-and-kv.md) | 5 | `internal/infrastructure/nats/**`, `pkg/constants/storage.go`, `pkg/constants/nats.go`, or a write handler that mutates cached resources |
| [`endpoint-and-goa.md`](endpoint-and-goa.md) | 6 | `cmd/member-api/design/**`, `cmd/member-api/service/**`, `internal/service/**`, or `gen/**` |
| [`fga-and-indexer.md`](fga-and-indexer.md) | 5 | `internal/service/**` or `internal/domain/model/**` building FGA/indexer messages, `pkg/constants/subjects.go`, `docs/fga-contract.md` |
| [`chart-and-deploy.md`](chart-and-deploy.md) | 5 | `charts/lfx-v2-member-service/**` |
| [`docs-and-comments-drift.md`](docs-and-comments-drift.md) | 3 | a `.go` doc-comment, `CLAUDE.md`, `README.md`, `ARCHITECTURE.md`, or `docs/**` changed alongside a behavior change |
| [`observability-and-resilience.md`](observability-and-resilience.md) | 4 | `pkg/errors/**`, `cmd/member-api/service/error.go`, `internal/infrastructure/nats/project_rpc.go`, `.../project_id_map_handler.go`, `.../client.go`, or any error map/log |
| [`known-false-positives.md`](known-false-positives.md) | 10 entries | always (applied LAST as the floor) |

**33 promoted patterns** across 7 category files, plus 10 false-positive entries.

## Highest-value patterns

- `salesforce-and-uuid/swallowed-sfid-conversion-error` (Critical) — recurred across PRs
  #23, #37, #40; a swallowed SFID-conversion error silently zeroes a foreign key.
- `endpoint-and-goa/status-flip-bypasses-capacity` and `.../missing-ifmatch-guard-parity`
  (Critical) — maintainer `[blocking]` findings (dealako, PR #37); data-integrity / lost-update.
- `cache-and-kv/write-without-cache-invalidation` (Critical) — PR #39; writes that don't
  carry the UID needed to invalidate `membership-cache`.
- `fga-and-indexer/update-access-race-without-exclude-relations` (Critical) — PR #39;
  concurrent `update_access` clobbers just-added tuples without `exclude_relations`.
- `chart-and-deploy/empty-openfga-object-id` (Critical) — PRs #11/#26/#38; the `_null`
  sentinel pattern that prevents an empty OpenFGA object.
- `docs-and-comments-drift/contract-doc-vs-code` (Important) — the single highest-recurrence
  surface on this repo (16+ PRs); flagged by all three reviewer buckets.

## Uncertainties / human calls

- `observability-and-resilience/4xx-logged-at-error` is only *partially* live: `origin/main`
  already routes NotImplemented→Debug and Conflict/Precondition→Warn, but Validation/NotFound
  still log at Error. Kept as a Nit; a maintainer should decide whether to lower them.
- The repo's former conventions-review agent (since retired) once stated in its "Known
  False Positives" that the service does NOT publish FGA/indexer and that key-contact
  mutations do not accept `If-Match`. Both became stale on `origin/main` (publishing exists;
  `If-Match` guards exist). This KB treats the current code as authoritative.

_Built 2026-05-29 against `origin/main` @ `7f6ca55` (PR #44 merged)._
