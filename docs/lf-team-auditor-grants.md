<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# LF Team Auditor Grants — Operator Runbook

`team:lf-staff#member` and `team:lf-contractor#member` hold the `auditor` relation on every `b2b_org`. The service asserts these grants on every full-sync publish path; the scripts in this document exist for the orgs that already existed when that behaviour shipped, and for rolling the grant back.

See [fga-contract.md](./fga-contract.md) for the message-level contract and [LFXV2-2937](https://linuxfoundation.atlassian.net/browse/LFXV2-2937) for the change itself.

## What the grant actually confers

Read access to **all six document types this service indexes**, on every org. Two distinct mechanisms are involved, and auditing the Heimdall ruleset alone understates the reach:

| Mechanism | Reaches |
|---|---|
| The four `auditor`-gated REST routes in `ruleset.yaml` | `GET /b2b_orgs/{uid}`, `GET /b2b_orgs/{uid}/settings`, `GET /project_memberships/{uid}`, `GET /project_memberships/{membership_uid}/key_contacts/{uid}` |
| Search — every index config declares `AccessCheckRelation: auditor` | `b2b_org`, `b2b_org_settings`, `project_membership`, `key_contact`, **`workspace`**, **`workspace_project`** |

Workspaces and workspace-projects have no `auditor` REST route at all (every workspace route is `writer`-gated), but their index documents access-check `auditor` on the parent `b2b_org` by design, so they are reachable via search.

Note that `GET /b2b_orgs/{uid}/settings` exposes **pending-invite email addresses**. That route was gated on `auditor` rather than `writer` on the premise that auditors are per-org trusted principals; the blanket grant changes that premise.

No write access anywhere. The `[user, team#member]` branch of `b2b_org.auditor` feeds nothing upward, unlike `global_org_admin`, which flows into `writer`.

## The one-way-door property

**fga-sync never deletes a tuple whose subject begins with `team:`.** Reverting the service code stops *new* grants being written; it does not remove existing ones. Setting `LF_STAFF_TEAM_NAME` / `LF_CONTRACTOR_TEAM_NAME` to `""` behaves the same way.

Removing the tuples requires `revoke-lf-teams-auditor-openfga.sh`. That is why it ships alongside the grant script rather than being written later under incident pressure.

## Why scripts rather than `/admin/reindex`

`POST /admin/reindex {"type":"b2b_org"}` also emits these grants — `PublishB2BOrgTeamGrantsFGA` is the FGA publisher on both reindex paths, and it is the same call that maintains `global_org_admin`. It produces identical tuples and is a valid fallback.

The scripts are the primary route because reindex re-fetches every org from Salesforce and is quota-gated (`ADMIN_REINDEX_QUOTA_THRESHOLD`, default `0.80`): it returns `503` at or above the threshold and stops mid-run if the threshold is crossed while running. The scripts write OpenFGA tuples only and cost no Salesforce quota. Reach for reindex when the tuples need to be repaired alongside the indexer documents anyway.

## Scripts

All three live in `scripts/`. The two OpenFGA scripts take the store ID as a **required first argument** — there is deliberately no default, because a default target on a script whose writes cannot be undone is a foot-gun.

For the same reason they require the team names in the environment rather than defaulting them. The authoritative values live in `charts/lfx-v2-member-service/values.yaml`; a third hardcoded copy in the scripts would drift, and granting the wrong team name is exactly as unreapable as granting on the wrong store:

```bash
export LF_STAFF_TEAM_NAME=lf-staff
export LF_CONTRACTOR_TEAM_NAME=lf-contractor
```

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

**Stop the emission before you revoke, not after.** Set both team names to `""` (or revert) and roll out the API *and* the CDC consumer first. Revoking against a service that is still emitting is a race the script cannot win: any org written during or after the run re-acquires the tuple, and fga-sync will not reap it later because the subject begins with `team:`. The residue is invisible — a post-run dry-run only reports what exists at that instant, so a clean dry-run against a live emitter proves nothing.

### Rollback order

1. Set `LF_STAFF_TEAM_NAME` and `LF_CONTRACTOR_TEAM_NAME` to `""` (or revert the code) and roll out both deployments.
2. Confirm no pod is still running the emitting config.
3. `revoke-lf-teams-auditor-openfga.sh <store-id> --dry-run`, then the live run.
4. Re-run the dry-run; expect zero. This is only meaningful once step 1 has landed.

## Rollout order

1. Deploy to dev, confirm new orgs get the grants.
2. Deploy to prod. From this point CDC upserts assert the grants for any org that changes.
3. Run the export, then the grant script's dry-run, then the live run.
4. Re-run the dry-run; expect zero.
5. Spot-check the cascade: pick an org with no per-user auditor, confirm an `lf-staff` member can `GET` it and the `project_membership` beneath it.

Step 3 comes after step 2 deliberately — that ordering is why the collision handling is required rather than optional.
