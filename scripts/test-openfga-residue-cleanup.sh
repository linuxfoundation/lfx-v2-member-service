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
	printf 'read\n' >>"$CALL_LOG"
	if [[ -n "${FAIL_OBJECT:-}" && "$(printf '%s' "$1" | jq -r '.object')" == "$FAIL_OBJECT" ]]; then
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
	local keys
	keys=$(jq -cS '{user, relation, object}' "$batch" | LC_ALL=C sort -u)
	if [[ "$mode" == deletes ]]; then
		jq -cS '{user, relation, object}' "$STORE_FILE" | LC_ALL=C sort -u |
			comm -23 - <(printf '%s\n' "$keys") >"$STORE_FILE.next"
	else
		{ jq -cS '{user, relation, object}' "$STORE_FILE"; printf '%s\n' "$keys"; } | LC_ALL=C sort -u >"$STORE_FILE.next"
	fi
	mv "$STORE_FILE.next" "$STORE_FILE"
}

# shellcheck disable=SC2329 # Fixture override invoked through residue_apply_paced.
sleep() { :; }

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

test_hash_mismatch_refuses_before_any_read() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	residue_load_list "$LIST" "0000" "$OUT" >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "a list that does not match the reviewed hash must be refused"
	[[ ! -s "$CALL_LOG" ]] || fail "a hash mismatch must stop before any OpenFGA call"
}

test_list_guard_rejects_stable_team_and_foreign_types() {
	local bad status
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	for bad in \
		"$(tuple "team:global_org_admin#member" global_org_admin "b2b_org:22222222-2222-2222-2222-222222222222")" \
		"$(tuple "user:alice" writer "project:33333333-3333-3333-3333-333333333333")"; do
		{ cat "$LIST"; printf '%s\n' "$bad"; } >"$WORK/bad.jsonl"
		status=0
		residue_load_list "$WORK/bad.jsonl" "$(list_hash "$WORK/bad.jsonl")" "$OUT" >/dev/null 2>&1 || status=$?
		assert_eq "4" "$status" "a list containing $bad must be refused even with a matching hash"
	done
}

test_delete_removes_only_listed_present_tuples() {
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	# One listed tuple was already removed elsewhere (e.g. reaped by fga-sync).
	local gone
	gone=$(tuple "user:alice" writer "b2b_org:22222222-2222-2222-2222-222222222222")
	jq -c --argjson g "$gone" 'select(. != $g)' "$STORE_FILE" >"$STORE_FILE.next" && mv "$STORE_FILE.next" "$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null
	assert_eq "3" "$(residue_line_count "$OUT/deleted-set.jsonl")" "the restore record must hold exactly the tuples this run deleted"
	! grep -qxF "$(printf '%s' "$gone" | jq -cS .)" "$OUT/deleted-set.jsonl" ||
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

test_failed_reader_stops_before_any_delete() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"; unset FAIL_OBJECT' RETURN
	# One of several parallel readers fails; the others succeed.
	FAIL_OBJECT="project_membership:44444444-4444-4444-4444-444444444444"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 3 0 >/dev/null 2>&1 || status=$?
	assert_eq "5" "$status" "a failed OpenFGA read must fail the run with the read error"
	! grep -q '^deletes' "$CALL_LOG" || fail "a partial read must never lead to a delete"
	[[ ! -f "$OUT/deleted-set.jsonl" ]] || fail "a partial read must not record a restore set"
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
	[[ ! -f "$OUT/deleted-set.jsonl" ]] || fail "a dry run must not record a restore set"
}

test_restore_writes_only_recorded_set_and_rejects_tampering() {
	local status=0
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	local gone
	gone=$(tuple "user:alice" writer "b2b_org:22222222-2222-2222-2222-222222222222")
	jq -c --argjson g "$gone" 'select(. != $g)' "$STORE_FILE" >"$STORE_FILE.next" && mv "$STORE_FILE.next" "$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null
	residue_restore "$OUT" false true 0 >/dev/null
	while IFS= read -r t; do
		store_has "$t" || fail "restore must bring back every deleted tuple: $t"
	done <"$OUT/deleted-set.jsonl"
	! store_has "$gone" || fail "restore must not revive a tuple this tool never deleted"
	printf '%s\n' "$gone" >>"$OUT/deleted-set.jsonl"
	residue_restore "$OUT" false true 0 >/dev/null 2>&1 || status=$?
	assert_eq "6" "$status" "restore must refuse a deleted set that no longer matches its hash"
}

test_delete_batches_at_most_one_hundred() {
	local i
	setup_fixture
	trap 'rm -rf "$WORK"' RETURN
	: >"$LIST"
	for i in $(seq 1 250); do
		tuple "project:33333333-3333-3333-3333-333333333333" project \
			"project_membership:$(printf '%08d-0000-0000-0000-000000000000' "$i")" >>"$LIST"
	done
	cp "$LIST" "$STORE_FILE"
	residue_load_list "$LIST" "$(list_hash "$LIST")" "$OUT" >/dev/null
	residue_delete "$OUT" false true 2 0 >/dev/null
	assert_eq "deletes:100,deletes:100,deletes:50" "$(grep '^deletes' "$CALL_LOG" | paste -sd, -)" \
		"deletes must go out in batches of at most 100"
	[[ ! -s "$STORE_FILE" ]] || fail "every listed tuple must be deleted"
}

test_hash_mismatch_refuses_before_any_read
test_list_guard_rejects_stable_team_and_foreign_types
test_delete_removes_only_listed_present_tuples
test_unexpected_tuple_on_listed_object_fails_closed
test_verify_catches_unlisted_subject_on_listed_object
test_failed_reader_stops_before_any_delete
test_dry_run_and_mode_write_nothing
test_restore_writes_only_recorded_set_and_rejects_tampering
test_delete_batches_at_most_one_hundred

echo "PASS: OpenFGA residue cleanup"
