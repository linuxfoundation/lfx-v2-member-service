<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# LF Team Auditor Grants — Operator Runbook

The LF staff team — named by `LF_STAFF_TEAM_NAME`, written as `team:<staff-team>#member` throughout this document — holds the `auditor` relation on every `b2b_org`. The service asserts the grant on every full-sync publish path; the scripts in this document exist for the orgs that already existed when that behaviour shipped, and for rolling the grant back.

## When the grant starts

**The deploy is the cutover, not the backfill.** Every write path asserts the reference, so orgs begin acquiring the tuple as soon as the new build is running and CDC events arrive. The scripts below only sweep up orgs that never change on their own.

The team name is the same in every environment, so it lives as a real value in `charts/lfx-v2-member-service/values.yaml` rather than being overridden per environment in `lfx-v2-argocd`. It is the single authoritative copy: neither the service code nor the scripts hardcode it.

Rollout order:

1. Deploy the API **and** the CDC consumer. New and changed orgs start acquiring the grant from this point.
2. Run the census and grant scripts below for the existing estate.
3. Re-run the grant script with `--dry-run`; expect zero missing.

Plan step 1 deliberately. Reverting the deploy or clearing the variable afterwards stops further writes but removes nothing already written — that needs the revoke script.

**Staff only.** Contractors do not inherit staff access — contractor is its own role, tracked in [LFXV2-3071](https://linuxfoundation.atlassian.net/browse/LFXV2-3071). The scripts and the message plumbing take a list of teams, so a future role is a configuration change, but nothing here grants a contractor team by default.

See [fga-contract.md](./fga-contract.md) for the message-level contract and [LFXV2-2937](https://linuxfoundation.atlassian.net/browse/LFXV2-2937) for the change itself.

## What the grant actually confers

Read access to **all six document types this service indexes**, on every org. Two distinct mechanisms are involved, and auditing the Heimdall ruleset alone understates the reach:

| Mechanism | Reaches |
|---|---|
| The four `auditor`-gated REST routes in `ruleset.yaml` | `GET /b2b_orgs/{uid}`, `GET /b2b_orgs/{uid}/settings`, `GET /project_memberships/{uid}`, `GET /project_memberships/{membership_uid}/key_contacts/{uid}` |
| Search — every index config declares `AccessCheckRelation: auditor` | `b2b_org`, `b2b_org_settings`, `project_membership`, `key_contact`, **`workspace`**, **`workspace_project`** |

Workspaces and workspace-projects have no `auditor` REST route at all (every workspace route is `writer`-gated), but their index documents access-check `auditor` on the parent `b2b_org` by design, so they are reachable via search.

Note that `GET /b2b_orgs/{uid}/settings` exposes **pending-invite email addresses**. That route was gated on `auditor` rather than `writer` on the premise that auditors are per-org trusted principals; the blanket grant changes that premise. The `b2b_org_settings` index document carries the same `auditor` access check, so the roster is reachable through search as well as through the route — any narrowing would have to cover both.

This was reviewed and accepted rather than narrowed. [LFXV2-3026](https://linuxfoundation.atlassian.net/browse/LFXV2-3026) is Org Dash / PCC parity, and staff already reach this roster in legacy, so the grant migrates an existing disclosure rather than creating one. Narrowing the route to `writer` would also strip roster read from the per-org auditors who hold it today.

No write access anywhere. The `[user, team#member]` branch of `b2b_org.auditor` feeds nothing upward, unlike `global_org_admin`, which flows into `writer`.

## The one-way-door property

**fga-sync never deletes a tuple whose subject begins with `team:`.** Reverting the service code stops *new* grants being written; it does not remove existing ones. Setting `LF_STAFF_TEAM_NAME` to `""` behaves the same way.

That guard belongs to the **deployed** fga-sync, not to this repository's dependency pin. It was added in fga-sync `v0.3.1` — the delete branch of `SyncObjectTuples` in `fga.go` — and the platform chart deploys `~0.3.5`. This repo pins `v0.2.17` in `go.mod`, which predates the guard, but that pin supplies only the message types in `pkg/types` and `pkg/constants`; nothing here links the sync engine, so the pin has no bearing on what the running service deletes. Everything below assumes a deployed fga-sync at `v0.3.1` or later. On anything older the guard is absent, and a settings write would revoke these grants instead of preserving them.

It is also why the contractor key was deleted outright rather than blanked in `values.yaml`. A configuration opt-out only holds if every pod in every environment is deployed from the values carrying it; one stale rollout writes tuples nothing can reap. A key that does not exist cannot be populated by accident.

Removing the tuples requires `revoke-lf-teams-auditor-openfga.sh`. That is why it ships alongside the grant script rather than being written later under incident pressure.

## Why scripts rather than `/admin/reindex`

`POST /admin/reindex {"type":"b2b_org"}` also emits these grants — `PublishB2BOrgTeamGrantsFGA` is the FGA publisher on both reindex paths, and it is the same call that maintains `global_org_admin`. It produces identical tuples and is a valid fallback.

The scripts are the primary route because reindex re-fetches every org from Salesforce and is quota-gated (`ADMIN_REINDEX_QUOTA_THRESHOLD`, default `0.80`): it returns `503` at or above the threshold and stops mid-run if the threshold is crossed while running. The scripts write OpenFGA tuples only and cost no Salesforce quota. Reach for reindex when the tuples need to be repaired alongside the indexer documents anyway.

## Scripts

All three live in `scripts/`. The two OpenFGA scripts take the store ID as a **required first argument** — there is deliberately no default, because a default target on a script whose writes cannot be undone is a foot-gun.

For the same reason they require the team names in the environment rather than defaulting them. `lfStaffTeamName` in `charts/lfx-v2-member-service/values.yaml` is the authoritative copy; a second hardcoded copy in the scripts would drift, and granting the wrong team name is exactly as unreapable as granting on the wrong store. Read it back from the deployment you are about to back-fill, rather than retyping it — that also confirms the running build is the one that emits the grant:

```bash
export LF_STAFF_TEAM_NAME=$(kubectl --context <ctx> -n lfx get deploy lfx-v2-member-service \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="LF_STAFF_TEAM_NAME")].value}')
```

The two scripts deliberately do not have the same reach. The grant script reads `LF_STAFF_TEAM_NAME` and nothing else, so it cannot grant the contractor team even if `LF_CONTRACTOR_TEAM_NAME` is exported in the same shell — which it very plausibly is, since the revoke script is the reason to export it. Granting contractors before LFXV2-3071 has decided whether they get access would be unreapable by any service path.

The revoke script reads both, because rollback has to be able to target a team the service no longer emits. Export only the team you intend to remove; whichever variable is left unset is left untouched. That is how the contractor tuples were cleared from dev without disturbing the staff grants. Either script errors out if none of the variables it reads is set.

The grant and revoke scripts share their OpenFGA helpers via `scripts/lib/openfga-team-auditor.sh` (pagination, batch apply, the transport guard, argument validation). They were near-copies; sharing matters here because the revoke script is the rollback path that runs under incident pressure, and a rollback that has quietly drifted from the tested grant path is worse than no rollback. Both scripts source the library relative to their own location, so they must be run from a checkout rather than copied to a pod in isolation.

### 1. `export-b2b-org-uids-from-opensearch.sh`

Builds the org census from OpenSearch.

```bash
kubectl --context lfx-v2-prod -n lfx port-forward pod/opensearch-proxy-… 9299:9200

./scripts/export-b2b-org-uids-from-opensearch.sh /tmp/lf-team-auditor-backfill
```

OpenSearch rather than OpenFGA, because an FGA-based census can only enumerate orgs that already hold some tuple. Reading `global_org_admin` grants would skip every org that never received one — precisely the population most in need of the auditor grant. `ListObjects` is worse: it caps at ~1,000 results with no continuation token, so it truncates silently.

The script deduplicates UIDs unconditionally and reports `duplicates_dropped` in the summary. This is not tidying: `latest=true` is not guaranteed unique (the indexer ships a janitor whose job is resolving "multiple latest documents" conflicts), and a repeated UID inside one write batch is rejected with `cannot_allow_duplicate_tuples_in_one_request` — which `on_duplicate: ignore` does **not** cover, since that governs collisions with *existing* tuples, not repeats within one request.

### 2. `grant-lf-teams-auditor-openfga.sh`

Read-diff-write. Reads what each team already holds, diffs against the census, writes only what is missing.

```bash
kubectl --context lfx-v2-prod -n lfx port-forward svc/lfx-platform-openfga 8080:8080

# Always dry-run first
./scripts/grant-lf-teams-auditor-openfga.sh <store-id> /tmp/lf-team-auditor-backfill --dry-run
./scripts/grant-lf-teams-auditor-openfga.sh <store-id> /tmp/lf-team-auditor-backfill
```

Because the diff is recomputed on every run, `--dry-run` over a completed run reports zero. That makes dry-run the reconciliation check rather than a separate tool — run it before *and* after.

Every write batch sets `"on_duplicate": "ignore"` inside the `writes` object. This is load-bearing: `/write` is transactional per request, the backfill runs after the rollout so the service is concurrently asserting the same tuples, and without it the first collision would abort the whole run. Precedent is in-house — `fga-sync` sets the same options on every batch it writes.

### 3. `revoke-lf-teams-auditor-openfga.sh`

The rollback path. **Not part of the rollout.**

```bash
./scripts/revoke-lf-teams-auditor-openfga.sh <store-id> --dry-run
./scripts/revoke-lf-teams-auditor-openfga.sh <store-id>          # prompts for the store ID
./scripts/revoke-lf-teams-auditor-openfga.sh <store-id> --yes    # skips the prompt
```

The live form prompts for the store ID before deleting, because it differs from the dry-run form by a single flag and recovery is asymmetric — a full re-export plus the grant backfill. On a non-interactive stdin it refuses outright unless `--yes` is given.

Deletes only tuples whose subject is exactly one of the configured teams; per-user `auditor` grants and `global_org_admin` are never touched. Batches set `"on_missing": "ignore"` inside the `deletes` object — the mirror of the grant script's problem, since the tuple list comes from a paginated read that can go stale mid-run.

**Stop the emission before you revoke, not after.** Set the team name to `""` (or revert) and roll out the API *and* the CDC consumer first. Revoking against a service that is still emitting is a race the script cannot win: any org written during or after the run re-acquires the tuple, and fga-sync will not reap it later because the subject begins with `team:`. The residue is invisible — a post-run dry-run only reports what exists at that instant, so a clean dry-run against a live emitter proves nothing.

### Rollback order

1. Set `LF_STAFF_TEAM_NAME` to `""` (or revert the code) and roll out both deployments.
2. Confirm no pod is still running the emitting config.
3. Export the team name to revoke — the service no longer emits it, but the script still needs to know what to look for.
4. `revoke-lf-teams-auditor-openfga.sh <store-id> --dry-run`, then the live run.
5. Re-run the dry-run; expect zero. This is only meaningful once step 1 has landed.

## Rollout order

1. Deploy to dev, confirm new orgs get the grants.
2. Deploy to prod. From this point CDC upserts assert the grants for any org that changes.
3. Run the export, then the grant script's dry-run, then the live run.
4. Re-run the dry-run; expect zero.
5. Spot-check the cascade: pick an org with no per-user auditor, confirm a staff-team member can `GET` it and the `project_membership` beneath it.

Step 3 comes after step 2 deliberately — that ordering is why the collision handling is required rather than optional.
