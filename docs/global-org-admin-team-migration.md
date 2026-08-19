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
- Any OpenSearch-to-Salesforce count difference blocks writes until explicitly approved.
- Cleanup requires a matching verification checkpoint, completed API and CDC rollouts, and no
  old-team tuples absent from the pre-cutover snapshot.
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

## 3. Preview and duplicate stable-team tuples

```bash
scripts/migrate-global-org-admin-team-openfga.sh apply \
  --store-id "$STORE_ID" \
  --old-team "$OLD_TEAM" \
  --output-dir "$OUT" \
  --dry-run
```

After peer review, replace `--dry-run` with `--confirm`. Rerunning is safe.

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
  --cutover-complete \
  --dry-run
```

The command re-reads old-team tuples, preserves the pre-cleanup set in the output directory, and
blocks if any tuple was not present in the pre-cutover snapshot. This set comparison detects new old
writes even when unrelated deletions keep the total count unchanged. After review, replace
`--dry-run` with `--confirm`. Re-run `verify` and direct authorization checks after cleanup.

## Rollback

Before cleanup, revert the GitOps value to the old team identifier; both tuple sets still exist.
After cleanup, rollback is manual: rerun the migration in the opposite direction using a reviewed
snapshot, because normal service reconciliation does not remove or restore team-subject tuples.
