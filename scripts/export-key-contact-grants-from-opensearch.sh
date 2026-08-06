#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# export-key-contact-grants-from-opensearch.sh — Scroll OpenSearch for every
# key_contact doc that has a resolved LFID username (i.e. one that would have
# a live FGA member_put) and write {uid, membership_uid, username} to a JSONL
# file, for seeding the key-contact-grants NATS KV index without touching
# Salesforce or re-publishing to FGA. See LFXV2-2907.
#
# The filter (non-empty username AND non-empty membership_uid) matches exactly
# the gate in PublishKeyContactFGA (internal/service/key_contact_grant.go) —
# these are the only key contacts that ever got a member_put, so they're the
# only ones that need a grant-index entry.
#
# Caveat: OpenSearch is a secondary index, not authoritative evidence that a
# member_put was actually published — PublishKeyContactIndexer (which writes
# this doc) and PublishKeyContactFGA (which publishes the grant) are two
# independent publish calls that can fail independently. A key contact with a
# live FGA grant whose indexer publish failed (or hasn't run yet) is absent
# from this export and is left with no grant-index entry, same as if it
# predated the index entirely — its next CDC delete still hits the documented
# fallback (see docs/fga-contract.md). This export closes the common case
# cheaply; it is not a substitute for a full FGA-state reconciliation.
#
# Prerequisites:
#   kubectl --context lfx-v2-prod -n lfx port-forward pod/opensearch-proxy-… 9299:9200
#
# Usage:
#   ./scripts/export-key-contact-grants-from-opensearch.sh [output_dir]
#
# Outputs (default: /tmp/key-contact-grants-backfill):
#   key-contact-grants.jsonl    — one {"uid","membership_uid","username"} object per line
#   key-contact-grants.csv      — uid, membership_uid, username, updated_at
#   key-contact-grants.summary  — total count

set -euo pipefail

# The output files carry LFID usernames. umask 077 (owner rwx, nothing for
# group/other) applies to both the directory this creates and every file the
# Python export writes into it, so a shared /tmp default output_dir doesn't
# leave that data world/group-readable for other users on the host.
umask 077

OUT_DIR="${1:-/tmp/key-contact-grants-backfill}"
OPENSEARCH_URL="${OPENSEARCH_URL:-http://localhost:9299}"
INDEX="${OPENSEARCH_INDEX:-resources}"
SCROLL_TTL="${SCROLL_TTL:-5m}"
PAGE_SIZE="${PAGE_SIZE:-500}"

mkdir -p "$OUT_DIR"
JSONL="$OUT_DIR/key-contact-grants.jsonl"
CSV="$OUT_DIR/key-contact-grants.csv"
SUMMARY="$OUT_DIR/key-contact-grants.summary"

echo "→ OpenSearch: ${OPENSEARCH_URL}/${INDEX}"
echo "→ Output dir: ${OUT_DIR}"
echo ""

python3 - "$OPENSEARCH_URL" "$INDEX" "$SCROLL_TTL" "$PAGE_SIZE" "$JSONL" "$CSV" "$SUMMARY" <<'PY'
import csv
import json
import sys
import urllib.error
import urllib.request

base, index, scroll_ttl, page_size, jsonl_path, csv_path, summary_path = sys.argv[1:8]
page_size = int(page_size)

QUERY = {
    "size": page_size,
    # track_total_hits here (not just in a separate count call) lets the scroll's
    # own opening response report the exact total for the same snapshot the
    # scroll reads from — see the expected/rows check below.
    "track_total_hits": True,
    "_source": [
        "object_id",
        "data.membership_uid",
        "data.username",
        "updated_at",
    ],
    "query": {
        "bool": {
            "must": [
                {"term": {"object_type": "key_contact"}},
                {"exists": {"field": "data.username"}},
                {"exists": {"field": "data.membership_uid"}},
            ],
            "must_not": [
                {"term": {"data.username": ""}},
                {"term": {"data.membership_uid": ""}},
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


try:
    initial = post(f"{index}/_search?scroll={scroll_ttl}", QUERY)
except urllib.error.URLError as e:
    print(f"ERROR: cannot reach OpenSearch at {base}: {e}", file=sys.stderr)
    print("Start port-forward, e.g.:", file=sys.stderr)
    print("  kubectl --context lfx-v2-prod -n lfx port-forward pod/opensearch-proxy-… 9299:9200", file=sys.stderr)
    sys.exit(1)

# Read the total from this same response rather than a separate _count/_search
# call: a separate call opens its own snapshot, so live writes between it and
# the scroll's own open (constant CDC traffic against ~100k key contacts)
# could make "expected" and the scroll's actual snapshot disagree, failing an
# otherwise-valid export with the mismatch check below. track_total_hits on
# QUERY (used for this same request) keeps the total exact rather than capped
# at OpenSearch's default 10,000 threshold.
total = initial["hits"]["total"]
expected = total.get("value", 0) if isinstance(total, dict) else int(total)
print(f"→ Expected docs (key_contact, resolved username + membership_uid): {expected:,}")

rows = []
scroll_id = initial["_scroll_id"]
hits = initial["hits"]["hits"]

while hits:
    for h in hits:
        src = h.get("_source", {})
        data = src.get("data", {})
        uid = src.get("object_id") or h.get("_id", "").split(":", 1)[-1]
        membership_uid = data.get("membership_uid") or ""
        username = data.get("username") or ""
        if not uid or not membership_uid or not username:
            # Belt-and-braces: the query already filters this, but a malformed
            # doc must not produce a half-populated grant entry.
            continue
        rows.append(
            {
                "uid": uid,
                "membership_uid": membership_uid,
                "username": username,
                "updated_at": src.get("updated_at") or "",
            }
        )
    if len(rows) % 2000 == 0 and rows:
        print(f"  … fetched {len(rows):,} / {expected:,}")
    page = post("_search/scroll", {"scroll": scroll_ttl, "scroll_id": scroll_id})
    scroll_id = page["_scroll_id"]
    hits = page["hits"]["hits"]

try:
    post("_search/scroll", {"scroll_id": scroll_id, "scroll": scroll_ttl})
except Exception:
    pass

rows.sort(key=lambda r: r["uid"])

with open(jsonl_path, "w", encoding="utf-8") as f:
    for r in rows:
        f.write(json.dumps({"uid": r["uid"], "membership_uid": r["membership_uid"], "username": r["username"]}) + "\n")

with open(csv_path, "w", newline="", encoding="utf-8") as f:
    w = csv.DictWriter(f, fieldnames=["uid", "membership_uid", "username", "updated_at"])
    w.writeheader()
    w.writerows(rows)

with open(summary_path, "w", encoding="utf-8") as f:
    f.write(f"total={len(rows)}\n")
    f.write(f"expected_from_scroll_open={expected}\n")

print(f"→ Wrote {len(rows):,} rows to {jsonl_path}")
print(f"→ Wrote CSV to {csv_path}")

if expected and len(rows) != expected:
    print(f"WARNING: scroll returned {len(rows):,} rows but the scroll-open total reported {expected:,}", file=sys.stderr)
    sys.exit(2)
PY

echo ""
echo "Next: dry-run backfill into the key-contact-grants KV bucket"
echo "  ./scripts/backfill-key-contact-grants-kv.sh ${OUT_DIR} --dry-run"
