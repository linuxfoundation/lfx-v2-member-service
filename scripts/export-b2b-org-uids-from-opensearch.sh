#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# export-b2b-org-uids-from-opensearch.sh — Scroll OpenSearch for every live
# b2b_org and write its UID to a JSONL file, as the authoritative org census
# for the lf-staff / lf-contractor auditor backfill. See LFXV2-2937.
#
# Why OpenSearch and not OpenFGA: an FGA-based census can only enumerate orgs
# that already hold some tuple. Reading global_org_admin grants would skip any
# org that never received one — and that population is precisely the set most
# in need of the auditor grant. ListObjects is worse still: it caps at ~1,000
# results with no continuation token, so it would silently truncate.
#
# Prerequisites:
#   kubectl --context lfx-v2-prod -n lfx port-forward pod/opensearch-proxy-… 9299:9200
#
# Usage:
#   ./scripts/export-b2b-org-uids-from-opensearch.sh [output_dir]
#
# Outputs (default: /tmp/lf-team-auditor-backfill):
#   b2b-org-uids.jsonl    — one {"uid"} object per line, deduplicated and sorted
#   b2b-org-uids.summary  — raw hit count, deduplicated count, and the gap

set -euo pipefail

OUT_DIR="${1:-/tmp/lf-team-auditor-backfill}"
OPENSEARCH_URL="${OPENSEARCH_URL:-http://localhost:9299}"
INDEX="${OPENSEARCH_INDEX:-resources}"
SCROLL_TTL="${SCROLL_TTL:-5m}"
PAGE_SIZE="${PAGE_SIZE:-500}"

mkdir -p "$OUT_DIR"
JSONL="$OUT_DIR/b2b-org-uids.jsonl"
SUMMARY="$OUT_DIR/b2b-org-uids.summary"

echo "→ OpenSearch: ${OPENSEARCH_URL}/${INDEX}"
echo "→ Output dir: ${OUT_DIR}"
echo ""

python3 - "$OPENSEARCH_URL" "$INDEX" "$SCROLL_TTL" "$PAGE_SIZE" "$JSONL" "$SUMMARY" <<'PY'
import json
import sys
import urllib.error
import urllib.request

base, index, scroll_ttl, page_size, jsonl_path, summary_path = sys.argv[1:7]
page_size = int(page_size)

# latest=true excludes superseded document versions; the deleted_at must_not
# excludes soft-deleted orgs. Neither guarantees uniqueness on its own — see
# the dedupe below.
QUERY = {
    "size": page_size,
    "_source": ["object_id"],
    "query": {
        "bool": {
            "must": [
                {"term": {"object_type": "b2b_org"}},
                {"term": {"latest": True}},
            ],
            "must_not": [
                {"exists": {"field": "deleted_at"}},
            ],
        }
    },
    "sort": ["_doc"],
}


def post(path, body):
    url = f"{base.rstrip('/')}/{path.lstrip('/')}"
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        return json.load(resp)


def get_total():
    # track_total_hits is required: without it OpenSearch stops counting at
    # 10,000 and reports {"value": 10000, "relation": "gte"}. The consistency
    # check below compares the scroll's hit count against this number, so a
    # capped total would turn every run over a 10k estate into a false
    # truncation warning — training the operator to ignore the one check that
    # would catch a real truncation.
    data = post(
        f"{index}/_search",
        {"size": 0, "track_total_hits": True, "query": QUERY["query"]},
    )
    total = data["hits"]["total"]
    if isinstance(total, dict):
        if total.get("relation") == "gte":
            raise RuntimeError(
                f"OpenSearch returned a lower-bound total ({total.get('value')}); "
                "track_total_hits was not honoured, so the consistency check "
                "cannot be trusted."
            )
        return total.get("value", 0)
    return int(total)


try:
    expected = get_total()
except urllib.error.URLError as e:
    print(f"ERROR: cannot reach OpenSearch at {base}: {e}", file=sys.stderr)
    print("Start port-forward, e.g.:", file=sys.stderr)
    print("  kubectl --context lfx-v2-prod -n lfx port-forward pod/opensearch-proxy-… 9299:9200", file=sys.stderr)
    sys.exit(1)
except RuntimeError as e:
    print(f"ERROR: {e}", file=sys.stderr)
    sys.exit(1)

print(f"→ Expected docs (b2b_org, latest, not deleted): {expected:,}")

raw_hits = 0
uids = set()

initial = post(f"{index}/_search?scroll={scroll_ttl}", QUERY)
scroll_id = initial["_scroll_id"]
hits = initial["hits"]["hits"]

while hits:
    for h in hits:
        raw_hits += 1
        src = h.get("_source", {})
        uid = src.get("object_id") or h.get("_id", "").split(":", 1)[-1]
        if uid:
            uids.add(uid)
    if raw_hits % 2000 == 0:
        print(f"  … fetched {raw_hits:,} / {expected:,}")
    page = post("_search/scroll", {"scroll": scroll_ttl, "scroll_id": scroll_id})
    scroll_id = page["_scroll_id"]
    hits = page["hits"]["hits"]

try:
    post("_search/scroll", {"scroll_id": scroll_id, "scroll": scroll_ttl})
except Exception:
    pass

# Dedupe is unconditional, not defensive tidying. latest=true is not guaranteed
# unique: the indexer ships a janitor whose whole job is resolving
# "multiple latest documents" conflicts, so the window where two docs for one
# org both carry latest=true is a state the platform expects to occur. A
# duplicate UID inside a single OpenFGA write batch is rejected outright with
# cannot_allow_duplicate_tuples_in_one_request, which would abort the batch —
# and on_duplicate:ignore does not cover it, since that governs collisions with
# *existing* tuples, not repeats within one request.
ordered = sorted(uids)
duplicates = raw_hits - len(ordered)

# The consistency check runs before anything is written. A scroll that dies
# partway — an expired TTL on a slow index, or a shard failure — still yields a
# syntactically valid, sorted census, and the grant script's only validation is
# that the file exists. Writing first would leave a truncated census on disk
# that is indistinguishable from a good one, silently omitting orgs from the
# backfill while the follow-up dry-run confirms the wrong answer.
if expected and raw_hits != expected:
    print(
        f"ERROR: scroll returned {raw_hits:,} hits but _search reported {expected:,} — "
        "refusing to write a possibly truncated census.",
        file=sys.stderr,
    )
    sys.exit(2)

with open(jsonl_path, "w", encoding="utf-8") as f:
    for uid in ordered:
        f.write(json.dumps({"uid": uid}) + "\n")

with open(summary_path, "w", encoding="utf-8") as f:
    f.write(f"raw_hits={raw_hits}\n")
    f.write(f"unique_uids={len(ordered)}\n")
    f.write(f"duplicates_dropped={duplicates}\n")
    f.write(f"expected_from_search_api={expected}\n")

print(f"→ Wrote {len(ordered):,} unique UIDs to {jsonl_path}")
if duplicates:
    print(f"→ NOTE: dropped {duplicates:,} duplicate hits (multiple latest=true docs for the same org)")
PY

echo ""
echo "Compare the unique count against the number of global_org_admin grants in the target store."
echo "The gap measures the orgs an FGA-based enumeration would have missed."
echo ""
echo "Next: dry-run the grant script"
echo "  ./scripts/grant-lf-teams-auditor-openfga.sh <store-id> ${OUT_DIR} --dry-run"
