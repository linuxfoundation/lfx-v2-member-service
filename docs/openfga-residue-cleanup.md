<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# OpenFGA residue cleanup

This runbook removes a reviewed list of leftover OpenFGA tuples with
`scripts/cleanup-openfga-residue.sh`. The tool exists for the residue left by LFXV2-2049, which moved
the public `uid` of `b2b_org` and `project_membership` from UUID v8 to the 18-character Salesforce
ID without deleting the UUID-keyed objects. It also covers reviewed anomalies on those object types.
No member-service code path reads or writes UUID-keyed objects, so nothing republishes or repairs
them; they stay in OpenFGA until deleted.

The tool does not decide what is residue. The list is built and reviewed before the change window,
and the tool deletes only tuples from that list.

## Safety properties

- The list is bound to its reviewed content hash (`--expect-sha256`). A mismatch stops before any
  OpenFGA call.
- The list may only contain tuples on `b2b_org` and `project_membership` objects and must never grant
  `team:global_org_admin#member`. Lists that violate this are refused even when the hash matches.
- `verify` reads every listed object in full, in parallel (`--concurrency`, default 16). Any failed
  read fails the run before anything is written. Tuples already gone are recorded and skipped. A
  tuple on a listed object that the list lacks fails closed: the list is stale and must be rebuilt.
- `delete` and `restore` require exactly one of `--dry-run` or `--confirm`.
- Before the first delete, `delete --confirm` records exactly the tuples it is about to remove in
  `deleted-set.jsonl`, bound by `deleted-set.sha256`. A retry merges into that record, so restore
  covers every run.
- Deletes and restores use batches of at most 100, pause between batches (`--pause-seconds`), and
  ignore tuples that are already absent or already present, so every phase can be retried.
- `restore` writes back only the recorded deleted set and refuses a set that no longer matches its
  hash.
- Output files contain principal identifiers and are written with owner-only permissions. Keep them
  outside the repository and delete them according to the approved operational retention policy.

## Building the list

The list is JSONL, one tuple per line: `{"user": ..., "relation": ..., "object": ...}`. Field order,
duplicates, and extra fields do not matter; the tool canonicalizes before hashing. Compute the hash
the tool expects with:

```bash
jq -cS '{user, relation, object}' list.jsonl | LC_ALL=C sort -u | shasum -a 256
```

Build it read-only from OpenFGA and review it before the change window:

1. Enumerate every `b2b_org` and `project_membership` object whose ID is not an 18-character
   Salesforce ID, and read each object's tuples in full.
2. Add the tuples where those objects are the subject, for example `parent` and `child` edges and the
   `b2b_org` relation on `project_membership`.
3. Confirm that no live Salesforce-ID object depends on a listed object. A live object that would lose
   access through the list is a blocker, not residue.
4. Leave out legacy global org-admin team grants; `docs/global-org-admin-team-migration.md` removes
   those.
5. Record the canonical content hash next to the list and have it peer-reviewed.

## 1. Verify

```bash
export OPENFGA_URL=http://localhost:8080
STORE_ID=<explicit-store-id>
LIST=<reviewed-list.jsonl>
LIST_SHA256=<reviewed-hash>
OUT=/tmp/openfga-residue-cleanup/<environment>

scripts/cleanup-openfga-residue.sh verify \
  --store-id "$STORE_ID" \
  --list "$LIST" \
  --expect-sha256 "$LIST_SHA256" \
  --output-dir "$OUT"
```

Review the counts and `present.jsonl`, `gone.jsonl`, and `unexpected.jsonl`. Each read is one
request per listed object, so wall time is dominated by request latency: through a
`kubectl port-forward` a request takes about a second, and `--concurrency 32` keeps a verify of
tens of thousands of objects under an hour. A confirmed `delete` verifies twice, before and after
deleting. Lower `--concurrency` if OpenFGA shows read pressure.

## 2. Preview and delete

Run `delete` with `--dry-run`, review the count, then replace `--dry-run` with `--confirm` in the
approved window:

```bash
scripts/cleanup-openfga-residue.sh delete \
  --store-id "$STORE_ID" \
  --list "$LIST" \
  --expect-sha256 "$LIST_SHA256" \
  --output-dir "$OUT" \
  --dry-run
```

A confirmed delete re-runs verify, records the deleted set, deletes in paced batches, and verifies
again: it ends with zero listed tuples left or returns an error. On failure, re-run the same command;
already-deleted tuples are skipped.

The script writes to OpenFGA directly, so fga-sync's check cache is not invalidated by it. No live
access decision should depend on the listed objects, but invalidating is cheap and removes any doubt:

```bash
kubectl -n lfx exec deploy/lfx-platform-nats-box -- nats kv put fga-sync-cache inv 1
```

## Rollback

```bash
scripts/cleanup-openfga-residue.sh restore \
  --store-id "$STORE_ID" \
  --list "$LIST" \
  --expect-sha256 "$LIST_SHA256" \
  --output-dir "$OUT" \
  --dry-run
```

Review the count, then replace `--dry-run` with `--confirm`. `restore` writes back only
`deleted-set.jsonl` from the same output directory and refuses if it no longer matches
`deleted-set.sha256`. Invalidate `fga-sync-cache` afterwards.
