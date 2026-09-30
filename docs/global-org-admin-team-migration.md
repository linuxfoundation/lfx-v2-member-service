<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Global organization administrator team migration

This runbook moves the global organization administrator roster and organization grants from an
environment-specific team identifier to `team:global_org_admin`.

The migration command is intentionally phase-oriented. Snapshot, planning, deployment, verification,
and cleanup happen at different times. Never run the command against a shared environment without an
approved change window and an explicit store ID.

## Safety properties

- `snapshot` and `plan` do not mutate OpenFGA.
- `apply` and `cleanup` require either `--dry-run` or `--confirm`.
- Writes and deletes use batches of at most 100 and ignore duplicates or already-missing tuples.
- Plan copies grants only for current, non-deleted organizations in the OpenSearch census.
- Each plan records the snapshot manifest hash; a newer snapshot invalidates the old plan.
- The approved stable roster is copied from the legacy roster, or with `--stable-roster-from-live`
  read from the current stable team (direct `user:` members only); either way it is hash-bound in
  `summary.json`.
- Any OpenSearch-to-Salesforce count difference blocks `apply`, `verify`, and `cleanup` until
  explicitly approved. Each phase checks `census_approved` itself, so a checkpoint written by an
  older script cannot carry an unapproved plan into `cleanup`.
- Cleanup requires a matching verification checkpoint, completed API and CDC rollouts, and no
  old-team tuples absent from the pre-cutover snapshot. Immediately before deletion it also
  revalidates the stable roster and grants against the approved plan and reruns the baseline
  allowed/denied authorization controls.
- `restore` verifies the final pre-cleanup tuple hashes and requires exactly one of `--dry-run` or `--confirm`.
- Snapshot files can contain principal identifiers. Keep them outside the repository and delete them
  according to the approved operational retention policy.

## Prerequisites

- `curl`, `jq`, and access to the target OpenFGA API.
- The target OpenFGA store ID and the environment's old team identifier.
- A fresh census from `scripts/export-b2b-org-uids-from-opensearch.sh`.
- A fresh Salesforce Membership Asset account count for comparison.
- One known allowed principal, one known denied principal, and one representative live organization.

Use a dedicated output directory for each environment:

```bash
export OPENFGA_URL=http://localhost:8080
STORE_ID=<explicit-store-id>
OLD_TEAM=<environment-specific-old-team>
OUT=/tmp/global-org-admin-migration/<environment>
```

## 1. Snapshot the old team and baseline access

```bash
scripts/migrate-global-org-admin-team-openfga.sh snapshot \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --allowed-user <known-admin-lfid> \
  --denied-user <known-non-admin-lfid> \
  --sample-org <live-b2b-org-uid>
```

Review `manifest.json`, `legacy-roster.jsonl`, `legacy-grants.jsonl`, and
`baseline-checks.json`. The allowed result must be `true`; the denied result must be `false`.

## 2. Export and review the live organization census

```bash
scripts/export-b2b-org-uids-from-opensearch.sh "$OUT/census"
```

Plan the migration with the independently measured Salesforce count:

```bash
scripts/migrate-global-org-admin-team-openfga.sh plan \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --census-file "$OUT/census/b2b-org-uids.jsonl" \
  --salesforce-count <count>
```

If the counts differ, investigate first. After recording the explanation in the operator change,
rerun with `--approve-census-difference`. Record `orphan_count` from `summary.json` on LFXV2-3034.

By default the plan approves a stable roster copied from the legacy roster, and `verify`/`cleanup`
require the stable team to match it exactly. If the stable team is already the authoritative roster
(members were added or removed there after cutover), add `--stable-roster-from-live`: the plan
records the stable team's current roster instead, `summary.json` shows
`stable_roster_source: "live"`, and `verify`/`cleanup` require exactly that reviewed roster.

With `--stable-roster-from-live`:

- Nothing compares the approved roster with the legacy team any more. `plan` prints and records
  `stable_roster_count` and the members added and removed relative to the legacy roster
  (`stable_roster_added_vs_legacy`, `stable_roster_removed_vs_legacy`). The peer reviewer must check
  `stable-roster-plan.jsonl` against the intended administrator list, alongside those counts and the
  grant counts, before `apply` or `verify`.
- `plan` refuses a stable team that is empty or has members other than direct users (wildcards,
  usersets, or conditional tuples).
- `apply` never writes members. Writing the approved roster back would undo any removal made in
  sso-tools after `plan`, and `verify` would then pass. So `apply` first confirms that the stable
  team still equals the approved roster and exits 4 if it changed. If every live
  organization already holds the stable grant, `snapshot` → `plan` → `verify` → `cleanup` needs no
  `apply`; `verify` still requires the census difference to be approved.
- The stable team is curated in sso-tools. Any membership edit between `plan` and `cleanup` makes
  `apply`, `verify`, or `cleanup` exit 4. Freeze edits for the change window, or re-run `snapshot` →
  `plan` → review → `verify` after an edit.

## 3. Preview and duplicate stable-team tuples

```bash
scripts/migrate-global-org-admin-team-openfga.sh apply \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --dry-run
```

After peer review, replace `--dry-run` with `--confirm`. Rerunning is safe. After a confirmed
`apply`, invalidate fga-sync's check cache (section 6), so new stable-team grants are not denied
from cached decisions.

## 4. Change configuration through GitOps

Release the chart containing the `globalOrgAdminTeamName: "global_org_admin"` default and
`GLOBAL_ORG_ADMIN_TEAM_NAME`. ArgoCD environment values deliberately omit this platform-wide
setting. Staging and production use a pinned OCI chart: bump their `targetRevision` to the release
containing the renamed key when removing the old UID override. Removing it while the old chart is
still pinned falls back to `team:_null`, emits placeholder references, and blocks legitimate admin
requests.

Development tracks the member-service chart at `HEAD`, so merging the member-service change is the
development cutover. Complete the stable-team `apply --confirm` phase in development before that
merge. Do not merge the staging or production override removals until their `targetRevision` is
bumped to the released chart in the same ArgoCD change.

Wait until both the API Deployment and the CDC consumer Deployment run the new configuration. During
rollout skew, old pods can continue writing old-team grants.

## 5. Verify parity and access

```bash
scripts/migrate-global-org-admin-team-openfga.sh verify \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --allowed-user <known-admin-lfid> \
  --denied-user <known-non-admin-lfid> \
  --sample-org <live-b2b-org-uid>
```

Verification re-reads both teams. It requires exact stable roster parity, every planned live grant
to exist, no legacy tuple absent from the pre-cutover snapshot, and unchanged allowed/denied checks.
Stable grants created after cutover are reported separately and do not fail verification. Review
`post-cutover-extra-stable-grants.jsonl` and its count in `summary.json`.

If rollout skew created a new legacy tuple, wait until no old pod remains, then repeat snapshot,
plan, and apply with a fresh census before verifying again. Do not bypass the gate or delete the
reported tuple manually. A successful run writes `verified.checkpoint`, binding cleanup to the
reviewed manifest and summary.

## 6. Preview and remove the old team

```bash
scripts/migrate-global-org-admin-team-openfga.sh cleanup \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --allowed-user <known-admin-lfid> \
  --denied-user <known-non-admin-lfid> \
  --sample-org <live-b2b-org-uid> \
  --cutover-complete \
  --dry-run
```

The command revalidates the stable tuple sets and authorization controls, then performs a final
old-team read. It blocks if any tuple was not present in the pre-cutover snapshot. `--confirm` binds
that exact roster and grants in `precleanup.checkpoint` before deleting them. Retries require the
live old-team sets to remain subsets of that immutable binding, then delete from the full binding.
`--dry-run` does not create it. Re-run `verify` and direct authorization checks after cleanup.

The script writes to OpenFGA directly, so fga-sync's check cache (`fga-sync-cache`) is not
invalidated by it; only fga-sync's own writes do that. Cached `allowed` decisions for removed
legacy-team members would otherwise stay in effect until the next fga-sync write or cache expiry.
Immediately after every confirmed `apply`, `cleanup`, or `restore`, invalidate the cache the same
way fga-sync does, by rewriting its `inv` marker; every cached decision older than the marker is
then re-checked against OpenFGA:

```bash
kubectl -n lfx exec deploy/lfx-platform-nats-box -- nats kv put fga-sync-cache inv 1
```

Then run the direct authorization checks.

## Rollback

Before cleanup, revert the GitOps value to the old team identifier; both tuple sets still exist.
After cleanup, first restore the hash-verified final pre-cleanup tuple set while the service still
targets the stable team:

```bash
scripts/migrate-global-org-admin-team-openfga.sh restore \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --dry-run
```

Review that the preview covers `precleanup-old-roster.jsonl` and `precleanup-old-grants.jsonl`, then
replace `--dry-run` with `--confirm`. The command refuses to proceed if either file differs from its
hash binding in `precleanup.checkpoint`; writes ignore tuples that already exist, so a confirmed
restore can be retried.

After the confirmed restore, invalidate `fga-sync-cache` with the command in section 6 so restored
access is visible through the API,
then directly verify the known allowed and denied principals against a
representative organization. Only then revert the GitOps value to the old team identifier and wait
for both the API and CDC consumer Deployments to complete. Keep the stable tuples in place until the
rollback has been verified; normal service reconciliation does not restore team-subject tuples.
