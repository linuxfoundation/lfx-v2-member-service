#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Migrates the global organization administrator team to its stable OpenFGA
# name through explicit snapshot, plan, apply, verify, and cleanup phases.
# The phase boundaries and hashed artifacts make operator review mandatory
# before writes and prevent cleanup when the reviewed state has drifted.
#
# This script writes principal-bearing artifacts with owner-only permissions.
# Keep output outside the repository and follow the retention guidance in
# docs/global-org-admin-team-migration.md.

set -euo pipefail
umask 077
export LC_ALL=C

MIGRATION_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/openfga-global-org-admin-migration.sh
source "$MIGRATION_SCRIPT_DIR/lib/openfga-global-org-admin-migration.sh"

migration_usage() {
	cat <<'USAGE'
Usage: migrate-global-org-admin-team-openfga.sh PHASE --store-id ID --old-team NAME --output-dir DIR [options]

Phases: snapshot, plan, apply, verify, cleanup
Options:
  --openfga-url URL
  --census-file FILE
  --salesforce-count N
  --approve-census-difference
  --allowed-user USER --denied-user USER --sample-org UID
  --dry-run
  --confirm
  --cutover-complete
USAGE
}

migration_line_count() {
	awk 'NF { count++ } END { print count + 0 }' "$1"
}

migration_validate_manifest() {
	local directory="$1"
	local store_id="$2"
	local old_team="$3"
	[[ -f "$directory/manifest.json" ]] || {
		fga_error "manifest.json is missing; run snapshot first"
		return 6
	}
	local recorded_store recorded_team
	recorded_store=$(jq -r '.store_id // ""' "$directory/manifest.json")
	recorded_team=$(jq -r '.old_team // ""' "$directory/manifest.json")
	if [[ "$recorded_store" != "$store_id" || "$recorded_team" != "$old_team" ]]; then
		fga_error "manifest target does not match the requested store and legacy team"
		return 6
	fi
}

migration_validate_snapshot_hashes() {
	local directory="$1"
	local expected_roster expected_grants expected_checks
	expected_roster=$(jq -r '.legacy_roster_sha256 // ""' "$directory/manifest.json")
	expected_grants=$(jq -r '.legacy_grants_sha256 // ""' "$directory/manifest.json")
	expected_checks=$(jq -r '.baseline_checks_sha256 // ""' "$directory/manifest.json")
	if [[ -z "$expected_roster" || -z "$expected_grants" || -z "$expected_checks" ||
		"$expected_roster" != "$(fga_hash_file "$directory/legacy-roster.jsonl")" ||
		"$expected_grants" != "$(fga_hash_file "$directory/legacy-grants.jsonl")" ||
		"$expected_checks" != "$(fga_hash_file "$directory/baseline-checks.json")" ]]; then
		fga_error "snapshot artifacts changed; take and review a new snapshot"
		return 6
	fi
}

migration_sorted_read() {
	local filter="$1"
	local output="$2"
	# OpenFGA emits an optional null condition on unconditional tuples. Drop only
	# that representation detail so equivalent planned and live tuples compare.
	fga_read_all "$filter" |
		jq -cS 'if .condition == null then del(.condition) else . end' |
		LC_ALL=C sort -u >"$output"
}

migration_require_checks() {
	local required="$1"
	local allowed_user="$2"
	local denied_user="$3"
	local sample_org="$4"
	if [[ -z "$allowed_user" && -z "$denied_user" && -z "$sample_org" ]]; then
		[[ "$required" == false ]] && return 0
		fga_error "authorization controls are required for snapshot and verification"
		return 2
	fi
	if [[ -z "$allowed_user" || -z "$denied_user" || -z "$sample_org" ]]; then
		fga_error "allowed user, denied user, and sample org must be provided together"
		return 2
	fi
}

migration_write_checks() {
	local output="$1"
	local allowed_user="$2"
	local denied_user="$3"
	local sample_org="$4"
	migration_require_checks false "$allowed_user" "$denied_user" "$sample_org" || return $?
	[[ -n "$allowed_user" ]] || return 0
	local allowed denied
	allowed=$(fga_check "user:$allowed_user" writer "b2b_org:$sample_org")
	denied=$(fga_check "user:$denied_user" writer "b2b_org:$sample_org")
	if [[ "$allowed" != true || "$denied" != false ]]; then
		fga_error "authorization controls failed: allowed=$allowed denied=$denied"
		return 4
	fi
	jq -n --arg allowed_user "$allowed_user" --arg denied_user "$denied_user" \
		--arg sample_org "$sample_org" --argjson allowed "$allowed" --argjson denied "$denied" \
		'{allowed_user:$allowed_user,denied_user:$denied_user,sample_org:$sample_org,
		  allowed_result:$allowed,denied_result:$denied}' >"$output"
}

migration_snapshot() {
	local directory="$1"
	local old_team="$2"
	local allowed_user="${3:-}"
	local denied_user="${4:-}"
	local sample_org="${5:-}"
	local dry_run="${6:-false}"
	migration_require_checks true "$allowed_user" "$denied_user" "$sample_org" || return $?
	mkdir -p "$directory"
	chmod 700 "$directory"

	migration_sorted_read \
		"$(jq -n --arg object "team:$old_team" '{relation:"member",object:$object}')" \
		"$directory/legacy-roster.jsonl"
	migration_sorted_read \
		"$(jq -n --arg user "team:$old_team#member" \
			'{user:$user,relation:"global_org_admin",object:"b2b_org:"}')" \
		"$directory/legacy-grants.jsonl"

	migration_write_checks "$directory/baseline-checks.json" "$allowed_user" "$denied_user" "$sample_org"

	local roster_count grant_count total roster_hash grants_hash checks_hash
	roster_count=$(migration_line_count "$directory/legacy-roster.jsonl")
	grant_count=$(migration_line_count "$directory/legacy-grants.jsonl")
	total=$((roster_count + grant_count))
	roster_hash=$(fga_hash_file "$directory/legacy-roster.jsonl")
	grants_hash=$(fga_hash_file "$directory/legacy-grants.jsonl")
	checks_hash=$(fga_hash_file "$directory/baseline-checks.json")
	jq -n --arg store_id "$STORE_ID" --arg old_team "$old_team" \
		--arg stable_team "$FGA_STABLE_TEAM" --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		--argjson roster_count "$roster_count" --argjson grant_count "$grant_count" \
		--argjson source_old_tuple_count "$total" --arg roster_hash "$roster_hash" \
		--arg grants_hash "$grants_hash" --arg checks_hash "$checks_hash" --argjson dry_run "$dry_run" \
		'{store_id:$store_id,old_team:$old_team,stable_team:$stable_team,captured_at:$captured_at,
		  source_roster_count:$roster_count,source_grant_count:$grant_count,
		  source_old_tuple_count:$source_old_tuple_count,legacy_roster_sha256:$roster_hash,
		  legacy_grants_sha256:$grants_hash,baseline_checks_sha256:$checks_hash,dry_run:$dry_run}' \
		>"$directory/manifest.json"
	echo "Snapshot: $roster_count roster, $grant_count grants"
}

migration_write_grant_file() {
	local uid_file="$1"
	local team="$2"
	local output="$3"
	: >"$output"
	while IFS= read -r uid; do
		[[ -z "$uid" ]] && continue
		jq -n -c --arg user "team:$team#member" --arg object "b2b_org:$uid" \
			'{user:$user,relation:"global_org_admin",object:$object}' >>"$output"
	done <"$uid_file"
}

migration_write_plan_summary() {
	local directory="$1"
	local census_uids="$2"
	local orphan_uids="$3"
	local missing_uids="$4"
	local salesforce_count="$5"
	local approve_difference="$6"
	local census_count orphan_count missing_count difference approved roster_hash grants_hash
	census_count=$(migration_line_count "$census_uids")
	orphan_count=$(migration_line_count "$orphan_uids")
	missing_count=$(migration_line_count "$missing_uids")
	difference=$((census_count - salesforce_count))
	approved=false
	[[ "$difference" -eq 0 || "$approve_difference" == true ]] && approved=true
	roster_hash=$(fga_hash_file "$directory/stable-roster-plan.jsonl")
	grants_hash=$(fga_hash_file "$directory/live-grants.jsonl")
	jq -n --argjson census_count "$census_count" --argjson salesforce_count "$salesforce_count" \
		--argjson difference "$difference" --argjson orphan_count "$orphan_count" \
		--argjson missing_source_count "$missing_count" --argjson census_approved "$approved" \
		--arg roster_hash "$roster_hash" --arg grants_hash "$grants_hash" \
		'{census_count:$census_count,salesforce_count:$salesforce_count,census_difference:$difference,
		  census_approved:$census_approved,live_count:$census_count,orphan_count:$orphan_count,
		  missing_source_count:$missing_source_count,stable_roster_sha256:$roster_hash,
		  live_grants_sha256:$grants_hash}' >"$directory/summary.json"
	echo "Plan: $census_count live, $orphan_count orphan, $missing_count missing source"
	if [[ "$approved" != true ]]; then
		fga_error "non-zero census difference requires --approve-census-difference"
		return 4
	fi
}

migration_plan() {
	local directory="$1"
	local census_file="$2"
	local salesforce_count="$3"
	local approve_difference="$4"
	[[ -f "$directory/manifest.json" && -f "$directory/legacy-grants.jsonl" &&
		-f "$directory/legacy-roster.jsonl" && -f "$census_file" ]] || {
		fga_error "snapshot or census artifact is missing"
		return 2
	}
	[[ "$salesforce_count" =~ ^[0-9]+$ ]] || {
		fga_error "salesforce count must be a non-negative integer"
		return 2
	}

	local census_uids source_uids orphan_uids missing_uids
	census_uids=$(mktemp)
	source_uids=$(mktemp)
	orphan_uids=$(mktemp)
	missing_uids=$(mktemp)
	jq -r 'select(.uid | type == "string" and length > 0) | .uid' "$census_file" | LC_ALL=C sort -u >"$census_uids"
	if [[ ! -s "$census_uids" ]]; then
		rm -f "$census_uids" "$source_uids" "$orphan_uids" "$missing_uids"
		fga_error "live-organization census is empty"
		return 4
	fi
	jq -r '.object | sub("^b2b_org:";"")' "$directory/legacy-grants.jsonl" | LC_ALL=C sort -u >"$source_uids"
	comm -23 "$source_uids" "$census_uids" >"$orphan_uids"
	comm -13 "$source_uids" "$census_uids" >"$missing_uids"

	local old_team
	old_team=$(jq -r '.old_team' "$directory/manifest.json")
	migration_write_grant_file "$census_uids" "$FGA_STABLE_TEAM" "$directory/live-grants.jsonl"
	migration_write_grant_file "$orphan_uids" "$old_team" "$directory/orphan-grants.jsonl"
	migration_write_grant_file "$missing_uids" "$FGA_STABLE_TEAM" "$directory/missing-source-grants.jsonl"
	jq -c --arg object "team:$FGA_STABLE_TEAM" '.object = $object' \
		"$directory/legacy-roster.jsonl" >"$directory/stable-roster-plan.jsonl"
	local status=0
	migration_write_plan_summary "$directory" "$census_uids" "$orphan_uids" \
		"$missing_uids" "$salesforce_count" "$approve_difference" || status=$?
	rm -f "$census_uids" "$source_uids" "$orphan_uids" "$missing_uids"
	return "$status"
}

migration_validate_plan_hashes() {
	local directory="$1"
	local expected_roster expected_grants actual_roster actual_grants
	expected_roster=$(jq -r '.stable_roster_sha256 // ""' "$directory/summary.json")
	expected_grants=$(jq -r '.live_grants_sha256 // ""' "$directory/summary.json")
	actual_roster=$(fga_hash_file "$directory/stable-roster-plan.jsonl")
	actual_grants=$(fga_hash_file "$directory/live-grants.jsonl")
	if [[ -z "$expected_roster" || -z "$expected_grants" ||
		"$expected_roster" != "$actual_roster" || "$expected_grants" != "$actual_grants" ]]; then
		fga_error "approved tuple plan changed; rerun plan and review"
		return 6
	fi
}

migration_apply() {
	local directory="$1"
	local dry_run="$2"
	local confirm="$3"
	[[ -f "$directory/summary.json" ]] || {
		fga_error "summary.json is missing; run plan first"
		return 6
	}
	[[ "$(jq -r '.census_approved' "$directory/summary.json")" == true ]] || {
		fga_error "census difference is not approved"
		return 4
	}
	migration_validate_plan_hashes "$directory" || return $?
	[[ "$dry_run" == true || "$confirm" == true ]] || {
		fga_error "apply requires --dry-run or --confirm"
		return 2
	}
	fga_apply_tuple_file writes "$directory/stable-roster-plan.jsonl" "$dry_run"
	fga_apply_tuple_file writes "$directory/live-grants.jsonl" "$dry_run"
	if [[ "$dry_run" == false ]]; then
		local migrated_count summary_tmp
		migrated_count=$(migration_line_count "$directory/live-grants.jsonl")
		summary_tmp=$(mktemp)
		jq --argjson migrated_count "$migrated_count" \
			'. + {migrated_count:$migrated_count}' "$directory/summary.json" >"$summary_tmp"
		mv "$summary_tmp" "$directory/summary.json"
	fi
}

migration_normalize_file() {
	local input="$1"
	local output="$2"
	jq -cS 'if .condition == null then del(.condition) else . end' "$input" |
		LC_ALL=C sort -u >"$output"
}

migration_verify() {
	local directory="$1"
	local allowed_user="${2:-}"
	local denied_user="${3:-}"
	local sample_org="${4:-}"
	migration_require_checks true "$allowed_user" "$denied_user" "$sample_org" || return $?
	[[ -f "$directory/baseline-checks.json" ]] || {
		fga_error "baseline-checks.json is missing; rerun snapshot"
		return 6
	}
	migration_validate_plan_hashes "$directory" || return $?
	local actual_roster actual_grants expected_roster expected_grants
	actual_roster=$(mktemp)
	actual_grants=$(mktemp)
	expected_roster=$(mktemp)
	expected_grants=$(mktemp)
	migration_sorted_read \
		"$(jq -n --arg object "team:$FGA_STABLE_TEAM" '{relation:"member",object:$object}')" \
		"$actual_roster"
	migration_sorted_read \
		"$(jq -n --arg user "team:$FGA_STABLE_TEAM#member" \
			'{user:$user,relation:"global_org_admin",object:"b2b_org:"}')" \
		"$actual_grants"
	local old_team old_roster old_grants
	old_team=$(jq -r '.old_team' "$directory/manifest.json")
	old_roster="$directory/verified-old-roster.jsonl"
	old_grants="$directory/verified-old-grants.jsonl"
	migration_sorted_read \
		"$(jq -n --arg object "team:$old_team" '{relation:"member",object:$object}')" \
		"$old_roster"
	migration_sorted_read \
		"$(jq -n --arg user "team:$old_team#member" \
			'{user:$user,relation:"global_org_admin",object:"b2b_org:"}')" \
		"$old_grants"
	migration_assert_no_new_old_tuples "$directory" "$old_roster" "$old_grants" || return $?
	migration_normalize_file "$directory/stable-roster-plan.jsonl" "$expected_roster"
	migration_normalize_file "$directory/live-grants.jsonl" "$expected_grants"
	local missing_grants extra_grants
	missing_grants=$(mktemp)
	extra_grants=$(mktemp)
	comm -23 "$expected_grants" "$actual_grants" >"$missing_grants"
	comm -13 "$expected_grants" "$actual_grants" >"$extra_grants"
	if ! cmp -s "$actual_roster" "$expected_roster" || [[ -s "$missing_grants" ]]; then
		rm -f "$actual_roster" "$actual_grants" "$expected_roster" "$expected_grants"
		rm -f "$missing_grants" "$extra_grants"
		fga_error "stable team does not match the approved plan"
		return 4
	fi
	rm -f "$actual_roster" "$actual_grants" "$expected_roster" "$expected_grants" "$missing_grants"

	rm -f "$directory/current-checks.json"
	migration_write_checks "$directory/current-checks.json" "$allowed_user" "$denied_user" "$sample_org"
	local baseline_checks current_checks
	baseline_checks=$(jq -cS '.' "$directory/baseline-checks.json")
	current_checks=$(jq -cS '.' "$directory/current-checks.json")
	if [[ "$baseline_checks" != "$current_checks" ]]; then
		rm -f "$extra_grants"
		fga_error "authorization checks differ from baseline"
		return 4
	fi
	local extra_count summary_tmp
	extra_count=$(migration_line_count "$extra_grants")
	mv "$extra_grants" "$directory/post-cutover-extra-stable-grants.jsonl"
	summary_tmp=$(mktemp)
	jq --argjson extra_count "$extra_count" \
		'. + {post_cutover_extra_stable_grant_count:$extra_count}' \
		"$directory/summary.json" >"$summary_tmp"
	mv "$summary_tmp" "$directory/summary.json"
	fga_write_checkpoint "$directory"
	echo "Verification passed; $extra_count post-cutover stable grants recorded"
}

migration_assert_no_new_old_tuples() {
	local directory="$1"
	local current_roster="$2"
	local current_grants="$3"
	local expected_roster expected_grants new_roster new_grants
	expected_roster=$(mktemp)
	expected_grants=$(mktemp)
	new_roster=$(mktemp)
	new_grants=$(mktemp)
	migration_normalize_file "$directory/legacy-roster.jsonl" "$expected_roster"
	migration_normalize_file "$directory/legacy-grants.jsonl" "$expected_grants"
	comm -23 "$current_roster" "$expected_roster" >"$new_roster"
	comm -23 "$current_grants" "$expected_grants" >"$new_grants"
	local new_count
	new_count=$(( $(migration_line_count "$new_roster") + $(migration_line_count "$new_grants") ))
	rm -f "$expected_roster" "$expected_grants"
	if [[ "$new_count" -gt 0 ]]; then
		cp "$new_roster" "$directory/new-old-roster-tuples.jsonl"
		cp "$new_grants" "$directory/new-old-grant-tuples.jsonl"
		rm -f "$new_roster" "$new_grants"
		fga_error "$new_count new old-team tuples appeared after the pre-cutover snapshot"
		return 4
	fi
	rm -f "$new_roster" "$new_grants"
}

migration_cleanup() {
	local directory="$1"
	local old_team="$2"
	local dry_run="$3"
	local confirm="$4"
	local cutover_complete="$5"
	fga_verify_checkpoint "$directory" || return $?
	[[ "$dry_run" == true || "$confirm" == true ]] || {
		fga_error "cleanup requires --dry-run or --confirm"
		return 2
	}
	[[ "$cutover_complete" == true ]] || {
		fga_error "cleanup requires --cutover-complete"
		return 6
	}

	local current_roster current_grants
	current_roster="$directory/precleanup-old-roster.jsonl"
	current_grants="$directory/precleanup-old-grants.jsonl"
	migration_sorted_read \
		"$(jq -n --arg object "team:$old_team" '{relation:"member",object:$object}')" \
		"$current_roster"
	migration_sorted_read \
		"$(jq -n --arg user "team:$old_team#member" \
			'{user:$user,relation:"global_org_admin",object:"b2b_org:"}')" \
		"$current_grants"
	migration_assert_no_new_old_tuples "$directory" "$current_roster" "$current_grants" || return $?
	fga_apply_tuple_file deletes "$current_grants" "$dry_run"
	fga_apply_tuple_file deletes "$current_roster" "$dry_run"
}

migration_parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--store-id) MIGRATION_STORE_ID="${2:-}"; shift 2 ;;
		--old-team) MIGRATION_OLD_TEAM="${2:-}"; shift 2 ;;
		--output-dir) MIGRATION_OUTPUT_DIR="${2:-}"; shift 2 ;;
		--openfga-url) MIGRATION_BASE_URL="${2:-}"; shift 2 ;;
		--census-file) MIGRATION_CENSUS_FILE="${2:-}"; shift 2 ;;
		--salesforce-count) MIGRATION_SALESFORCE_COUNT="${2:-}"; shift 2 ;;
		--allowed-user) MIGRATION_ALLOWED_USER="${2:-}"; shift 2 ;;
		--denied-user) MIGRATION_DENIED_USER="${2:-}"; shift 2 ;;
		--sample-org) MIGRATION_SAMPLE_ORG="${2:-}"; shift 2 ;;
		--approve-census-difference) MIGRATION_APPROVE_DIFFERENCE=true; shift ;;
		--dry-run) MIGRATION_DRY_RUN=true; shift ;;
		--confirm) MIGRATION_CONFIRM=true; shift ;;
		--cutover-complete) MIGRATION_CUTOVER_COMPLETE=true; shift ;;
		*) fga_error "unknown argument: $1"; migration_usage; return 2 ;;
		esac
	done
}

migration_dispatch() {
	case "$MIGRATION_PHASE" in
	snapshot)
		migration_snapshot "$MIGRATION_OUTPUT_DIR" "$MIGRATION_OLD_TEAM" \
			"$MIGRATION_ALLOWED_USER" "$MIGRATION_DENIED_USER" "$MIGRATION_SAMPLE_ORG" \
			"$MIGRATION_DRY_RUN"
		;;
	plan)
		[[ -n "$MIGRATION_CENSUS_FILE" && -n "$MIGRATION_SALESFORCE_COUNT" ]] || {
			fga_error "plan requires --census-file and --salesforce-count"
			return 2
		}
		migration_plan "$MIGRATION_OUTPUT_DIR" "$MIGRATION_CENSUS_FILE" \
			"$MIGRATION_SALESFORCE_COUNT" "$MIGRATION_APPROVE_DIFFERENCE"
		;;
	apply) migration_apply "$MIGRATION_OUTPUT_DIR" "$MIGRATION_DRY_RUN" "$MIGRATION_CONFIRM" ;;
	verify)
		migration_verify "$MIGRATION_OUTPUT_DIR" "$MIGRATION_ALLOWED_USER" \
			"$MIGRATION_DENIED_USER" "$MIGRATION_SAMPLE_ORG"
		;;
	cleanup)
		migration_cleanup "$MIGRATION_OUTPUT_DIR" "$MIGRATION_OLD_TEAM" \
			"$MIGRATION_DRY_RUN" "$MIGRATION_CONFIRM" "$MIGRATION_CUTOVER_COMPLETE"
		;;
	*) fga_error "unknown phase: $MIGRATION_PHASE"; migration_usage; return 2 ;;
	esac
}

migration_main() {
	[[ $# -ge 1 ]] || {
		migration_usage
		return 2
	}
	MIGRATION_PHASE="$1"
	shift
	MIGRATION_STORE_ID="" MIGRATION_OLD_TEAM="" MIGRATION_OUTPUT_DIR=""
	MIGRATION_CENSUS_FILE="" MIGRATION_SALESFORCE_COUNT=""
	MIGRATION_BASE_URL="${OPENFGA_URL:-http://localhost:8080}"
	MIGRATION_APPROVE_DIFFERENCE=false MIGRATION_DRY_RUN=false
	MIGRATION_CONFIRM=false MIGRATION_CUTOVER_COMPLETE=false
	MIGRATION_ALLOWED_USER="" MIGRATION_DENIED_USER="" MIGRATION_SAMPLE_ORG=""
	migration_parse_args "$@" || return $?
	[[ -n "$MIGRATION_STORE_ID" && -n "$MIGRATION_OLD_TEAM" && -n "$MIGRATION_OUTPUT_DIR" ]] || {
		fga_error "--store-id, --old-team, and --output-dir are required"
		return 2
	}
	[[ "$MIGRATION_STORE_ID" =~ ^[A-Za-z0-9_-]+$ ]] || {
		fga_error "--store-id contains unsupported characters"
		return 2
	}
	[[ "$MIGRATION_BASE_URL" =~ ^https?://[^[:space:]]+$ ]] || {
		fga_error "--openfga-url must be an HTTP(S) URL without whitespace"
		return 2
	}
	fga_validate_team_name "$MIGRATION_OLD_TEAM" || return $?
	BASE_URL="$MIGRATION_BASE_URL"
	STORE_ID="$MIGRATION_STORE_ID"
	export BASE_URL STORE_ID
	echo "OpenFGA target: $BASE_URL store=$STORE_ID phase=$MIGRATION_PHASE"
	if [[ "$MIGRATION_PHASE" != snapshot ]]; then
		migration_validate_manifest "$MIGRATION_OUTPUT_DIR" "$STORE_ID" "$MIGRATION_OLD_TEAM" || return $?
		migration_validate_snapshot_hashes "$MIGRATION_OUTPUT_DIR" || return $?
	fi
	migration_dispatch
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	migration_main "$@"
fi
