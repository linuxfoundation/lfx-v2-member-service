#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# revoke-dangling-key-contact-grants.sh — Revoke the dangling `key_contact`
# OpenFGA tuples measured by the LFXV2-3265 investigation: a `key_contact`
# tuple on a LIVE project_membership whose underlying key-contact record no
# longer exists. Each one confers `auditor` on the membership and, via
# `key_contact from membership`, on the parent b2b_org. See
# docs/exclude-docs/investigation-LFXV2-3265-dangling-key-contact-grants.md.
#
# This talks to the OpenFGA HTTP API directly — not the fga-sync/NATS
# member_remove path — because this is a single one-off remediation of a
# closed legacy population (LFXV2-2907 fixed the write path; nothing has
# leaked since 2026-08-07), and per-tuple synchronous success/failure is
# exactly what the audit record below needs. See specs/040-lfxv2-3265-
# revoke-dangling-key-contact-grants/plan.md § Complexity Tracking for the
# full justification against Principle II of the platform constitution.
#
# Unlike scripts/revoke-lf-teams-auditor-openfga.sh (which deletes a blanket
# team grant and only needs a total count), this remediation must attribute
# an outcome to each of the 872 individual {membership_uid, username} pairs,
# so it cannot use that script's batched on_missing:ignore delete — a batch
# response cannot tell you *which* member of the batch was already absent.
# Instead each pair gets its own pre-check read, then (in --live mode) its
# own delete, so the outcome is directly attributable per pair.
#
# Rollback: this script has no automated undo. If a revoked grant is later
# found to have been wrongly measured, the JSONL run record's
# {membership_uid, username, relation} for that row is sufficient for an
# engineer to manually re-grant it via the normal key-contact write path
# (member_put) — see docs/fga-contract.md § Key contact relation. This is a
# deliberate manual step, not a button to press (spec.md § Clarifications,
# 2026-08-17).
#
# Prerequisites:
#   kubectl --context lfx-v2-prod -n lfx port-forward svc/lfx-platform-openfga 8080:8080
#   curl, jq installed
#
# Usage:
#   ./scripts/revoke-dangling-key-contact-grants.sh <store-id> <input_tsv> [--dry-run|--live] [--yes]
#
# <input_tsv>: TSV of `membership_uid<TAB>username` pairs, one per line — the
#   investigation's full-dangling.tsv (see docs/exclude-docs/
#   LFXV2-3265-sweep-scripts/investigation-LFXV2-3265-dangling-key-contact-grants.md).
#
# Without --dry-run (the default) nothing is mutated: each pair is read-
# checked against live OpenFGA state and reported, grouped by affected user
# and by affected organization. --live performs the actual deletes and
# prompts for confirmation unless --yes is also given.
#
# Outputs (written under the output dir, default /tmp/lfxv2-3265-revoke):
#   run-record.jsonl   one {membership_uid, username, relation, outcome,
#                      timestamp, detail} line per pair, every run
#   failed.tsv         membership_uid<TAB>username for any "failed" outcome,
#                      written only if at least one pair failed
#
# Examples:
#   ./scripts/revoke-dangling-key-contact-grants.sh 01K3S60BS505DDR3VF9RAZDVHG full-dangling.tsv --dry-run
#   ./scripts/revoke-dangling-key-contact-grants.sh 01K3S60BS505DDR3VF9RAZDVHG full-dangling.tsv --live

set -euo pipefail

# The run record and failure list carry LFID usernames. umask 077 applies the
# same owner-only protection as export-key-contact-grants-from-opensearch.sh
# and backfill-key-contact-grants-kv.sh use for equivalent output.
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/openfga-team-auditor.sh
source "${SCRIPT_DIR}/lib/openfga-team-auditor.sh"

BASE_URL="${OPENFGA_URL:-http://localhost:8080}"
DRY_RUN=true
ASSUME_YES=false
OUT_DIR="${OUT_DIR:-/tmp/lfxv2-3265-revoke}"
PARALLELISM="${PARALLELISM:-10}"

USAGE="$0 <store-id> <input_tsv> [--dry-run|--live] [--yes]"

fga_require_store_id "${1:-}" "$USAGE"
STORE_ID="$1"
shift

INPUT_TSV="${1:-}"
if [[ -z "$INPUT_TSV" || "$INPUT_TSV" == --* ]]; then
	echo "ERROR: the input TSV path is required as the second argument." >&2
	echo "Usage: $USAGE" >&2
	exit 1
fi
shift

while [[ $# -gt 0 ]]; do
	case "$1" in
	--dry-run) DRY_RUN=true ;;
	--live) DRY_RUN=false ;;
	--yes) ASSUME_YES=true ;;
	-h | --help)
		echo "Usage: $USAGE"
		exit 0
		;;
	*)
		echo "ERROR: unknown argument $1" >&2
		echo "Usage: $USAGE" >&2
		exit 1
		;;
	esac
	shift
done

# --- preconditions -----------------------------------------------------

if [[ ! -f "$INPUT_TSV" ]]; then
	echo "ERROR: input file not found: $INPUT_TSV" >&2
	exit 1
fi
if [[ ! -s "$INPUT_TSV" ]]; then
	echo "ERROR: input file is empty: $INPUT_TSV" >&2
	exit 1
fi
for bin in curl jq; do
	if ! command -v "$bin" >/dev/null 2>&1; then
		echo "ERROR: required command not found on PATH: $bin" >&2
		exit 1
	fi
done
# Fail before processing any row rather than partway through, per
# contracts/openfga-revoke-contract.md § Failure modes.
if ! curl -sf -m 5 "${BASE_URL}/stores/${STORE_ID}" >/dev/null 2>&1; then
	echo "ERROR: cannot reach OpenFGA store ${STORE_ID} at ${BASE_URL}." >&2
	echo "       Is the port-forward running? See this script's header comment." >&2
	exit 1
fi

mkdir -p "$OUT_DIR"
RECORD_FILE="${OUT_DIR}/run-record.jsonl"
FAILED_FILE="${OUT_DIR}/failed.tsv"

TOTAL=$(wc -l <"$INPUT_TSV" | tr -d ' ')
echo "Store:       $STORE_ID"
echo "Base URL:    $BASE_URL"
echo "Input:       $INPUT_TSV ($TOTAL rows)"
echo "Mode:        $([[ "$DRY_RUN" == true ]] && echo 'DRY RUN — no mutation' || echo 'LIVE')"
echo "Run record:  $RECORD_FILE"
echo ""

# --- confirmation gate (--live only) ------------------------------------
#
# The live and dry-run forms differ by one flag, same reasoning as
# revoke-lf-teams-auditor-openfga.sh: recalling the wrong shell-history line
# must not silently revoke real users' access. --yes exists for runbooks/CI.
if [[ "$DRY_RUN" == false && "$ASSUME_YES" == false ]]; then
	if [[ ! -t 0 ]]; then
		echo "ERROR: refusing to revoke without confirmation on a non-interactive stdin." >&2
		echo "       Re-run with --yes if this is intentional." >&2
		exit 1
	fi
	echo "About to revoke up to $TOTAL dangling key_contact grants in store $STORE_ID."
	read -r -p "Type the store ID to confirm: " confirm
	if [[ "$confirm" != "$STORE_ID" ]]; then
		echo "Aborted — input did not match the store ID." >&2
		exit 1
	fi
	echo ""
fi

# --- per-pair processing -------------------------------------------------

export BASE_URL STORE_ID DRY_RUN RECORD_FILE
# fga_curl is defined in the sourced lib; each pair runs in its own `xargs
# -P`-spawned subshell, which only inherits functions exported into the
# environment, not ones merely sourced into this shell.
export -f fga_curl

# now() prints an RFC 3339 UTC timestamp for the run record.
now() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }
export -f now

# tuple_present checks whether the specific key_contact tuple still exists.
# contracts/openfga-revoke-contract.md § 1.
tuple_present() {
	local pm="$1" user="$2"
	local body resp
	body=$(jq -n --arg pm "project_membership:${pm}" --arg user "user:${user}" \
		'{"tuple_key": {"object": $pm, "relation": "key_contact", "user": $user}, "page_size": 1}')
	resp=$(fga_curl "read" "$body") || return 2
	[[ "$(echo "$resp" | jq -r '.tuples | length')" -gt 0 ]]
}
export -f tuple_present

# delete_tuple issues the single-tuple delete. contracts/openfga-revoke-contract.md § 2.
delete_tuple() {
	local pm="$1" user="$2"
	local body
	body=$(jq -n --arg pm "project_membership:${pm}" --arg user "user:${user}" \
		'{"deletes": {"tuple_keys": [{"object": $pm, "relation": "key_contact", "user": $user}]}}')
	fga_curl "write" "$body" >/dev/null
}
export -f delete_tuple

# org_for_membership resolves the b2b_org edge for preview grouping only
# (contracts/openfga-revoke-contract.md § 3). Best-effort: preview grouping
# degrading to "(unresolved)" must never fail the whole run.
org_for_membership() {
	local pm="$1"
	local body resp
	body=$(jq -n --arg pm "project_membership:${pm}" \
		'{"tuple_key": {"object": $pm, "relation": "b2b_org"}, "page_size": 10}')
	if ! resp=$(fga_curl "read" "$body" 2>/dev/null); then
		echo "(unresolved)"
		return
	fi
	echo "$resp" | jq -r '[.tuples[]?.key.user] | first // "(unresolved)"' | sed 's/^b2b_org://'
}
export -f org_for_membership

# emit_record writes one JSONL line to stdout (collected by the caller).
emit_record() {
	local pm="$1" user="$2" outcome="$3" detail="${4:-}"
	jq -n --arg pm "$pm" --arg user "$user" --arg outcome "$outcome" \
		--arg ts "$(now)" --arg detail "$detail" \
		'{membership_uid: $pm, username: $user, relation: "key_contact", outcome: $outcome, timestamp: $ts, detail: (if $detail == "" then null else $detail end)}'
}
export -f emit_record

# process_pair handles one row end to end and prints its JSONL record line.
process_pair() {
	local pm="$1" user="$2"

	if [[ -z "$pm" || -z "$user" ]]; then
		emit_record "$pm" "$user" "failed" "malformed row: empty membership_uid or username"
		return
	fi

	local present_check
	if present_check=$(tuple_present "$pm" "$user" 2>&1); then
		present=true
	elif [[ $? -eq 2 ]]; then
		emit_record "$pm" "$user" "failed" "pre-check read failed: ${present_check}"
		return
	else
		present=false
	fi

	if [[ "$present" == false ]]; then
		emit_record "$pm" "$user" "already_clear"
		return
	fi

	if [[ "$DRY_RUN" == true ]]; then
		emit_record "$pm" "$user" "would_revoke"
		return
	fi

	local err
	if err=$(delete_tuple "$pm" "$user" 2>&1); then
		emit_record "$pm" "$user" "revoked"
	else
		emit_record "$pm" "$user" "failed" "$err"
	fi
}
export -f process_pair

echo "Processing $TOTAL pairs ($PARALLELISM-way parallel)..."
: >"$RECORD_FILE"
awk -F'\t' '{print $1"|"$2}' "$INPUT_TSV" |
	xargs -P "$PARALLELISM" -I{} bash -c 'a=${0%%|*}; b=${0#*|}; process_pair "$a" "$b"' {} >>"$RECORD_FILE"

# --- summary -------------------------------------------------------------

REVOKED=$(jq -sr '[.[] | select(.outcome == "revoked")] | length' "$RECORD_FILE")
ALREADY_CLEAR=$(jq -sr '[.[] | select(.outcome == "already_clear")] | length' "$RECORD_FILE")
WOULD_REVOKE=$(jq -sr '[.[] | select(.outcome == "would_revoke")] | length' "$RECORD_FILE")
FAILED=$(jq -sr '[.[] | select(.outcome == "failed")] | length' "$RECORD_FILE")

echo ""
echo "=== Summary ==="
if [[ "$DRY_RUN" == true ]]; then
	echo "Would revoke:  $WOULD_REVOKE"
	echo "Already clear: $ALREADY_CLEAR"
	echo "Failed pre-check: $FAILED"
	echo ""
	echo "=== Preview: affected users (top 10 by count) ==="
	jq -sr '[.[] | select(.outcome == "would_revoke") | .username] | group_by(.) | map({user: .[0], count: length}) | sort_by(-.count) | .[:10][] | "\(.count)\t\(.user)"' "$RECORD_FILE"
	echo ""
	echo "=== Preview: affected organizations (top 10 by count) ==="
	jq -sr '[.[] | select(.outcome == "would_revoke") | .membership_uid] | unique | .[]' "$RECORD_FILE" |
		while IFS= read -r pm; do org_for_membership "$pm"; done |
		sort | uniq -c | sort -rn | head -10
else
	echo "Revoked:       $REVOKED"
	echo "Already clear: $ALREADY_CLEAR"
	echo "Failed:        $FAILED"
	echo ""
	echo "Run record: $RECORD_FILE"
	if [[ "$FAILED" -gt 0 ]]; then
		jq -sr '[.[] | select(.outcome == "failed")] | .[] | "\(.membership_uid)\t\(.username)"' "$RECORD_FILE" >"$FAILED_FILE"
		echo "Failed pairs written to $FAILED_FILE — re-run this script with a TSV built from that file to retry."
	fi
	echo ""
	echo "Next: run the LFXV2-3265 sweep (docs/exclude-docs/LFXV2-3265-sweep-scripts/step12.sh)"
	echo "to confirm 0 dangling grants remain."
fi
