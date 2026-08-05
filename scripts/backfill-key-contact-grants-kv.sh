#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# backfill-key-contact-grants-kv.sh — Seed the key-contact-grants NATS KV
# bucket from the JSONL exported by export-key-contact-grants-from-opensearch.sh.
#
# This does NOT touch Salesforce and does NOT re-publish any FGA member_put:
# the source rows already have a resolved username in OpenSearch, which only
# happens after a member_put was published for them, so the OpenFGA tuple
# already exists. This script only closes the key-contact-grants index gap so
# a future CDC delete for these contacts can address the correct revoke — see
# LFXV2-2907.
#
# Each write uses `nats kv create` (exclusive create — fails if the key already
# exists), mirroring the revision==0 path in
# internal/infrastructure/nats/key_contact_grant_index.go. This makes the
# script safe to re-run and safe to run concurrently with live CDC/API traffic:
# if a real write already populated the entry (either from a previous partial
# run, or from live traffic during the backfill window), `create` fails with
# "key exists" and is treated as an expected skip, never an overwrite of a
# value that may be more current than this export.
#
# Prerequisites:
#   - export-key-contact-grants-from-opensearch.sh already run
#   - nats CLI configured against the target cluster (NATS_URL or
#     `nats context select …`), and the key-contact-grants bucket already
#     created (i.e. the chart change from this fix is already deployed —
#     see charts/lfx-v2-member-service/templates/nats-kv-buckets.yaml)
#
# Usage:
#   ./scripts/backfill-key-contact-grants-kv.sh [input_dir] [--dry-run|--live]
#
# Examples:
#   ./scripts/backfill-key-contact-grants-kv.sh /tmp/key-contact-grants-backfill --dry-run
#   ./scripts/backfill-key-contact-grants-kv.sh /tmp/key-contact-grants-backfill --live

set -euo pipefail

# The failure file (written below) carries LFID usernames from rows that
# failed to create. umask 077 (owner rwx, nothing for group/other) keeps it
# from being world/group-readable under a shared /tmp default input_dir.
umask 077

INPUT_DIR="/tmp/key-contact-grants-backfill"
MODE="dry-run"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) MODE="dry-run"; shift ;;
    --live) MODE="live"; shift ;;
    -*) echo "Unknown option: $1" >&2; exit 1 ;;
    *) INPUT_DIR="$1"; shift ;;
  esac
done

JSONL="$INPUT_DIR/key-contact-grants.jsonl"
FAILURES="$INPUT_DIR/key-contact-grants-failures.jsonl"
BUCKET="key-contact-grants"

if [[ ! -f "$JSONL" ]]; then
  echo "ERROR: input not found: $JSONL" >&2
  echo "Run: ./scripts/export-key-contact-grants-from-opensearch.sh $INPUT_DIR" >&2
  exit 1
fi

if ! command -v nats >/dev/null 2>&1; then
  echo "ERROR: nats CLI not found on PATH" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq not found on PATH" >&2
  exit 1
fi

if ! nats kv ls "$BUCKET" >/dev/null 2>&1; then
  echo "ERROR: bucket '$BUCKET' not reachable — is it created yet? (see charts/lfx-v2-member-service/templates/nats-kv-buckets.yaml)" >&2
  exit 1
fi

TOTAL=$(wc -l <"$JSONL" | tr -d ' ')
echo "→ Bucket:  $BUCKET"
echo "→ Input:   $JSONL ($TOTAL rows)"
echo "→ Mode:    $MODE"
echo ""

if [[ "$MODE" == "dry-run" ]]; then
  echo "[DRY RUN] Would attempt create for $TOTAL keys. Sample:"
  head -n 5 "$JSONL" | jq -c '{key: ("key_contact." + .uid), value: {membership_uid, username}}'
  exit 0
fi

: >"$FAILURES"
created=0
skipped_exists=0
failed=0
processed=0

while IFS= read -r line; do
  processed=$((processed + 1))
  uid=$(echo "$line" | jq -r '.uid')
  key="key_contact.${uid}"
  value=$(echo "$line" | jq -c '{membership_uid, username}')

  if err=$(nats kv create "$BUCKET" "$key" "$value" 2>&1); then
    created=$((created + 1))
  elif echo "$err" | grep -qi "key exists"; then
    skipped_exists=$((skipped_exists + 1))
  else
    failed=$((failed + 1))
    echo "$line" >>"$FAILURES"
    echo "ERROR: create failed for $key: $err" >&2
  fi

  if (( processed % 2000 == 0 )); then
    echo "  … processed ${processed}/${TOTAL} (created=${created} skipped_exists=${skipped_exists} failed=${failed})"
  fi
done <"$JSONL"

echo ""
echo "→ Done. processed=${processed} created=${created} skipped_exists=${skipped_exists} failed=${failed}"
if [[ "$failed" -gt 0 ]]; then
  echo "→ Failed rows written to $FAILURES — to retry, copy it to a fresh dir as key-contact-grants.jsonl and re-run:"
  echo "    mkdir -p /tmp/key-contact-grants-retry && cp $FAILURES /tmp/key-contact-grants-retry/key-contact-grants.jsonl"
  echo "    ./scripts/backfill-key-contact-grants-kv.sh /tmp/key-contact-grants-retry --live"
  exit 1
fi
