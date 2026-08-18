#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# revoke-dangling-key-contact-grants.sh — Revoke the dangling `key_contact`
# OpenFGA tuples measured by the LFXV2-3265 investigation: a `key_contact`
# tuple on a LIVE project_membership whose underlying key-contact record no
# longer exists. Each one confers `auditor` on the membership and, via
# `key_contact from membership`, on the parent b2b_org. Tracked by LFXV2-3265.
#
# This talks to the OpenFGA HTTP API directly — not the fga-sync/NATS
# member_remove path — because this is a single one-off remediation of a
# closed legacy population (LFXV2-2907 fixed the write path; nothing has
# leaked since 2026-08-07), and per-tuple synchronous success/failure is
# exactly what the audit record below needs.
#
# Freshness revalidation: the input TSV is a snapshot, and a legitimate
# key-contact create/update can re-establish the exact same {membership_uid,
# username} pair after that snapshot was taken (internal/service/
# key_contact_writer.go's Create republishes this FGA tuple). An OpenFGA
# tuple-presence check alone cannot tell "still dangling" apart from "was
# just re-legitimised" — both look identical. So before treating a present
# tuple as dangling, each pair is revalidated against OpenSearch's
# key_contact index — the same operational snapshot source
# scripts/export-key-contact-grants-from-opensearch.sh reads, populated by
# the same PublishKeyContactIndexer call that runs alongside the FGA
# member_put. A live match there means the pair is presently legitimate and
# is skipped, never deleted.
#
# OpenSearch is a secondary index, not Salesforce (the true source of truth):
# export-key-contact-grants-from-opensearch.sh's own header notes the
# indexer and FGA publishes can fail independently, so this check is
# best-effort, not a guarantee, in either direction. This is accepted for a
# single one-off remediation of a closed, already-small population (not a
# recurring/automated job). The investigation compared 104,581 OpenSearch
# key_contact documents and found complete live-population coverage: 1,105
# username-bearing contacts and 1,105 grant-index entries, with zero unmatched
# in either direction. Repeating that full comparison against Salesforce would
# materially consume its guarded API quota, which is reserved for user-facing
# reads; the repository's review policy flags unbatched per-record Salesforce
# fetch loops as unsafe quota consumption. Full Salesforce API integration is
# therefore out of proportion to this scope. Mitigate by exporting
# full-dangling.tsv again shortly before the --live run, minimizing the window
# in which this residual gap could matter; the dry-run preview and the JSONL
# audit trail's manual re-grant path (see Rollback, above) remain the safety net.
#
# Unlike scripts/revoke-lf-teams-auditor-openfga.sh (which deletes a blanket
# team grant and only needs a total count), this remediation must attribute
# an outcome to each of the 872 individual {membership_uid, username} pairs,
# so it cannot use that script's batched delete — a batch response cannot tell
# you *which* member of the batch was already absent. Instead each pair gets
# its own pre-check read, then an idempotent on_missing:ignore delete in
# --live mode, so the outcome is directly attributable per pair.
#
# Rollback: this script has no automated undo. If a revoked grant is later
# found to have been wrongly measured, the JSONL run record's
# {membership_uid, username, relation} for that row is sufficient for an
# engineer to manually re-grant it via the normal key-contact write path
# (member_put) — see docs/fga-contract.md § Key contact relation. This is a
# deliberate manual step, not a button to press.
#
# Prerequisites:
#   kubectl --context lfx-v2-prod -n lfx port-forward svc/lfx-platform-openfga 8080:8080
#   kubectl --context lfx-v2-prod -n lfx port-forward pod/opensearch-proxy-… 9299:9200
#   curl, jq installed
#
# Usage:
#   ./scripts/revoke-dangling-key-contact-grants.sh <store-id> <input_tsv> [--live] [--yes]
#
# <input_tsv>: TSV of `membership_uid<TAB>username` pairs, one per line.
#
# By default nothing is mutated: each pair is read-
# checked against live OpenFGA state and reported, grouped by affected user
# and by affected organization. --live performs the actual deletes and
# prompts for confirmation unless --yes is also given.
#
# Outputs: each invocation gets its own timestamped subdirectory under the
# output dir (default /tmp/lfxv2-3265-revoke), so a re-run never overwrites a
# prior run's evidence:
#   run-record.jsonl   one compact-JSON {membership_uid, username, relation,
#                      outcome, timestamp, detail} line per pair, every run.
#                      outcome is one of: revoked, would_revoke, already_clear,
#                      skipped_live_contact (tuple present but revalidated as
#                      currently legitimate), failed.
#   failed.tsv         membership_uid<TAB>username for any "failed" outcome,
#                      written only if at least one pair failed
#
# Exit status is nonzero if any pair's outcome was "failed", in either mode.
#
# Examples:
#   ./scripts/revoke-dangling-key-contact-grants.sh 01K3S60BS505DDR3VF9RAZDVHG full-dangling.tsv
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
OPENSEARCH_URL="${OPENSEARCH_URL:-http://localhost:9299}"
OPENSEARCH_INDEX="${OPENSEARCH_INDEX:-resources}"
DRY_RUN=true
ASSUME_YES=false
OUT_DIR="${OUT_DIR:-/tmp/lfxv2-3265-revoke}"
PARALLELISM="${PARALLELISM:-10}"

USAGE="$0 <store-id> <input_tsv> [--live] [--yes]"

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
if [[ "$DRY_RUN" == true && "$ASSUME_YES" == true ]]; then
	echo "ERROR: --yes is valid only with --live." >&2
	echo "Usage: $USAGE" >&2
	exit 1
fi
if [[ ! "$PARALLELISM" =~ ^[1-9][0-9]*$ ]]; then
	echo "ERROR: PARALLELISM must be a positive integer, got: $PARALLELISM" >&2
	exit 1
fi

# --- preconditions -----------------------------------------------------

if [[ ! -f "$INPUT_TSV" ]]; then
	echo "ERROR: input file not found: $INPUT_TSV" >&2
	exit 1
fi
if [[ ! -s "$INPUT_TSV" ]]; then
	echo "ERROR: input file is empty: $INPUT_TSV" >&2
	exit 1
fi
if ! awk -F'\t' '
	{ sub(/\r$/, "", $2) }
	NF != 2 || $1 == "" || $2 == "" {
		printf "ERROR: malformed TSV row %d; expected non-empty membership_uid<TAB>username\n", NR > "/dev/stderr"
		invalid = 1
	}
	END { exit invalid }
' "$INPUT_TSV"; then
	exit 1
fi
for bin in curl jq; do
	if ! command -v "$bin" >/dev/null 2>&1; then
		echo "ERROR: required command not found on PATH: $bin" >&2
		exit 1
	fi
done
# Fail before processing any row rather than partway through.
if ! curl -sf -m 5 "${BASE_URL}/stores/${STORE_ID}" >/dev/null 2>&1; then
	echo "ERROR: cannot reach OpenFGA store ${STORE_ID} at ${BASE_URL}." >&2
	echo "       Is the port-forward running? See this script's header comment." >&2
	exit 1
fi
if ! curl -sf -m 5 "${OPENSEARCH_URL}/${OPENSEARCH_INDEX}/_count" >/dev/null 2>&1; then
	echo "ERROR: cannot reach OpenSearch index ${OPENSEARCH_INDEX} at ${OPENSEARCH_URL}." >&2
	echo "       Is the port-forward running? See this script's header comment." >&2
	exit 1
fi
mapping=$(curl -sf -m 5 "${OPENSEARCH_URL}/${OPENSEARCH_INDEX}/_mapping") || {
	echo "ERROR: cannot read OpenSearch mapping for ${OPENSEARCH_INDEX}." >&2
	exit 1
}
if ! echo "$mapping" | jq -e '
	[.[] | {
		object_type: .mappings.properties.object_type.type,
		membership_uid: .mappings.properties.data.properties.membership_uid.type,
		username: .mappings.properties.data.properties.username.type
	}] | length > 0 and all(
		.object_type == "keyword" and
		.membership_uid == "keyword" and
		.username == "keyword"
	)
' >/dev/null 2>&1; then
	echo "ERROR: OpenSearch fields object_type, data.membership_uid, and data.username must be keyword-mapped; exact revalidation is unsafe." >&2
	exit 1
fi

# Each invocation gets its own subdirectory (timestamp + pid) so a retry or
# idempotency re-run never overwrites a prior run's revoked/failed evidence —
# that evidence is the only rollback path this script has (see header comment).
RUN_DIR="${OUT_DIR}/$(date -u +"%Y%m%dT%H%M%SZ")-$$"
mkdir -p "$RUN_DIR"
RECORD_FILE="${RUN_DIR}/run-record.jsonl"
FAILED_FILE="${RUN_DIR}/failed.tsv"

TOTAL=$(awk 'END { print NR }' "$INPUT_TSV")
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

export BASE_URL STORE_ID DRY_RUN OPENSEARCH_URL OPENSEARCH_INDEX
# fga_curl is defined in the sourced lib; each pair runs in its own `xargs
# -P`-spawned subshell, which only inherits functions exported into the
# environment, not ones merely sourced into this shell.
export -f fga_curl

# now() prints an RFC 3339 UTC timestamp for the run record.
now() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }
export -f now

# tuple_present checks whether the specific key_contact tuple still exists.
# Returns 0 when present, 1 when absent, and 2 on an infrastructure or
# response-validation error. Fails closed (return 2) on a 200
# response with a missing/malformed `tuples` array, rather than letting a
# broken response silently read as absent (already_clear).
tuple_present() {
	local pm="$1" user="$2"
	local body resp
	body=$(jq -n --arg pm "project_membership:${pm}" --arg user "user:${user}" \
		'{"tuple_key": {"object": $pm, "relation": "key_contact", "user": $user}, "page_size": 1}')
	resp=$(fga_curl "read" "$body") || return 2
	if ! echo "$resp" | jq -e '(.tuples | type) == "array"' >/dev/null 2>&1; then
		return 2
	fi
	[[ "$(echo "$resp" | jq -r '.tuples | length')" -gt 0 ]]
}
export -f tuple_present

# currently_live checks whether {pm, user} is presently backed by a live
# key_contact record in OpenSearch — see the freshness-revalidation header
# comment. Returns 0 when live, 1 when not live, and 2 on an infrastructure
# or response-validation error. Fails closed on return 2, same reasoning as
# tuple_present.
currently_live() {
	local pm="$1" user="$2"
	local body resp
	body=$(jq -n --arg pm "$pm" --arg user "$user" \
		'{"size": 0, "query": {"bool": {"must": [
			{"term": {"object_type": "key_contact"}},
			{"term": {"data.membership_uid": $pm}},
			{"term": {"data.username": $user}}
		]}}}')
	resp=$(curl -sf -m 10 -X POST "${OPENSEARCH_URL}/${OPENSEARCH_INDEX}/_search" \
		-H "Content-Type: application/json" -d "$body") || return 2
	if ! echo "$resp" | jq -e \
		'(.timed_out == false) and
		((._shards | type) == "object") and
		((._shards.total | type) == "number") and
		((._shards.successful | type) == "number") and
		((._shards.skipped | type) == "number") and
		((._shards.failed | type) == "number") and
		(._shards.failed == 0) and
		(._shards.successful + ._shards.failed + ._shards.skipped == ._shards.total) and
		((.hits.total.value | type) == "number")' \
		>/dev/null 2>&1; then
		return 2
	fi
	[[ "$(echo "$resp" | jq -r '.hits.total.value')" -gt 0 ]]
}
export -f currently_live

# delete_tuple issues the single-tuple delete.
delete_tuple() {
	local pm="$1" user="$2"
	local body
	body=$(jq -n --arg pm "project_membership:${pm}" --arg user "user:${user}" \
		'{"deletes": {"tuple_keys": [{"object": $pm, "relation": "key_contact", "user": $user}], "on_missing": "ignore"}}')
	fga_curl "write" "$body" >/dev/null
}
export -f delete_tuple

# org_for_membership resolves the b2b_org edge for preview grouping only
# and is best-effort: preview grouping degrading to "(unresolved)" must never
# fail the whole run.
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

# emit_record writes one compact (single-line) JSON record to stdout,
# collected by the caller. Compact (-c) is required, not cosmetic: many
# process_pair invocations run concurrently under `xargs -P` and append to the
# same $RECORD_FILE, so a pretty-printed multi-line object here would let two
# workers' writes interleave mid-object and corrupt the JSONL audit trail.
emit_record() {
	local pm="$1" user="$2" outcome="$3" detail="${4:-}"
	jq -nc --arg pm "$pm" --arg user "$user" --arg outcome "$outcome" \
		--arg ts "$(now)" --arg detail "$detail" \
		'{membership_uid: $pm, username: $user, relation: "key_contact", outcome: $outcome, timestamp: $ts, detail: (if $detail == "" then null else $detail end)}'
}
export -f emit_record

# process_pair handles one row end to end and prints its JSONL record line.
process_pair() {
	local pm="$1" user="$2"
	local present live

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

	local live_check
	if live_check=$(currently_live "$pm" "$user" 2>&1); then
		live=true
	elif [[ $? -eq 2 ]]; then
		emit_record "$pm" "$user" "failed" "revalidation read failed: ${live_check}"
		return
	else
		live=false
	fi

	if [[ "$live" == true ]]; then
		emit_record "$pm" "$user" "skipped_live_contact" "OpenSearch shows a current key_contact record for this pair; not dangling"
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
PROCESS_STATUS=0
# $1/$2 expand in each xargs-spawned shell.
# shellcheck disable=SC2016
while IFS=$'\t' read -r pm user || [[ -n "$pm$user" ]]; do
	user=${user%$'\r'}
	printf '%s\0%s\0' "$pm" "$user"
done <"$INPUT_TSV" |
	xargs -0 -P "$PARALLELISM" -n 2 bash -c 'process_pair "$1" "$2"' _ \
		>>"$RECORD_FILE" || PROCESS_STATUS=$?

# --- summary -------------------------------------------------------------

RECORD_COUNT=$(awk 'END { print NR }' "$RECORD_FILE")
if [[ "$RECORD_COUNT" -ne "$TOTAL" ]]; then
	echo "ERROR: processed record count ${RECORD_COUNT} does not match input row count ${TOTAL}." >&2
	PROCESS_STATUS=1
fi
if ! jq -se 'all(.[]; type == "object" and (.outcome | type) == "string")' "$RECORD_FILE" >/dev/null; then
	echo "ERROR: run record is not valid JSONL; summary unavailable. Inspect $RECORD_FILE." >&2
	exit 1
fi

REVOKED=$(jq -sr '[.[] | select(.outcome == "revoked")] | length' "$RECORD_FILE")
ALREADY_CLEAR=$(jq -sr '[.[] | select(.outcome == "already_clear")] | length' "$RECORD_FILE")
SKIPPED_LIVE=$(jq -sr '[.[] | select(.outcome == "skipped_live_contact")] | length' "$RECORD_FILE")
WOULD_REVOKE=$(jq -sr '[.[] | select(.outcome == "would_revoke")] | length' "$RECORD_FILE")
FAILED=$(jq -sr '[.[] | select(.outcome == "failed")] | length' "$RECORD_FILE")

echo ""
echo "=== Summary ==="
if [[ "$DRY_RUN" == true ]]; then
	echo "Would revoke:      $WOULD_REVOKE"
	echo "Already clear:     $ALREADY_CLEAR"
	echo "Skipped (currently live): $SKIPPED_LIVE"
	echo "Failed:            $FAILED"
	echo ""
	echo "=== Preview: affected users (top 10 by count) ==="
	jq -sr '[.[] | select(.outcome == "would_revoke") | .username] | group_by(.) | map({user: .[0], count: length}) | sort_by(-.count) | .[:10][] | "\(.count)\t\(.user)"' "$RECORD_FILE"
	echo ""
	echo "=== Preview: affected organizations (top 10 by count) ==="
	jq -sr '[.[] | select(.outcome == "would_revoke") | .membership_uid] | unique | .[]' "$RECORD_FILE" |
		while IFS= read -r pm; do org_for_membership "$pm"; done |
		sort | uniq -c | sort -rn | head -10
else
	echo "Revoked:           $REVOKED"
	echo "Already clear:     $ALREADY_CLEAR"
	echo "Skipped (currently live): $SKIPPED_LIVE"
	echo "Failed:            $FAILED"
	echo ""
	echo "Run record: $RECORD_FILE"
	echo ""
	echo "Next: rerun the independent sweep used to produce the input TSV"
	echo "and confirm that 0 dangling grants remain."
fi

if [[ "$FAILED" -gt 0 ]]; then
	jq -sr '[.[] | select(.outcome == "failed")] | .[] | "\(.membership_uid)\t\(.username)"' "$RECORD_FILE" >"$FAILED_FILE"
	echo "Failed pairs written to $FAILED_FILE — re-run this script with a TSV built from that file to retry."
fi
if [[ "$PROCESS_STATUS" -ne 0 ]]; then
	echo "ERROR: one or more workers exited unexpectedly (xargs status ${PROCESS_STATUS}); inspect $RECORD_FILE." >&2
fi

# A "failed" outcome (in either mode) must not report success: it means an
# API pre-check, revalidation, or delete could not be completed, which an
# operator/runbook must not treat as a clean remediation.
if [[ "$FAILED" -gt 0 || "$PROCESS_STATUS" -ne 0 ]]; then
	exit 1
fi
