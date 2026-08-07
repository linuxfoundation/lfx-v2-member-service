#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# revoke-lf-teams-auditor-openfga.sh — Remove the blanket `auditor` grants held
# by team:lf-staff#member and team:lf-contractor#member on b2b_org objects.
# The inverse of grant-lf-teams-auditor-openfga.sh. See LFXV2-2937.
#
# This exists because the grant is otherwise irreversible: fga-sync never
# deletes a tuple whose subject begins with `team:`, so reverting the service
# code does not remove the tuples — it only stops new ones being written.
# Shipping this alongside the grant turns "point of no return" into a
# one-command rollback rather than a script written under incident pressure.
#
# NOT part of the rollout. It is the rollback path. It shares its OpenFGA
# helpers with the grant script (scripts/lib/openfga-team-auditor.sh) so the
# rollback cannot drift away from the path that was actually exercised.
#
# Scope: it deletes only tuples whose subject is exactly one of the configured
# teams. Per-user auditor grants and global_org_admin are never touched.
#
# Prerequisites:
#   kubectl --context lfx-v2-prod -n lfx port-forward svc/lfx-platform-openfga 8080:8080
#   jq installed
#   export LF_STAFF_TEAM_NAME=… LF_CONTRACTOR_TEAM_NAME=…
#
# Usage:
#   ./scripts/revoke-lf-teams-auditor-openfga.sh <store-id> [--dry-run] [--yes]
#
# Without --dry-run the script prompts for the store ID before deleting. Pass
# --yes to skip the prompt in a runbook or CI context.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/openfga-team-auditor.sh
source "${SCRIPT_DIR}/lib/openfga-team-auditor.sh"

BASE_URL="${OPENFGA_URL:-http://localhost:8080}"
BATCH_SIZE=100
DRY_RUN=false
ASSUME_YES=false

USAGE="$0 <store-id> [--dry-run] [--yes]"
fga_require_store_id "${1:-}" "$USAGE"
STORE_ID="$1"
shift

while [[ $# -gt 0 ]]; do
	case "$1" in
	--dry-run) DRY_RUN=true ;;
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

# Collect into a variable first, then split — a process substitution would
# discard fga_team_names' exit status and leave TEAMS empty, reporting a clean
# "Deleted 0 tuples" for a rollback that did nothing. See the grant script.
#
# Read loop rather than mapfile: mapfile is bash 4+, and macOS ships bash 3.2
# as /bin/bash, which is what an operator running this from a laptop will hit.
TEAM_NAMES=$(fga_team_names)
TEAMS=()
while IFS= read -r team_name; do
	TEAMS+=("$team_name")
done <<<"$TEAM_NAMES"

if [[ "$DRY_RUN" == true ]]; then
	echo "=== DRY RUN MODE — no tuples will be deleted ==="
fi
echo "Store:    $STORE_ID"
echo "Base URL: $BASE_URL"
echo "Teams:    ${TEAMS[*]}"
echo ""

# Confirmation gate. The live form differs from the dry-run form by one flag, so
# recalling the wrong line from shell history deletes every team auditor grant in
# the store. Recovery is asymmetric — a full OpenSearch re-export plus the grant
# backfill — because nothing in the service re-creates tuples for orgs that do
# not subsequently change. --yes exists so CI and runbooks can skip the prompt.
if [[ "$DRY_RUN" == false && "$ASSUME_YES" == false ]]; then
	if [[ ! -t 0 ]]; then
		echo "ERROR: refusing to delete without confirmation on a non-interactive stdin." >&2
		echo "       Re-run with --yes if this is intentional." >&2
		exit 1
	fi
	echo "About to DELETE the auditor grant for ${TEAMS[*]} on every b2b_org in store $STORE_ID."
	read -r -p "Type the store ID to confirm: " confirm
	if [[ "$confirm" != "$STORE_ID" ]]; then
		echo "Aborted — input did not match the store ID." >&2
		exit 1
	fi
	echo ""
fi

TOTAL_TARGETS=0

for team in "${TEAMS[@]}"; do
	subject="team:${team}#member"
	echo "=== ${subject} ==="

	targets=$(mktemp)
	fga_read_org_uids "$subject" | sort -u >"$targets"
	target_count=$(wc -l <"$targets" | tr -d ' ')
	echo "  Auditor grants held: $target_count"
	TOTAL_TARGETS=$((TOTAL_TARGETS + target_count))

	if [[ "$target_count" -eq 0 ]]; then
		echo "  Nothing to do."
	elif [[ "$DRY_RUN" == true ]]; then
		echo "  [DRY RUN] Would delete $target_count tuples; first 10:"
		head -10 "$targets" | sed 's/^/    b2b_org:/'
	else
		fga_apply_uid_file "deletes" "$subject" "$targets" "$target_count"
	fi

	rm -f "$targets"
	echo ""
done

echo "=== Summary ==="
if [[ "$DRY_RUN" == true ]]; then
	echo "Dry run: $TOTAL_TARGETS tuples would be deleted across ${#TEAMS[@]} teams."
else
	echo "Deleted $TOTAL_TARGETS tuples. Re-run with --dry-run to confirm zero remaining."
	echo ""
	echo "Reminder: revert or reconfigure the service too (LF_STAFF_TEAM_NAME/"
	echo "LF_CONTRACTOR_TEAM_NAME set to \"\"), or the next write re-grants them."
fi
