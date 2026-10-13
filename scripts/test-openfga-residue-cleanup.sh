#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Deterministic fixture suite for the OpenFGA residue cleanup safety gates.
# OpenFGA is replaced by a JSONL tuple store in a temp file: reads filter it like
# /read does, and batches really add or remove tuples, so the tests observe the
# effect on the store rather than call shapes. Safe for CI; no server needed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/cleanup-openfga-residue.sh
source "$SCRIPT_DIR/cleanup-openfga-residue.sh"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_eq() {
	local want="$1"
	local got="$2"
	local message="$3"
	[[ "$want" == "$got" ]] || fail "$message: want=$want got=$got"
}

tuple() {
	jq -nc --arg u "$1" --arg r "$2" --arg o "$3" '{user:$u,relation:$r,object:$o}'
}

# The fake store: STORE_FILE holds the tuples OpenFGA would return.
# shellcheck disable=SC2329 # Fixture override invoked through residue_verify.
fga_read_all() {
	local object
	object=$(printf '%s' "$1" | jq -r '.object')
	printf 'read %s\n' "$object" >>"$CALL_LOG"
	if [[ -n "${FAIL_OBJECT:-}" && "$object" == "$FAIL_OBJECT" ]]; then
		return 5
	fi
	# Fails the first read of this object after printing a partial page, as a
	# connection dropped mid-pagination would.
	if [[ -n "${FLAKY_OBJECT:-}" && "$object" == "$FLAKY_OBJECT" && ! -e "$WORK/flaked" ]]; then
		: >"$WORK/flaked"
		jq -c --arg o "$object" 'select(.object == $o)' "$STORE_FILE" | head -n 1
		return 5
	fi
	jq -c --argjson f "$1" '
		select(
			(if ($f.user // "") != "" then .user == $f.user else true end) and
			(if ($f.object | endswith(":")) then (.object | startswith($f.object)) else .object == $f.object end))
	' "$STORE_FILE"
}

# shellcheck disable=SC2329 # Fixture override invoked through residue_apply_paced.
fga_apply_batch() {
	local mode="$1"
	local batch="$2"
	printf '%s:%s\n' "$mode" "$(awk 'NF { n++ } END { print n + 0 }' "$batch")" >>"$CALL_LOG"
	if [[ "$mode" == deletes ]]; then
		DELETE_BATCHES=$((DELETE_BATCHES + 1))
		# Fail one delete batch once, as a transient OpenFGA error would.
		if [[ "$DELETE_BATCHES" -eq "${FAIL_DELETE_BATCH:-0}" ]]; then
			FAIL_DELETE_BATCH=0
			return 5
		fi
		# A store that acknowledges deletes without applying them.
		[[ -z "${DROP_DELETES:-}" ]] || return 0
	fi
	# A store that acknowledges writes without applying them.
	[[ "$mode" != writes || -z "${DROP_WRITES:-}" ]] || return 0
	local keys
	keys=$(jq -cS '{user, relation, object}' "$batch" | LC_ALL=C sort -u)
	if [[ "$mode" == deletes ]]; then
		jq -cS '{user, relation, object}' "$STORE_FILE" | LC_ALL=C sort -u |
			comm -23 - <(printf '%s\n' "$keys") >"$STORE_FILE.next"
	else
		{
			jq -cS '{user, relation, object}' "$STORE_FILE"
			printf '%s\n' "$keys"
		} | LC_ALL=C sort -u >"$STORE_FILE.next"
	fi
	mv "$STORE_FILE.next" "$STORE_FILE"
}

# shellcheck disable=SC2329 # Fixture override invoked through residue_apply_paced.
sleep() { :; }

# Fails the rename that commits a new deleted-set seal, as an interrupt between
# writing a new generation and committing it would.
# shellcheck disable=SC2329 # Fixture override invoked through residue_commit_deleted_set.
mv() {
	if [[ -n "${FAIL_SEAL_COMMIT:-}" && "${!#}" == */deleted-set.sha256 ]]; then
		return 1
	fi
	command mv "$@"
}

# Breaks sharding of the object list: "error" fails it outright; "partial"
# succeeds but loses one shard, as a silently truncated write would.
# shellcheck disable=SC2329 # Fixture override invoked through residue_read_objects.
awk() {
	if [[ -z "${BREAK_SHARDING:-}" || "$*" != *'/shard.'* ]]; then
		command awk "$@"
		return
	fi
	[[ "$BREAK_SHARDING" != error ]] || return 1
	local arg dir=""
	for arg in "$@"; do
		[[ "$arg" != d=* ]] || dir="${arg#d=}"
	done
	command awk "$@" || return
	rm -f "$dir/shard.1"
}

# Fails the comparison of the list with OpenFGA, as a full disk would.
# shellcheck disable=SC2329 # Fixture override invoked through residue_verify.
comm() {
	[[ -z "${FAIL_COMPARE:-}" ]] || return 1
	command comm "$@"
}

# Fails hashing the deleted-set record before it is committed.
eval "real_$(declare -f fga_hash_file)"
fga_hash_file() {
	if [[ -n "${FAIL_RECORD_HASH:-}" && "$1" == */deleted-set.next.* ]]; then
		return 1
	fi
	real_fga_hash_file "$@"
}

# Fails creating the read path's private temporary directory.
# shellcheck disable=SC2329 # Fixture override invoked through residue_read_current.
mktemp() {
	if [[ -n "${FAIL_TEMP_DIR:-}" && "$*" == "-d" ]]; then
		return 1
	fi
	command mktemp "$@"
}

list_hash() {
	local file="$1"
	local canon
	canon=$(mktemp)
	residue_canonicalize "$file" "$canon"
	fga_hash_file "$canon"
	rm -f "$canon"
}

setup_fixture() {
	WORK=$(mktemp -d)
	STORE_FILE="$WORK/store.jsonl"
	CALL_LOG="$WORK/calls.log"
	OUT="$WORK/out"
	LIST="$WORK/list.jsonl"
	STORE_ID="store-a"
	DELETE_BATCHES=0
	FAIL_DELETE_BATCH=0
	DROP_DELETES=""
	DROP_WRITES=""
	mkdir -p "$OUT"
	: >"$CALL_LOG"
	# Listed residue: a UUID org's edges and grants, and a UUID membership.
	{
		tuple "b2b_org:11111111-1111-1111-1111-111111111111" parent "b2b_org:22222222-2222-2222-2222-222222222222"
		tuple "user:alice" writer "b2b_org:22222222-2222-2222-2222-222222222222"
		tuple "project:33333333-3333-3333-3333-333333333333" project "project_membership:44444444-4444-4444-4444-444444444444"
		tuple "b2b_org:22222222-2222-2222-2222-222222222222" b2b_org "project_membership:44444444-4444-4444-4444-444444444444"
	} >"$LIST"
	# Store: the listed residue plus live tuples of the same subjects that must survive.
	{
		cat "$LIST"
		tuple "user:alice" writer "b2b_org:001LIVEORG0000000A"
		tuple "project:33333333-3333-3333-3333-333333333333" project "project_membership:02iLIVEASSET00000A"
		tuple "team:global_org_admin#member" global_org_admin "b2b_org:001LIVEORG0000000A"
	} >"$STORE_FILE"
}

store_has() {
	jq -cS '{user, relation, object}' "$STORE_FILE" | grep -qxF "$(printf '%s' "$1" | jq -cS .)"
}

# The deleted-set generation the current seal vouches for.
committed_set() {
	residue_deleted_set_path "$OUT"
}

# Removes one listed tuple from the store before the run (e.g. reaped by
# fga-sync), then runs a confirmed delete. Prints the removed tuple.
delete_with_one_listed_tuple_gone() {
	local gone
	gone=$(tuple "user:alice" writer "b2b_org:22222222-2222-2222-2222-222222222222")
	jq -c --argjson g "$gone" 'select(. != $g)' "$STORE_FILE" >"$STORE_FILE.next"
	mv "$STORE_FILE.next" "$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null
	printf '%s\n' "$gone"
}

test_hash_mismatch_refuses_before_any_read() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	residue_main delete --store-id "$STORE_ID" --list "$LIST" --expect-sha256 0000 \
		--output-dir "$OUT" --confirm >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "a list that does not match the reviewed hash must be refused"
	[[ ! -s "$CALL_LOG" ]] || fail "a hash mismatch must stop before any OpenFGA call"
}

test_reusing_canonical_list_as_input_keeps_it() {
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_load_list "$OUT/list.jsonl" "$(list_hash "$LIST")" "$OUT" >/dev/null
	assert_eq "4" "$(residue_line_count "$OUT/list.jsonl")" "loading the output directory's own list must not erase it"
}

test_missing_flag_value_is_rejected() {
	local status=0
	residue_main verify --list >/dev/null 2>&1 || status=$?
	assert_eq "2" "$status" "a value flag without a value must be rejected, not loop"
}

test_one_run_per_output_directory() {
	local status=0 pid i
	local -a run=(verify --store-id store-a --list "" --expect-sha256 "" --output-dir "")
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	run[4]="$LIST"
	run[6]=$(list_hash "$LIST")
	run[8]="$OUT"
	mkdir "$OUT/.lock"
	residue_main "${run[@]}" >/dev/null 2>&1 || status=$?
	assert_eq "7" "$status" "a run must refuse an output directory another run holds"
	[[ ! -s "$CALL_LOG" && ! -e "$OUT/list.jsonl" ]] || fail "a refused run must not read OpenFGA or touch the directory"
	rmdir "$OUT/.lock"
	residue_main "${run[@]}" >/dev/null
	[[ ! -e "$OUT/.lock" ]] || fail "a finished run must release the lock"
	# An interrupted run releases it too.
	(
		# shellcheck disable=SC2329 # Fixture override invoked through residue_read_objects.
		fga_read_all() {
			: >"$WORK/reading"
			command sleep 1
		}
		residue_main "${run[@]}" >/dev/null 2>&1
	) &
	pid=$!
	for i in $(seq 1 100); do
		[[ -e "$WORK/reading" ]] && break
		command sleep 0.1
	done
	[[ -e "$OUT/.lock" ]] || fail "the run must hold the lock while reading (iterations: $i)"
	kill -TERM "$pid"
	{ wait "$pid" || true; } 2>/dev/null
	[[ ! -e "$OUT/.lock" ]] || fail "an interrupted run must release the lock"
}

test_list_guard_rejects_unsafe_or_malformed_tuples() {
	local bad status
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	for bad in \
		"$(tuple "team:global_org_admin#member" global_org_admin "b2b_org:22222222-2222-2222-2222-222222222222")" \
		"$(tuple "user:alice" writer "project:33333333-3333-3333-3333-333333333333")" \
		'{"user":"team:global_org_admin#member","relation":"global_org_admin"}' \
		'{"relation":"writer","object":"b2b_org:22222222-2222-2222-2222-222222222222"}' \
		"$(tuple "user:alice" writer "b2b_org")" \
		"$(tuple "b2b_org:22222222-2222-2222-2222-222222222222" child "b2b_org:001LIVEORG0000000A")" \
		'{"user":"user:alice","relation":"writer","object":"b2b_org:22222222-2222-2222-2222-222222222222","condition":{"name":"c"}}'; do
		{
			cat "$LIST"
			printf '%s\n' "$bad"
		} >"$WORK/bad.jsonl"
		status=0
		residue_load_list "$WORK/bad.jsonl" "$(list_hash "$WORK/bad.jsonl")" "$OUT" >/dev/null 2>&1 || status=$?
		assert_eq "4" "$status" "a list containing $bad must be refused even with a matching hash"
	done
}

test_delete_removes_only_listed_present_tuples() {
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	local gone
	gone=$(delete_with_one_listed_tuple_gone)
	assert_eq "3" "$(residue_line_count "$(committed_set)")" "the restore record must hold exactly the tuples this run deleted"
	! grep -qxF "$(printf '%s' "$gone" | jq -cS .)" "$(committed_set)" ||
		fail "a tuple removed before the run must not enter the restore record"
	while IFS= read -r t; do
		! store_has "$t" || fail "listed tuple survived delete: $t"
	done <"$LIST"
	store_has "$(tuple "user:alice" writer "b2b_org:001LIVEORG0000000A")" ||
		fail "a listed subject's tuple on an unlisted live org must survive"
	store_has "$(tuple "project:33333333-3333-3333-3333-333333333333" project "project_membership:02iLIVEASSET00000A")" ||
		fail "a listed project's tuple on an unlisted live membership must survive"
	store_has "$(tuple "team:global_org_admin#member" global_org_admin "b2b_org:001LIVEORG0000000A")" ||
		fail "the stable team grant must survive"
}

test_unexpected_tuple_on_listed_object_fails_closed() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	# A listed subject gained a different relation on a listed object.
	tuple "user:alice" auditor "b2b_org:22222222-2222-2222-2222-222222222222" >>"$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "4" "$status" "a tuple on a listed object that the list lacks must fail closed"
	! grep -q '^deletes' "$CALL_LOG" || fail "an unexpected tuple must stop the run before any delete"
}

test_conditional_tuple_on_listed_object_fails_closed() {
	local status=0 listed
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	# OpenFGA reports unconditional tuples with a null condition: those still match.
	listed=$(tuple "user:alice" writer "b2b_org:22222222-2222-2222-2222-222222222222")
	jq -c --argjson l "$listed" 'if . == $l then . + {condition: null} else . end' "$STORE_FILE" >"$STORE_FILE.next"
	mv "$STORE_FILE.next" "$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_verify "$OUT" 2 >/dev/null
	assert_eq "4" "$(residue_line_count "$OUT/present.jsonl")" "a null condition must compare equal to the listed tuple"
	# The same grant carrying a real condition must never be deleted as if unconditional.
	jq -c --argjson l "$listed" 'if {user, relation, object} == $l then . + {condition: {name: "c"}} else . end' \
		"$STORE_FILE" >"$STORE_FILE.next"
	mv "$STORE_FILE.next" "$STORE_FILE"
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "4" "$status" "a conditional tuple on a listed object must fail closed"
	! grep -q '^deletes' "$CALL_LOG" || fail "a conditional tuple must stop the run before any delete"
}

test_verify_catches_unlisted_subject_on_listed_object() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	# A subject that is not in the list at all gained a grant on a listed object;
	# only reading the object itself can see it.
	tuple "user:mallory" writer "b2b_org:22222222-2222-2222-2222-222222222222" >>"$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_verify "$OUT" 2 >/dev/null 2>&1 || status=$?
	assert_eq "4" "$status" "verify must catch an unlisted subject on a listed object"
}

test_transient_read_failure_is_retried() {
	setup_fixture
	trap 'rm -rf "$WORK"; unset FLAKY_OBJECT' RETURN
	FLAKY_OBJECT="b2b_org:22222222-2222-2222-2222-222222222222"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_verify "$OUT" 2 >/dev/null
	[[ -e "$WORK/flaked" ]] || fail "the fixture must have failed one read"
	assert_eq "4" "$(residue_line_count "$OUT/present.jsonl")" "a read that fails once must be retried, not fail the pass"
	assert_eq "0" "$(residue_line_count "$OUT/unexpected.jsonl")" "a retried read must not duplicate or invent tuples"
}

test_broken_object_sharding_fails_before_any_delete() {
	local mode status
	for mode in error partial; do
		setup_fixture
		residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
		BREAK_SHARDING="$mode"
		status=0
		residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
		BREAK_SHARDING=""
		assert_eq "3" "$status" "a $mode while sharding the object list must fail the read"
		! grep -q '^deletes' "$CALL_LOG" || fail "a $mode while sharding must never lead to a delete"
		[[ ! -f "$OUT/deleted-set.sha256" ]] || fail "a $mode while sharding must not record a restore set"
		rm -rf "$WORK"
	done
}

test_failed_comparison_fails_before_any_delete() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"; unset FAIL_COMPARE' RETURN
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	FAIL_COMPARE=1
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "3" "$status" "a failed comparison with OpenFGA must fail the run"
	! grep -q '^deletes' "$CALL_LOG" || fail "a failed comparison must never lead to a delete"
}

test_failed_record_hash_stops_before_any_delete() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"; unset FAIL_RECORD_HASH' RETURN
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	FAIL_RECORD_HASH=1
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "3" "$status" "a failed hash of the deleted-set record must fail the run"
	! grep -q '^deletes' "$CALL_LOG" || fail "nothing may be deleted without a committed record"
	[[ ! -f "$OUT/deleted-set.sha256" ]] || fail "a failed hash must not commit a seal"
	assert_eq "0" "$(find "$OUT" -name 'deleted-set.*' | wc -l | tr -d ' ')" "a failed hash must leave no record files"
}

test_failed_temp_dir_fails_the_read() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"; unset FAIL_TEMP_DIR' RETURN
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	FAIL_TEMP_DIR=1
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "3" "$status" "a failed temporary directory must fail the read"
	assert_eq "0" "$(grep -c '^read' "$CALL_LOG")" "no OpenFGA read may run without a private temporary directory"
	[[ ! -e "$OUT/present.jsonl" ]] || fail "a failed read must not write verification results"
}

test_failed_reader_stops_before_any_delete() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"; unset FAIL_OBJECT' RETURN
	# One of several parallel readers fails; the others succeed.
	FAIL_OBJECT="project_membership:44444444-4444-4444-4444-444444444444"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 3 0 >/dev/null 2>&1 || status=$?
	assert_eq "5" "$status" "a failed OpenFGA read must fail the run with the read error"
	assert_eq "3" "$(grep -c "^read $FAIL_OBJECT\$" "$CALL_LOG")" "a failing read must be retried exactly up to the attempt limit"
	! grep -q '^deletes' "$CALL_LOG" || fail "a partial read must never lead to a delete"
	[[ ! -f "$OUT/deleted-set.sha256" ]] || fail "a partial read must not record a restore set"
}

test_dry_run_and_mode_write_nothing() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" true false 2 0 >/dev/null
	residue_delete "$OUT" false false 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "2" "$status" "delete must require exactly one of --dry-run or --confirm"
	! grep -q '^deletes' "$CALL_LOG" || fail "dry-run and a missing mode must not delete"
	[[ ! -f "$OUT/deleted-set.sha256" ]] || fail "a dry run must not record a restore set"
}

test_restore_writes_only_recorded_set_and_rejects_tampering() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	local gone
	gone=$(delete_with_one_listed_tuple_gone)
	residue_restore "$OUT" false true 2 0 >/dev/null
	while IFS= read -r t; do
		store_has "$t" || fail "restore must bring back every deleted tuple: $t"
	done <"$(committed_set)"
	! store_has "$gone" || fail "restore must not revive a tuple this tool never deleted"
	printf '%s\n' "$gone" >>"$(committed_set)"
	residue_restore "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "restore must refuse a deleted set that no longer matches its hash"
}

write_large_fixture() {
	local i
	: >"$LIST"
	for i in $(seq 1 250); do
		tuple "project:33333333-3333-3333-3333-333333333333" project \
			"project_membership:$(printf '%08d-0000-0000-0000-000000000000' "$i")" >>"$LIST"
	done
	cp "$LIST" "$STORE_FILE"
}

test_delete_batches_at_most_one_hundred() {
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	write_large_fixture
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null
	assert_eq "deletes:100,deletes:100,deletes:50" "$(grep '^deletes' "$CALL_LOG" | paste -sd, -)" \
		"deletes must go out in batches of at most 100"
	[[ ! -s "$STORE_FILE" ]] || fail "every listed tuple must be deleted"
}

test_retry_after_failed_batch_restores_every_run() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	write_large_fixture
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	FAIL_DELETE_BATCH=2
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "5" "$status" "a failed delete batch must fail the run"
	assert_eq "150" "$(residue_line_count "$STORE_FILE")" "the first run must stop at the failed batch"
	residue_delete "$OUT" false true 2 0 >/dev/null
	[[ ! -s "$STORE_FILE" ]] || fail "the retry must delete what the first run left"
	assert_eq "250" "$(residue_line_count "$(committed_set)")" \
		"the restore record must cover the first run's deletions and the retry's"
	residue_restore "$OUT" false true 2 0 >/dev/null
	assert_eq "250" "$(residue_line_count "$STORE_FILE")" "restore must bring back every run's deletions"
}

test_deletes_that_do_not_take_effect_fail() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	DROP_DELETES=1
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "4" "$status" "listed tuples still present after delete must fail the run"
}

test_restores_that_do_not_take_effect_fail() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	delete_with_one_listed_tuple_gone >/dev/null
	DROP_WRITES=1
	residue_restore "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "4" "$status" "recorded tuples still missing after restore must fail the rollback"
	assert_eq "3" "$(residue_line_count "$OUT/missing.jsonl")" "the missing tuples must be reported"
}

test_real_read_pagination_returns_every_page() {
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	(
		# Restore the library's real paginating fga_read_all and fake only the
		# HTTP layer: one tuple per page, so every listed object needs a
		# continuation token to be read in full.
		# shellcheck source=scripts/lib/openfga-global-org-admin-migration.sh
		source "$SCRIPT_DIR/lib/openfga-global-org-admin-migration.sh"
		# shellcheck disable=SC2329 # Fixture override invoked through fga_read_all.
		fga_request() {
			printf '%s' "$2" | jq -c . >>"$WORK/requests.jsonl"
			jq -cn --argjson b "$2" --slurpfile s "$STORE_FILE" '
				[$s[] | select(.object == $b.tuple_key.object)] as $m
				| (($b.continuation_token // "0") | tonumber) as $i
				| {tuples: [$m[$i:$i + 1][] | {key: .}],
				   continuation_token: (if $i + 1 < ($m | length) then ($i + 1 | tostring) else "" end)}'
		}
		residue_verify "$OUT" 2 >/dev/null
	)
	assert_eq "4" "$(residue_line_count "$OUT/present.jsonl")" "every page of every listed object must be read"
	assert_eq "0" "$(residue_line_count "$OUT/unexpected.jsonl")" "pagination must not duplicate or invent tuples"
	assert_eq "2" "$(jq -r 'select(.continuation_token) | .tuple_key.object' "$WORK/requests.jsonl" | sort -u | wc -l | tr -d ' ')" \
		"both listed objects must have been read past their first page"
}

test_interrupted_delete_leaves_one_batch_file_and_resumes() {
	local pid i
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	write_large_fixture
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	mkdir "$WORK/tmp"
	(
		export TMPDIR="$WORK/tmp"
		# A slow batch, so the signal lands while it is being applied.
		# shellcheck disable=SC2329 # Fixture override invoked through residue_apply_paced.
		fga_apply_batch() {
			: >"$WORK/applying"
			command sleep 1
		}
		residue_delete "$OUT" false true 2 0 >/dev/null 2>&1
	) &
	pid=$!
	for i in $(seq 1 100); do
		[[ -e "$WORK/applying" ]] && break
		command sleep 0.1
	done
	[[ -e "$WORK/applying" ]] || fail "the fixture delete never reached a batch (iterations: $i)"
	kill -TERM "$pid"
	# The shell reports the deliberately killed job on stderr; that is expected.
	{ wait "$pid" || true; } 2>/dev/null
	assert_eq "1" "$(find "$OUT" -name 'batch.*' | wc -l | tr -d ' ')" \
		"an interrupted apply leaves exactly its current batch file, in the output directory"
	assert_eq "0" "$(find "$WORK/tmp" -mindepth 1 | wc -l | tr -d ' ')" "an interrupted apply leaves nothing in TMPDIR"
	residue_delete "$OUT" false true 2 0 >/dev/null
	[[ ! -s "$STORE_FILE" ]] || fail "the next run must resume and finish the delete"
}

test_deleted_set_is_bound_to_its_store() {
	local status
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	write_large_fixture
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	FAIL_DELETE_BATCH=2
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || true
	# The same output directory pointed at another store.
	STORE_ID="store-b"
	status=0
	residue_restore "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "restore must refuse a deleted set recorded for another store"
	status=0
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "a retry must not merge into a deleted set recorded for another store"
	assert_eq "150" "$(residue_line_count "$STORE_FILE")" "a refused retry must not delete anything"
	# Back on the right store, a tampered record is refused the same way.
	STORE_ID="store-a"
	tuple "user:alice" writer "b2b_org:001LIVEORG0000000A" >>"$(committed_set)"
	status=0
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "a retry must refuse a tampered deleted set"
}

test_interrupted_checkpoint_commit_keeps_previous_record() {
	local status=0 gone deletes_before
	setup_fixture
	trap 'rm -rf "$WORK"; unset FAIL_SEAL_COMMIT' RETURN
	gone=$(delete_with_one_listed_tuple_gone)
	# The previously gone tuple comes back, so the retry's record differs.
	printf '%s\n' "$gone" >>"$STORE_FILE"
	deletes_before=$(grep -c '^deletes' "$CALL_LOG")
	FAIL_SEAL_COMMIT=1
	residue_delete "$OUT" false true 2 0 >/dev/null 2>&1 || status=$?
	unset FAIL_SEAL_COMMIT
	[[ "$status" -ne 0 ]] || fail "a failed checkpoint commit must fail the run"
	assert_eq "$deletes_before" "$(grep -c '^deletes' "$CALL_LOG")" "nothing may be deleted before the record is committed"
	# The previous generation and its seal still validate: restore brings back
	# exactly what the first run deleted.
	residue_restore "$OUT" false true 2 0 >/dev/null
	assert_eq "writes:3" "$(grep '^writes' "$CALL_LOG" | paste -sd, -)" "restore must use the last committed record"
	# A clean retry commits a merged record that covers both runs.
	residue_delete "$OUT" false true 2 0 >/dev/null
	assert_eq "4" "$(residue_line_count "$(committed_set)")" "the retry must commit a record covering every run"
	assert_eq "1" "$(find "$OUT" -name 'deleted-set.*.jsonl' | wc -l | tr -d ' ')" "only the committed generation is kept"
}

test_hash_mismatch_refuses_before_any_read
test_reusing_canonical_list_as_input_keeps_it
test_missing_flag_value_is_rejected
test_one_run_per_output_directory
test_list_guard_rejects_unsafe_or_malformed_tuples
test_delete_removes_only_listed_present_tuples
test_unexpected_tuple_on_listed_object_fails_closed
test_conditional_tuple_on_listed_object_fails_closed
test_transient_read_failure_is_retried
test_verify_catches_unlisted_subject_on_listed_object
test_failed_reader_stops_before_any_delete
test_broken_object_sharding_fails_before_any_delete
test_failed_comparison_fails_before_any_delete
test_failed_record_hash_stops_before_any_delete
test_failed_temp_dir_fails_the_read
test_dry_run_and_mode_write_nothing
test_restore_writes_only_recorded_set_and_rejects_tampering
test_delete_batches_at_most_one_hundred
test_retry_after_failed_batch_restores_every_run
test_deletes_that_do_not_take_effect_fail
test_restores_that_do_not_take_effect_fail
test_real_read_pagination_returns_every_page
test_interrupted_delete_leaves_one_batch_file_and_resumes
test_deleted_set_is_bound_to_its_store
test_interrupted_checkpoint_commit_keeps_previous_record

echo "PASS: OpenFGA residue cleanup"
