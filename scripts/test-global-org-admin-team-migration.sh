#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Deterministic fixture suite for migration safety gates and OpenFGA request
# shapes. Network calls are replaced with local shell functions, so this suite
# is safe for CI and does not require an OpenFGA server or credentials.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/openfga-global-org-admin-migration.sh
source "$SCRIPT_DIR/lib/openfga-global-org-admin-migration.sh"
# shellcheck source=migrate-global-org-admin-team-openfga.sh
source "$SCRIPT_DIR/migrate-global-org-admin-team-openfga.sh"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_eq() {
	local want="$1"
	local got="$2"
	local message="$3"
	[[ "$got" == "$want" ]] || fail "$message: want=$want got=$got"
}

test_validate_team_name() {
	fga_validate_team_name "legacy-id"
	if fga_validate_team_name "team:legacy-id" 2>/dev/null; then
		fail "prefixed team name must be rejected"
	fi
	if fga_validate_team_name "global_org_admin" 2>/dev/null; then
		fail "stable team cannot be used as the legacy team"
	fi
	if fga_validate_team_name "legacy team" 2>/dev/null; then
		fail "team names containing whitespace must be rejected"
	fi
}

test_pagination() {
	local call_log output
	call_log=$(mktemp)
	output=$(mktemp)
	trap 'rm -f "$call_log" "$output"' RETURN
	fga_request() {
		local body="$2"
		printf 'call\n' >>"$call_log"
		if [[ "$(printf '%s' "$body" | jq -r '.continuation_token // ""')" == "" ]]; then
			printf '%s\n' '{"tuples":[{"key":{"user":"user:alice","relation":"member","object":"team:old"}}],"continuation_token":"next"}'
		else
			printf '%s\n' '{"tuples":[{"key":{"user":"user:bob","relation":"member","object":"team:old"}}],"continuation_token":""}'
		fi
	}

	fga_read_all '{"relation":"member","object":"team:old"}' >"$output"
	assert_eq "2" "$(jq -s 'length' "$output")" "pagination must return both pages"
	assert_eq "2" "$(wc -l <"$call_log" | tr -d ' ')" "pagination must call both pages"
}

test_malformed_response_fails() {
	fga_request() { printf '%s\n' '{"unexpected":true}'; }
	if fga_read_all '{"relation":"member"}' >/dev/null 2>&1; then
		fail "response without tuples must fail"
	fi
}

test_repeated_pagination_token_fails() {
	fga_request() {
		printf '%s\n' '{"tuples":[],"continuation_token":"repeat"}'
	}
	local status=0
	fga_read_all '{"relation":"member"}' >/dev/null 2>&1 || status=$?
	assert_eq "3" "$status" "repeated continuation tokens must fail"
}

test_all_pagination_tokens_are_tracked() {
	local seen
	seen=$(mktemp)
	trap 'rm -f "$seen"' RETURN
	fga_remember_token "$seen" "token-a"
	fga_remember_token "$seen" "token-b"
	local status=0
	fga_remember_token "$seen" "token-a" >/dev/null 2>&1 || status=$?
	assert_eq "3" "$status" "non-consecutive repeated continuation tokens must fail"
}

test_script_forces_c_locale() {
	local locale
	locale=$(LC_ALL=en_US.UTF-8 /bin/bash -c \
		'source "$1"; printf "%s" "$LC_ALL"' _ \
		"$SCRIPT_DIR/migrate-global-org-admin-team-openfga.sh")
	assert_eq "C" "$locale" "migration set operations must use C collation"
}

test_batches_and_dry_run() {
	local tmp
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' RETURN
	jq -n -c 'range(0; 205) | {user:("user:" + tostring),relation:"member",object:"team:global_org_admin"}' >"$tmp"

	local writes=0
	local max_batch=0
	fga_request() {
		local endpoint="$1"
		local body="$2"
		if [[ "$endpoint" == "write" ]]; then
			writes=$((writes + 1))
			local size
			size=$(printf '%s' "$body" | jq '.writes.tuple_keys | length')
			((size > max_batch)) && max_batch=$size
		fi
		printf '%s\n' '{}'
	}

	fga_apply_tuple_file writes "$tmp" true >/dev/null
	assert_eq "0" "$writes" "dry-run must send no writes"

	fga_apply_tuple_file writes "$tmp" false >/dev/null
	assert_eq "3" "$writes" "205 tuples must use three batches"
	assert_eq "100" "$max_batch" "batch size must not exceed 100"
}

test_idempotency_options() {
	local tmp
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' RETURN
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:global_org_admin"}' >"$tmp"
	fga_request() {
		local body="$2"
		if printf '%s' "$body" | jq -e '.writes' >/dev/null; then
			assert_eq "ignore" "$(printf '%s' "$body" | jq -r '.writes.on_duplicate')" \
				"writes must ignore existing tuples"
		else
			assert_eq "ignore" "$(printf '%s' "$body" | jq -r '.deletes.on_missing')" \
				"deletes must ignore missing tuples"
		fi
		printf '%s\n' '{}'
	}
	fga_apply_tuple_file writes "$tmp" false
	fga_apply_tuple_file deletes "$tmp" false
}

test_batch_failure_propagates() {
	local tmp
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' RETURN
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:global_org_admin"}' >"$tmp"
	fga_request() { return 5; }
	local status=0
	fga_apply_tuple_file writes "$tmp" false >/dev/null || status=$?
	assert_eq "5" "$status" "write failure must propagate"
}

test_apply_records_migrated_count() {
	local dir
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:global_org_admin"}' \
		>"$dir/stable-roster-plan.jsonl"
	printf '%s\n' \
		'{"user":"team:global_org_admin#member","relation":"global_org_admin","object":"b2b_org:one"}' \
		>"$dir/live-grants.jsonl"
	jq -n --arg roster "$(fga_hash_file "$dir/stable-roster-plan.jsonl")" \
		--arg grants "$(fga_hash_file "$dir/live-grants.jsonl")" \
		'{census_approved:true,stable_roster_sha256:$roster,live_grants_sha256:$grants}' \
		>"$dir/summary.json"
	fga_request() { printf '%s\n' '{}'; }
	migration_apply "$dir" false true
	assert_eq "1" "$(jq -r '.migrated_count' "$dir/summary.json")" \
		"confirmed apply must record migrated grant count"
}

test_cleanup_requires_checkpoint() {
	local dir status=0
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	migration_cleanup "$dir" old true false true >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "cleanup without a verified checkpoint must fail"
}

test_checkpoint_guard() {
	local dir
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	printf '%s\n' '{"store_id":"store","old_team":"old","source_old_tuple_count":2}' >"$dir/manifest.json"
	printf '%s\n' '{"live_count":1}' >"$dir/summary.json"
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:old"}' >"$dir/legacy-roster.jsonl"
	printf '%s\n' \
		'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:one"}' \
		>"$dir/legacy-grants.jsonl"
	fga_write_checkpoint "$dir"
	fga_verify_checkpoint "$dir"
	printf '%s\n' '{"live_count":2}' >"$dir/summary.json"
	if fga_verify_checkpoint "$dir" 2>/dev/null; then
		fail "changed summary must invalidate checkpoint"
	fi
}

test_plan_classifies_grants() {
	local dir census
	dir=$(mktemp -d)
	census=$(mktemp)
	trap 'rm -rf "$dir"; rm -f "$census"' RETURN
	printf '%s\n' '{"store_id":"store","old_team":"old","source_old_tuple_count":3}' >"$dir/manifest.json"
	printf '%s\n' \
		'{"user":"user:alice","relation":"member","object":"team:old"}' \
		>"$dir/legacy-roster.jsonl"
	printf '%s\n' \
		'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:live"}' \
		'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:orphan"}' \
		>"$dir/legacy-grants.jsonl"
	printf '%s\n' '{"uid":"live"}' '{"uid":"missing"}' >"$census"

	migration_plan "$dir" "$census" 2 true >/dev/null

	assert_eq "2" "$(wc -l <"$dir/live-grants.jsonl" | tr -d ' ')" "every census org must receive a stable grant"
	assert_eq "1" "$(wc -l <"$dir/orphan-grants.jsonl" | tr -d ' ')" "one grant must be orphaned"
	assert_eq "team:old#member" "$(jq -r '.user' "$dir/orphan-grants.jsonl")" "orphan report must preserve source subject"
	assert_eq "1" "$(wc -l <"$dir/missing-source-grants.jsonl" | tr -d ' ')" "one live org must be missing at source"
	assert_eq "true" "$(jq -r '.census_approved' "$dir/summary.json")" "approved difference must unblock plan"
	printf '%s\n' '{"user":"user:mallory","relation":"member","object":"team:global_org_admin"}' \
		>>"$dir/stable-roster-plan.jsonl"
	if migration_apply "$dir" true false >/dev/null 2>&1; then
		fail "apply must reject plan files changed after approval"
	fi
}

test_plan_rejects_empty_census() {
	local dir census status=0
	dir=$(mktemp -d)
	census=$(mktemp)
	trap 'rm -rf "$dir"; rm -f "$census"' RETURN
	printf '%s\n' '{"store_id":"store","old_team":"old","source_old_tuple_count":1}' >"$dir/manifest.json"
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:old"}' >"$dir/legacy-roster.jsonl"
	printf '%s\n' \
		'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:one"}' \
		>"$dir/legacy-grants.jsonl"
	migration_plan "$dir" "$census" 0 false >/dev/null 2>&1 || status=$?
	assert_eq "4" "$status" "an empty live-organization census must block planning"
}

test_cleanup_blocks_old_write_increase() {
	local dir
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	printf '%s\n' '{"store_id":"store","old_team":"old","source_old_tuple_count":2}' >"$dir/manifest.json"
	printf '%s\n' '{"live_count":1}' >"$dir/summary.json"
	fga_write_checkpoint "$dir"
	fga_read_all() {
		printf '%s\n' \
			'{"user":"user:alice","relation":"member","object":"team:old"}' \
			'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:one"}' \
			'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:new"}'
	}
	if migration_cleanup "$dir" "old" true true true >/dev/null 2>&1; then
		fail "cleanup must block when old tuple count increased"
	fi
}

test_cleanup_blocks_compensating_tuple_drift() {
	local dir
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	printf '%s\n' '{"store_id":"store","old_team":"old","source_old_tuple_count":2}' >"$dir/manifest.json"
	printf '%s\n' '{"live_count":1}' >"$dir/summary.json"
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:old"}' >"$dir/legacy-roster.jsonl"
	printf '%s\n' \
		'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:one"}' \
		>"$dir/legacy-grants.jsonl"
	fga_write_checkpoint "$dir"
	fga_read_all() {
		if [[ "$(printf '%s' "$1" | jq -r '.relation')" == member ]]; then
			printf '%s\n' '{"user":"user:bob","relation":"member","object":"team:old"}'
		else
			printf '%s\n' '{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:two"}'
		fi
	}
	if migration_cleanup "$dir" "old" true true true >/dev/null 2>&1; then
		fail "cleanup must block when new old-team tuples replace snapshotted tuples"
	fi
}

test_baseline_checks_require_allowed_and_denied_controls() {
	local output
	output=$(mktemp)
	trap 'rm -f "$output"' RETURN
	fga_check() { printf '%s\n' true; }
	if migration_write_checks "$output" admin nonadmin org >/dev/null 2>&1; then
		fail "baseline must reject a denied control that is allowed"
	fi
}

test_authorization_check_arguments_are_all_or_none() {
	local output
	output=$(mktemp)
	trap 'rm -f "$output"' RETURN
	if migration_write_checks "$output" admin "" org >/dev/null 2>&1; then
		fail "partial authorization controls must be rejected"
	fi
	local status=0
	migration_require_checks true "" "" "" >/dev/null 2>&1 || status=$?
	assert_eq "2" "$status" "verification with a baseline must require all authorization controls"
}

test_manifest_must_match_target() {
	local dir
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	printf '%s\n' '{"store_id":"store-a","old_team":"old-a"}' >"$dir/manifest.json"
	local status=0
	migration_validate_manifest "$dir" "store-b" "old-a" >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "store mismatch must block the phase"
	status=0
	migration_validate_manifest "$dir" "store-a" "old-b" >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "legacy team mismatch must block the phase"
}

test_snapshot_hashes_detect_tampering() {
	local dir roster_hash grants_hash checks_hash
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	printf '%s\n' '{"user":"user:alice"}' >"$dir/legacy-roster.jsonl"
	printf '%s\n' '{"object":"b2b_org:one"}' >"$dir/legacy-grants.jsonl"
	printf '%s\n' '{"allowed_result":true}' >"$dir/baseline-checks.json"
	roster_hash=$(fga_hash_file "$dir/legacy-roster.jsonl")
	grants_hash=$(fga_hash_file "$dir/legacy-grants.jsonl")
	checks_hash=$(fga_hash_file "$dir/baseline-checks.json")
	jq -n --arg roster "$roster_hash" --arg grants "$grants_hash" --arg checks "$checks_hash" \
		'{legacy_roster_sha256:$roster,legacy_grants_sha256:$grants,baseline_checks_sha256:$checks}' \
		>"$dir/manifest.json"
	migration_validate_snapshot_hashes "$dir"
	printf '%s\n' '{"user":"user:mallory"}' >>"$dir/legacy-roster.jsonl"
	if migration_validate_snapshot_hashes "$dir" >/dev/null 2>&1; then
		fail "changed snapshot artifacts must be rejected"
	fi
}

test_verify_accepts_equivalent_tuple_sets() {
	local dir call_log
	dir=$(mktemp -d)
	call_log=$(mktemp)
	trap 'rm -rf "$dir"; rm -f "$call_log"' RETURN
	printf '%s\n' '{"store_id":"store","old_team":"old","source_old_tuple_count":2}' >"$dir/manifest.json"
	printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:old"}' >"$dir/legacy-roster.jsonl"
	printf '%s\n' \
		'{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:one"}' \
		>"$dir/legacy-grants.jsonl"
	printf '%s\n' \
		'{"user":"user:alice","relation":"member","object":"team:global_org_admin"}' \
		>"$dir/stable-roster-plan.jsonl"
	printf '%s\n' \
		'{"user":"team:global_org_admin#member","relation":"global_org_admin","object":"b2b_org:one"}' \
		>"$dir/live-grants.jsonl"
	printf '%s\n' \
		'{"allowed_user":"admin","denied_user":"nonadmin","sample_org":"one","allowed_result":true,"denied_result":false}' \
		>"$dir/baseline-checks.json"
	jq -n --arg roster "$(fga_hash_file "$dir/stable-roster-plan.jsonl")" \
		--arg grants "$(fga_hash_file "$dir/live-grants.jsonl")" \
		'{census_approved:true,stable_roster_sha256:$roster,live_grants_sha256:$grants}' \
		>"$dir/summary.json"
	fga_read_all() {
		printf '%s' "$1" | jq -c '.' >>"$call_log"
		local object user
		object=$(printf '%s' "$1" | jq -r '.object // ""')
		user=$(printf '%s' "$1" | jq -r '.user // ""')
		if [[ "$object" == "team:old" ]]; then
			printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:old","condition":null}'
		elif [[ "$object" == "team:global_org_admin" ]]; then
			printf '%s\n' '{"user":"user:alice","relation":"member","object":"team:global_org_admin","condition":null}'
		elif [[ "$user" == "team:old#member" ]]; then
			printf '%s\n' '{"user":"team:old#member","relation":"global_org_admin","object":"b2b_org:one","condition":null}'
		else
			printf '%s\n' \
				'{"user":"team:global_org_admin#member","relation":"global_org_admin","object":"b2b_org:one","condition":null}' \
				'{"user":"team:global_org_admin#member","relation":"global_org_admin","object":"b2b_org:new-after-cutover","condition":null}'
		fi
	}
	fga_check() {
		[[ "$1" == "user:admin" ]] && printf '%s\n' true || printf '%s\n' false
	}

	migration_verify "$dir" admin nonadmin one >/dev/null
	[[ -f "$dir/verified.checkpoint" ]] || fail "equivalent tuple sets must create a checkpoint"
	assert_eq "4" "$(wc -l <"$call_log" | tr -d ' ')" \
		"verification must re-read both legacy and stable teams"
	assert_eq "1" "$(jq -r '.post_cutover_extra_stable_grant_count' "$dir/summary.json")" \
		"verification must record post-cutover stable grants"
	: >"$dir/live-grants.jsonl"
	if migration_verify "$dir" admin nonadmin one >/dev/null 2>&1; then
		fail "verify must reject plan files changed after approval"
	fi
}

test_snapshot_records_dry_run() {
	local dir
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' RETURN
	STORE_ID=store
	fga_read_all() { return 0; }
	fga_check() {
		[[ "$1" == "user:admin" ]] && printf '%s\n' true || printf '%s\n' false
	}
	migration_snapshot "$dir" old admin nonadmin one true >/dev/null
	assert_eq "true" "$(jq -r '.dry_run' "$dir/manifest.json")" \
		"snapshot dry-run must be recorded in the manifest"
}

test_validate_team_name
test_pagination
test_malformed_response_fails
test_repeated_pagination_token_fails
test_all_pagination_tokens_are_tracked
test_script_forces_c_locale
test_batches_and_dry_run
test_idempotency_options
test_batch_failure_propagates
test_apply_records_migrated_count
test_checkpoint_guard
test_cleanup_requires_checkpoint
test_plan_classifies_grants
test_plan_rejects_empty_census
test_cleanup_blocks_old_write_increase
test_cleanup_blocks_compensating_tuple_drift
test_baseline_checks_require_allowed_and_denied_controls
test_authorization_check_arguments_are_all_or_none
test_manifest_must_match_target
test_snapshot_hashes_detect_tampering
test_verify_accepts_equivalent_tuple_sets
test_snapshot_records_dry_run

echo "PASS: global org-admin migration helper"
