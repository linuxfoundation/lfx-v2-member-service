---
name: member-service-preflight
description: >
  Mechanical pre-PR pipeline for lfx-v2-member-service. Runs the Go-specific
  working tree, license header, formatting, lint, build, tests, protected
  file, commit verification, and change-summary checks after
  /member-service-pr-readiness has passed. Supports default validation and
  report-only dry-run mode.
allowed-tools: Bash, Read, Glob, Grep, Edit, AskUserQuestion
---

# Member Service Preflight

You are running the mechanical pre-PR pipeline for `lfx-v2-member-service`.
This repo is Go + Goa, Salesforce-backed, NATS/KV cached, and deployed by the
chart under `charts/lfx-v2-member-service/`.

Run this after `/member-service-pr-readiness` has completed. This skill does
not replace the shape check and does not run central or generic reviewer
flows. Every check here is shell-driven or file-list-driven.

## Modes

- **Default:** run the mechanical checks. `make fmt` may rewrite Go files.
  Ask before editing license headers by hand and before committing anything.
- **`--dry-run` / `report only`:** do not run mutating commands. Use
  read-only equivalents (`gofmt -l`, file scans, build/test/lint commands)
  and report what would need fixing.

Default base branch: `origin/main`. Normalize bare branch names such as
`main` to `origin/main`.

## Check 0 - Working tree status

Run:

```bash
git status
git diff --stat <base>...HEAD
git log --format="%h %s%n%b" <base>..HEAD
```

Evaluate:

- Uncommitted changes: report them clearly. Ask whether to continue with the
  dirty tree, commit first, or stash if the user is actively preparing a PR.
- No commits ahead of base: stop and ask whether this is the intended branch.
- Commit messages missing `LFXV2-`: report the missing references.
- Commits missing `Signed-off-by:`: report the missing DCO trailers.

## Check 1 - License headers

The repository enforces license headers through
`.github/workflows/license-header-check.yml`, excluding `gen/`.

For changed files, verify the repo's existing header style:

- Go files: `// Copyright The Linux Foundation and each contributor to LFX.`
  then `// SPDX-License-Identifier: MIT`
- YAML and shell-like files: `# Copyright The Linux Foundation and each contributor to LFX.`
  then `# SPDX-License-Identifier: MIT`
- Markdown and HTML-like docs: `<!-- Copyright The Linux Foundation and each contributor to LFX. -->`
  then `<!-- SPDX-License-Identifier: MIT -->`

Suggested read-only scan:

```bash
git diff --name-only <base>...HEAD
```

Check new or modified source/docs files outside `gen/`. In default mode, add
missing headers only after reading nearby files for the exact comment style.

## Check 2 - Formatting

Default mode:

```bash
make fmt
```

Dry-run mode:

```bash
gofmt -l $(find . -name '*.go' -not -path './gen/*' -not -path './vendor/*')
```

Report any files that were reformatted or would be reformatted.

## Check 3 - Lint

Run:

```bash
make lint
```

This repo installs/runs `golangci-lint` through the Makefile when needed. If
lint fails, read the output and fix only clear mechanical issues in default
mode. Report structural or behavioral issues instead of guessing.

If fixes were applied in Checks 1-3, rerun:

```bash
make lint
```

## Check 4 - Build verification

Run:

```bash
make build
```

If Goa design files under `cmd/member-api/design/` changed, first run:

```bash
make apigen
make build
```

`make apigen` writes generated files under `gen/`; never hand-edit `gen/`.

## Check 5 - Tests

Run:

```bash
make test
```

This currently runs `go test -v -race -coverprofile=coverage.out ./...`.
Report failures with package/file context. Do not hide race failures or
coverage-profile side effects.

## Check 6 - Protected files

Use this repo-local protected-path set. Keep it in sync with
`/member-service-pr-readiness`; do not substitute a central or generic list.

| Area | Protected paths |
| --- | --- |
| Salesforce integration/cache docs | `docs/agent-guidance/salesforce-integration.md`, `docs/agent-guidance/salesforce-cache.md` |
| Membership API | `cmd/member-api/**`, `gen/**` |
| Current-vs-target architecture | `ARCHITECTURE.md` |
| NATS and project-ID mapping | `.claude/skills/member-service-dev/references/nats-messaging.md`, `internal/infrastructure/nats/**`, `internal/infrastructure/project/**`, `pkg/constants/nats.go`, `pkg/constants/storage.go` |
| Charts | `charts/**` |
| Go dependencies | `go.mod`, `go.sum` |
| Build system | `Makefile` |
| Agent guidance | `CLAUDE.md`, `.claude/skills/**` |
| Service docs | `README.md`, `docs/**` |

Run:

```bash
git diff --name-only <base>...HEAD
```

Intersect changed files with the table above. For every match, report why it
is protected and what PR description or owner-review note is needed.

## Check 7 - Commit verification

Run:

```bash
git status
git log --format="%h %s%n%b" <base>..HEAD
git log --format="%G? %h %s" <base>..HEAD
```

Verify:

- All intended changes are committed, or clearly report remaining dirty files.
- Commit subjects follow conventional format:
  `type(scope): description`.
- Every commit has `Signed-off-by:`.
- Every commit is signed with an acceptable GPG/trust status (`G`, `U`, or
  `Y`).
- A relevant `LFXV2-<number>` ticket appears in the branch or commit text.

## Check 8 - Change summary

Run:

```bash
git diff --stat <base>...HEAD
git diff --name-status <base>...HEAD
```

Summarize:

1. New files created and their purpose.
2. Modified Go packages and what changed.
3. Goa design or generated-code changes.
4. Salesforce, NATS/KV, or project-ID resolver changes.
5. Helm chart or deployment changes.
6. Dependency changes in `go.mod` or `go.sum`.
7. Documentation or agent-guidance changes.

## Results report

Use this shape:

```text
PREFLIGHT RESULTS
-----------------
PASS Working tree     - Clean, N commits ahead of origin/main
PASS License headers  - All changed files have headers
PASS Formatting       - make fmt clean
PASS Lint             - make lint succeeded
PASS Build            - make build succeeded
PASS Tests            - make test succeeded
PASS Protected files  - charts/ touched; PR owner note required
PASS Commits          - Conventional, signed, signed off, LFXV2 referenced

Changes summary:
- ...

READY FOR PR
```

If checks fail:

```text
PREFLIGHT RESULTS
-----------------
PASS Working tree     - Dirty tree acknowledged
FAIL License headers  - 2 files missing headers
PASS Formatting       - make fmt clean
FAIL Lint             - golangci-lint failed in internal/...
FAIL Build            - make build failed after apigen
SKIP Tests            - skipped because build failed
WARN Protected files  - go.mod and charts/ touched
PASS Commits          - Signed, signed off, LFXV2 referenced

ISSUES FOUND - fix before submitting
```

Final verdict:

- `READY FOR PR`: all required checks pass.
- `READY WITH NOTES`: mechanical checks pass, but protected files or dirty
  tree require explicit PR notes.
- `NOT READY`: any required check fails.

If default-mode formatting or header fixes created uncommitted changes, end
by asking whether to commit them. Do not create the PR from this skill unless
the user explicitly asks.
