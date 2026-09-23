<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# LF Team Auditor Grants — Operator Runbook

The LF staff team — named by `LF_STAFF_TEAM_NAME`, written as `team:<name>#member` throughout this document — holds the `auditor` relation on every `b2b_org`. The service asserts the grant on every full-sync publish path; the scripts in this document exist for the orgs that already existed when that behaviour shipped, and for rolling the grant back.

> **Contractor rollback in progress.** `lf-contractor` was granted the same blanket `auditor` under [LFXV2-3071](https://linuxfoundation.atlassian.net/browse/LFXV2-3071) (8,105 prod backfilled 2026-09-16). Two writers kept adding it after that: member-service on every org write, and the `sync-global-groups` reconciler on every org in its census until its fixed-team build deployed. So the count tracks each environment's org census, which grew: dev's went from 1,634 to 6,738 between 2026-09-16 and 2026-09-23. Don't compare the revoke dry-run against a number written here. Compare it against the reconciler's latest `org auditor surplus` count for `lf-contractor` in that environment, which is the live figure). The service no longer emits it and the grant script no longer reads it, but **neither removes what was written** — fga-sync never deletes a `team:`-subject tuple. Until the revoke below has run per environment, those tuples are live and contractors read every org. The `sync-global-groups` reconciler reports them on every run as `org auditor surplus`.

## When the grant starts

**The deploy is the cutover, not the backfill.** Every write path asserts the reference, so orgs begin acquiring the tuple as soon as the new build is running and CDC events arrive. The scripts below only sweep up orgs that never change on their own.

The team name is the same in every environment, so it lives as a real value (`lfStaffTeamName`) in `charts/lfx-v2-member-service/values.yaml` rather than being overridden per environment in `lfx-v2-argocd`. It is the single authoritative copy: neither the service code nor the scripts hardcode it. The chart carries no contractor setting any more; the contractor tuples written under LFXV2-3071 are historical and removed only by the revoke script.

Rollout order:

1. Deploy the API **and** the CDC consumer. New and changed orgs start acquiring the grant from this point.
2. Run the census and grant scripts below for the existing estate.
3. Re-run the grant script with `--dry-run`; expect zero missing.

Plan step 1 deliberately. Reverting the deploy or clearing the team variables afterwards stops further writes but removes nothing already written — that needs the revoke script.

**Staff only.** [LFXV2-3071](https://linuxfoundation.atlassian.net/browse/LFXV2-3071) granted `lf-contractor` the same blanket `auditor` on the argument that it already held `auditor` on the tenant root project. That argument is withdrawn: lfx-self-serve#2814 Release 2 deletes the root tuple (dropped, not migrated), and contractors keep explicit per-org grants only (Manish Dixit, 2026-08-10). Adding a team back is not a one-line change: it needs the `values.yaml` key, an `LF_*_TEAM_NAME` env entry in **both** Deployment templates, the env list in `B2BOrgAuditorTeamNames`, the `fga_team_names` arguments of the grant script, the `kubectl` exports in this runbook, and the CLAUDE.md env tables — message construction alone is team-count-agnostic. The resulting grant cannot be taken back by reverting that change.

See [fga-contract.md](./fga-contract.md) for the message-level contract and [LFXV2-2937](https://linuxfoundation.atlassian.net/browse/LFXV2-2937) for the change itself.

## What the grant actually confers

Read access to **all six document types this service indexes**, on every org. Two distinct mechanisms are involved, and auditing the Heimdall ruleset alone understates the reach:

| Mechanism | Reaches |
|---|---|
| The four `auditor`-gated REST routes in `ruleset.yaml` | `GET /b2b_orgs/{uid}`, `GET /b2b_orgs/{uid}/settings`, `GET /project_memberships/{uid}`, `GET /project_memberships/{membership_uid}/key_contacts/{uid}` |
| Search — every index config declares `AccessCheckRelation: auditor` | `b2b_org`, `b2b_org_settings`, `project_membership`, `key_contact`, **`workspace`**, **`workspace_project`** |

Workspaces and workspace-projects have no `auditor` REST route at all (every workspace route is `writer`-gated), but their index documents access-check `auditor` on the parent `b2b_org` by design, so they are reachable via search.

Note that `GET /b2b_orgs/{uid}/settings` exposes **pending-invite email addresses**. That route was gated on `auditor` rather than `writer` on the premise that auditors are per-org trusted principals; the blanket grant changes that premise. The `b2b_org_settings` index document carries the same `auditor` access check, so the roster is reachable through search as well as through the route — any narrowing would have to cover both.

This was reviewed and accepted rather than narrowed. [LFXV2-3026](https://linuxfoundation.atlassian.net/browse/LFXV2-3026) is Org Dash / PCC parity. What is asserted from the `lf-staff` precedent rather than verifiable here: that legacy tooling already shows this roster to the same population — on that basis the grant migrates an existing disclosure rather than creating one. (The contractor half of this argument — that `lf-contractor` held the same tenant-root `auditor` tuple — is withdrawn: lfx-self-serve#2814 Release 2 deletes that tuple. Until the revoke has run, contractors still reach this roster.) Narrowing the route to `writer` would also strip roster read from the per-org auditors who hold it today.

No write access anywhere. The `[user, team#member]` branch of `b2b_org.auditor` feeds nothing upward, unlike `global_org_admin`, which flows into `writer`.

## The one-way-door property

**fga-sync never deletes a tuple whose subject begins with `team:`.** Reverting the service code stops *new* grants being written; it does not remove existing ones. Setting `LF_STAFF_TEAM_NAME` to `""` behaves the same way. `LF_CONTRACTOR_TEAM_NAME` is no longer read at all, so the service already emits no contractor grant; the existing contractor tuples still need the revoke script.

That guard belongs to the **deployed** fga-sync, not to this repository's dependency pin. It was added in fga-sync `v0.3.1` — the delete branch of `SyncObjectTuples` in `fga.go` — and the platform chart deploys `~0.3.5`. This repo pins `v0.2.17` in `go.mod`, which predates the guard, but that pin supplies only the message types in `pkg/types` and `pkg/constants`; nothing here links the sync engine, so the pin has no bearing on what the running service deletes. Everything below assumes a deployed fga-sync at `v0.3.1` or later. On anything older the guard is absent, and a settings write would revoke these grants instead of preserving them.

Removing the tuples requires `revoke-lf-teams-auditor-openfga.sh`. That is why it ships alongside the grant script rather than being written later under incident pressure.

## Why scripts rather than `/admin/reindex`

`POST /admin/reindex {"type":"b2b_org"}` also emits these grants — `PublishB2BOrgTeamGrantsFGA` is the FGA publisher on both reindex paths, and it is the same call that maintains `global_org_admin`. It produces identical tuples and is a valid fallback.

The scripts are the primary route because reindex re-fetches every org from Salesforce and is quota-gated (`ADMIN_REINDEX_QUOTA_THRESHOLD`, default `0.80`): it returns `503` at or above the threshold and stops mid-run if the threshold is crossed while running. The scripts write OpenFGA tuples only and cost no Salesforce quota. Reach for reindex when the tuples need to be repaired alongside the indexer documents anyway.

## Scripts

All three live in `scripts/`. The two OpenFGA scripts take the store ID as a **required first argument** — there is deliberately no default, because a default target on a script whose writes cannot be undone is a foot-gun.

For the same reason they require the team name in the environment rather than defaulting it. `lfStaffTeamName` in `charts/lfx-v2-member-service/values.yaml` is the authoritative copy; a second hardcoded copy in the scripts would drift, and granting the wrong team name is exactly as unreapable as granting on the wrong store. Read it back from the deployment you are about to back-fill, rather than retyping it — that also confirms the running build is the one that emits the grant:

```bash
export LF_STAFF_TEAM_NAME=$(kubectl --context <ctx> -n lfx get deploy lfx-v2-member-service \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="LF_STAFF_TEAM_NAME")].value}')
# The contractor variable is no longer injected into the deployment. To revoke it,
# export the name literally (see the revoke runbook below).
```

The two scripts no longer have the same reach, deliberately. The grant script reads `LF_STAFF_TEAM_NAME` only, so a backfill run from a shell that still exports `LF_CONTRACTOR_TEAM_NAME` cannot re-create the withdrawn grant. It errors out if that one variable is unset.

The revoke script still reads **both**, because rollback has to be able to target a team the service no longer emits — that is exactly the contractor case. Export only the team you intend to remove; whichever variable is left unset is left untouched. Exporting both deletes both: staff would lose `auditor` on every org.

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

**Stop the emission before you revoke, not after.** For `lf-contractor`, that means the staff-only build is running on the API *and* the CDC consumer — there is no variable to blank. For `lf-staff`, set `lfStaffTeamName: ""` (or revert) and roll out both first. Revoking against a service that is still emitting is a race the script cannot win: any org written during or after the run re-acquires the tuple, and fga-sync will not reap it later because the subject begins with `team:`. The residue is invisible — a post-run dry-run only reports what exists at that instant, so a clean dry-run against a live emitter proves nothing.

### Rollback order

1. Stop the emission for the team you are revoking, and roll out **both** deployments (API and CDC consumer). For `lf-contractor` this is already done in code — the chart key and both env entries are gone, so any build from this revision emits staff only; confirm the running pods are on it. For `lf-staff`, set `lfStaffTeamName: ""` and roll out. Do not blank a team you intend to keep: orgs written during the window would miss its tuple until re-written or backfilled.
2. Confirm no pod is still running the emitting config.
3. **Stop the reconciler re-granting.** `sync-global-groups` (lfx-v2-argocd) writes blanket `auditor` for every team it considers in scope, every 10 minutes, and it never deletes. Either deploy a build whose team set excludes the team you are revoking, or set `ORG_RECONCILE_ENABLED=false` in that environment's overlay. Skipping this loses the race exactly as a live emitter does — the tuples come back within one run, and the confirming dry-run in step 6 will show it.
4. Export the team name to revoke — the service no longer emits it, but the script still needs to know what to look for.
5. `revoke-lf-teams-auditor-openfga.sh <store-id> --dry-run`, then the live run.
6. Re-run the dry-run; expect zero. This is only meaningful once steps 1 and 3 have landed — against a live emitter *or* a reconciler still granting the team, a clean dry-run proves nothing.

## Rollout order

1. Deploy to dev, confirm new orgs get the grants.
2. Deploy to prod — API and CDC consumer together. During a staggered rollout the two emitters assert different team sets, which converges: references for `team:` subjects are additive under the deployed fga-sync guard, and the older emitter revokes nothing. From this point CDC upserts assert the grants for any org that changes.
3. Run the export, then the grant script's dry-run (with `LF_STAFF_TEAM_NAME` exported from the deployment), then the live run.
4. Re-run the dry-run; expect zero.
5. Spot-check the cascade: pick an org with no per-user auditor, confirm a member of the LF staff team can `GET` it and the `project_membership` beneath it.

Step 3 comes after step 2 deliberately — that ordering is why the collision handling is required rather than optional.
