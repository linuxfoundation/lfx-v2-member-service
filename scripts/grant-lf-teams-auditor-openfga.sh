#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# grant-lf-teams-auditor-openfga.sh — Grant the configured team subjects
# (the LF staff team by default) the `auditor` relation on every b2b_org in
# the exported census. One-off backfill for orgs that existed before the service
# started asserting these grants on every write. See LFXV2-2937.
#
# Read-diff-write: it reads the tuples each team already holds, diffs against
# the exported UID list, and writes only what is missing. Re-running is safe
# and converges; --dry-run over a completed run reports zero, which makes
# dry-run the reconciliation check rather than a separate tool.
#
# ⚠️  These writes are effectively permanent as far as the service is concerned.
#     fga-sync never deletes a tuple whose subject begins with `team:`, so no
#     code path can undo them. Reversal means running
#     scripts/revoke-lf-teams-auditor-openfga.sh.
#
# Prerequisites:
#   kubectl --context lfx-v2-prod -n lfx port-forward svc/lfx-platform-openfga 8080:8080
#   jq installed
#   ./scripts/export-b2b-org-uids-from-opensearch.sh has been run
#   export LF_STAFF_TEAM_NAME=…   (the only team this script can grant)
#
# Usage:
#   ./scripts/grant-lf-teams-auditor-openfga.sh <store-id> [input_dir] [--dry-run]

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/openfga-team-auditor.sh
source "${SCRIPT_DIR}/lib/openfga-team-auditor.sh"

BASE_URL="${OPENFGA_URL:-http://localhost:8080}"
BATCH_SIZE=100
DRY_RUN=false
IN_DIR="/tmp/lf-team-auditor-backfill"

USAGE="$0 <store-id> [input_dir] [--dry-run]"
fga_require_store_id "${1:-}" "$USAGE"
STORE_ID="$1"
shift

# A case loop rather than positional special-casing, so [input_dir] and
# --dry-run can be given in either order.
while [[ $# -gt 0 ]]; do
	case "$1" in
	--dry-run) DRY_RUN=true ;;
	-h | --help)
		echo "Usage: $USAGE"
		exit 0
		;;
	--*)
		echo "ERROR: unknown flag $1" >&2
		echo "Usage: $USAGE" >&2
		exit 1
		;;
	*) IN_DIR="$1" ;;
	esac
	shift
done

UID_FILE="$IN_DIR/b2b-org-uids.jsonl"
if [[ ! -f "$UID_FILE" ]]; then
	echo "ERROR: $UID_FILE not found — run ./scripts/export-b2b-org-uids-from-opensearch.sh first." >&2
	exit 1
fi

# Collect into a variable first, then split. Feeding fga_team_names through a
# process substitution would discard its exit status — a process substitution's
# status is never part of the enclosing command's, so `set -e` cannot see it and
# a missing env var would leave TEAMS empty and report a clean zero-missing run.
# Command substitution in an assignment does propagate the failure.
#
# Read loop rather than mapfile: mapfile is bash 4+, and macOS ships bash 3.2
# as /bin/bash, which is what an operator running this from a laptop will hit.
#
# Staff only, named explicitly: LF_CONTRACTOR_TEAM_NAME is deliberately out of
# reach here. The revoke script needs that variable, so an operator who has just
# used it can easily still have it exported — and a grant it picked up would
# blanket-grant contractors before LFXV2-3071 decides whether they get access,
# with no service path able to take a team tuple back.
TEAM_NAMES=$(fga_team_names LF_STAFF_TEAM_NAME)
TEAMS=()
while IFS= read -r team_name; do
	TEAMS+=("$team_name")
done <<<"$TEAM_NAMES"

if [[ "$DRY_RUN" == true ]]; then
	echo "=== DRY RUN MODE — no tuples will be written ==="
fi
echo "Store:      $STORE_ID"
echo "Base URL:   $BASE_URL"
echo "Org census: $UID_FILE"
echo "Teams:      ${TEAMS[*]}"
echo ""

CENSUS=$(mktemp)
trap 'rm -f "$CENSUS"' EXIT
jq -r '.uid' "$UID_FILE" | sort -u >"$CENSUS"
echo "→ Orgs in census: $(wc -l <"$CENSUS" | tr -d ' ')"
echo ""

TOTAL_MISSING=0

for team in "${TEAMS[@]}"; do
	subject="team:${team}#member"
	echo "=== ${subject} ==="

	existing=$(mktemp)
	fga_read_org_uids "$subject" | sort -u >"$existing"
	echo "  Existing auditor grants: $(wc -l <"$existing" | tr -d ' ')"

	missing=$(mktemp)
	comm -23 "$CENSUS" "$existing" >"$missing"
	missing_count=$(wc -l <"$missing" | tr -d ' ')
	echo "  Missing: $missing_count"
	TOTAL_MISSING=$((TOTAL_MISSING + missing_count))

	if [[ "$missing_count" -eq 0 ]]; then
		echo "  Nothing to do."
	elif [[ "$DRY_RUN" == true ]]; then
		echo "  [DRY RUN] Would write $missing_count tuples; first 10:"
		head -10 "$missing" | sed 's/^/    b2b_org:/'
	else
		fga_apply_uid_file "writes" "$subject" "$missing" "$missing_count"
	fi

	rm -f "$existing" "$missing"
	echo ""
done

echo "=== Summary ==="
if [[ "$DRY_RUN" == true ]]; then
	echo "Dry run: $TOTAL_MISSING tuples would be written across ${#TEAMS[@]} teams."
	if [[ "$TOTAL_MISSING" -eq 0 ]]; then
		echo "Zero missing — reconciliation check passes."
	fi
else
	echo "Wrote $TOTAL_MISSING tuples. Re-run with --dry-run to confirm zero remaining."
fi
