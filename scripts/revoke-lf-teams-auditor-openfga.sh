#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# revoke-lf-teams-auditor-openfga.sh — Remove the blanket `auditor` grants held
# by the configured team subjects on b2b_org objects. The inverse of
# grant-lf-teams-auditor-openfga.sh. See LFXV2-2937.
#
# This exists because the grant is otherwise irreversible: fga-sync preserves
# team subjects on relations not prefixed `global_`. This grant uses `auditor`,
# so reverting the service code does not remove the tuples — it only stops new
# ones being written.
# Shipping this alongside the grant turns "point of no return" into a
# one-command rollback rather than a script written under incident pressure.
#
# NOT part of the rollout. It is the rollback path. It shares its OpenFGA
# helpers with the grant script (scripts/lib/openfga-team-auditor.sh) so the
# rollback cannot drift away from the path that was actually exercised.
#
# Scope: it deletes only tuples whose subject is exactly one of the configured
# teams. Per-user auditor grants and global_org_admin are never touched.
# Configure the teams narrowly — to revoke one team while leaving another in
# place, export only that team's name.
#
# Prerequisites:
#   Stop EVERY writer of the grant FIRST. There are two:
#   1. member-service, on the API and the CDC consumer both. For lf-contractor
#      this is already done in code — the chart no longer injects
#      LF_CONTRACTOR_TEAM_NAME, so confirm both deployments run a staff-only
#      build; there is no variable to blank. For lf-staff, set
#      lfStaffTeamName to "" and roll out.
#   2. the sync-global-groups reconciler (lfx-v2-argocd), which writes blanket
#      auditor for every team in its in-code orgAuditorTeams set every 10
#      minutes and never deletes. Confirm the running build excludes the team
#      you are revoking — its logs should show `org auditor surplus` for that
#      team and no `org auditor reconcile` line naming it — or set
#      ORG_RECONCILE_ENABLED=false in that environment's overlay.
#   Revoking while either writer is live is a race this script cannot win:
#   any org written during or after the run re-acquires the tuple, and fga-sync
#   will not reap it afterwards because `auditor` is not a `global_*` relation.
#   The residue is invisible — a post-run dry-run reports only what it can see
#   at that instant, so wait at least two reconciler runs (~20 min) before the
#   confirming dry-run.
#
#   kubectl --context lfx-v2-prod -n lfx port-forward svc/lfx-platform-openfga 8080:8080
#   jq installed
#   export LF_STAFF_TEAM_NAME=… and/or LF_CONTRACTOR_TEAM_NAME=…
#     (set only the team you intend to revoke; see fga_team_names)
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
#
# Both teams — deliberately wider than the grant script, which is staff-only
# since the rollback of lfx-self-serve#2157: revoke must be able to target a team
# the service no longer emits (lf-contractor). Set only
# the variable for the team you intend to remove — whichever is left unset is
# left untouched, which is how a single team can be revoked while the other
# keeps its grants.
TEAM_NAMES=$(fga_team_names LF_STAFF_TEAM_NAME LF_CONTRACTOR_TEAM_NAME)
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
	echo "Reminder: this only holds if BOTH writers of the grant are stopped."
	echo "  - member-service (API and CDC consumer): for lf-contractor, confirm both run"
	echo "    a staff-only build; for lf-staff, lfStaffTeamName must be \"\"."
	echo "  - sync-global-groups reconciler: its build must exclude the team (logs show"
	echo "    'org auditor surplus' for it), or ORG_RECONCILE_ENABLED=false."
	echo "Wait at least two reconciler runs (~20 min), then re-run with --dry-run."
fi
