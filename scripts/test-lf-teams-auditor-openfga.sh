#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Deterministic fixture suite pinning which teams the LF team auditor grant and
# revoke scripts act on. Both scripts run unmodified as subprocesses; `curl` is
# replaced on PATH by a fake OpenFGA backed by a JSONL tuple store, so the tests
# observe the effect on the store rather than call shapes. Safe for CI; no
# server needed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

STAFF=lf-staff
AUDITORS=global-org-auditors
CONTRACTOR=lf-contractor

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_eq() {
	[[ "$1" == "$2" ]] || fail "$3: want=$1 got=$2"
}

mkdir -p "$WORK/bin"
cat >"$WORK/bin/curl" <<'FAKE'
#!/usr/bin/env bash
# Fake OpenFGA: answers /read and /write from $STORE_FILE, logging each call.
set -euo pipefail
url="" body=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-d) body="$2"; shift ;;
	http*) url="$1" ;;
	esac
	shift
done
endpoint="${url##*/}"
printf '%s\n' "$endpoint" >>"$CALL_LOG"
case "$endpoint" in
read)
	jq -c --argjson b "$body" '
		select(.user == $b.tuple_key.user and .relation == $b.tuple_key.relation
			and (.object | startswith($b.tuple_key.object)))
		| {key: .}' "$STORE_FILE" | jq -sc '{tuples: .}'
	;;
write)
	keys=$(printf '%s' "$body" | jq -c '(.writes // .deletes).tuple_keys[]')
	if printf '%s' "$body" | jq -e 'has("deletes")' >/dev/null; then
		jq -c --slurpfile k <(printf '%s\n' "$keys") 'select(. as $t | $k | index($t) | not)' \
			"$STORE_FILE" >"$STORE_FILE.next"
	else
		{ cat "$STORE_FILE"; printf '%s\n' "$keys"; } | jq -c . | sort -u >"$STORE_FILE.next"
	fi
	mv "$STORE_FILE.next" "$STORE_FILE"
	echo '{}'
	;;
*) exit 22 ;;
esac
FAKE
chmod +x "$WORK/bin/curl"

export STORE_FILE="$WORK/store.jsonl" CALL_LOG="$WORK/calls.log"

tuple() {
	jq -nc --arg u "$1" --arg o "b2b_org:$2" '{user: $u, relation: "auditor", object: $o}'
}

# Every team holds auditor on o1-o3, plus one per-user grant that no team
# operation may touch.
seed_store() {
	: >"$CALL_LOG"
	{
		local team org
		for team in "$STAFF" "$AUDITORS" "$CONTRACTOR"; do
			for org in o1 o2 o3; do
				tuple "team:${team}#member" "$org"
			done
		done
		tuple "user:alice" o1
	} >"$STORE_FILE"
}

held() {
	jq -r --arg u "team:$1#member" 'select(.user == $u) | .object' "$STORE_FILE" | wc -l | tr -d ' '
}

writes() {
	grep -c '^write$' "$CALL_LOG" || true
}

# run_script <script> [VAR=value ...] -- <args>: every team variable starts
# unset, so only the ones given explicitly reach the script.
run_script() {
	local script="$1"
	shift
	local assigns=()
	while [[ "$1" != -- ]]; do
		assigns+=("$1")
		shift
	done
	shift
	env -u LF_STAFF_TEAM_NAME -u GLOBAL_ORG_AUDITORS_TEAM_NAME -u LF_CONTRACTOR_TEAM_NAME \
		PATH="$WORK/bin:$PATH" OPENFGA_URL=http://fake ${assigns[@]+"${assigns[@]}"} \
		bash "$SCRIPT_DIR/$script" "$@" </dev/null >"$WORK/out.log" 2>&1
}

# Revoke: each variable set alone removes only that team's tuples.
for case_ in "LF_STAFF_TEAM_NAME:$STAFF" "GLOBAL_ORG_AUDITORS_TEAM_NAME:$AUDITORS" "LF_CONTRACTOR_TEAM_NAME:$CONTRACTOR"; do
	var="${case_%%:*}" target="${case_#*:}"
	seed_store
	run_script revoke-lf-teams-auditor-openfga.sh "$var=$target" -- store-1 --yes ||
		fail "revoke with only $var set exited non-zero: $(cat "$WORK/out.log")"
	for team in "$STAFF" "$AUDITORS" "$CONTRACTOR"; do
		want=3
		[[ "$team" != "$target" ]] || want=0
		assert_eq "$want" "$(held "$team")" "revoke with only $var set: $team grants"
	done
	assert_eq 1 "$(jq -r 'select(.user == "user:alice") | .object' "$STORE_FILE" | wc -l | tr -d ' ')" \
		"revoke with only $var set: per-user grant"
done

# Revoke: no team configured, or only whitespace, exits 1 before any /write.
for value in "" "   "; do
	seed_store
	if run_script revoke-lf-teams-auditor-openfga.sh "GLOBAL_ORG_AUDITORS_TEAM_NAME=$value" -- store-1 --yes; then
		fail "revoke with GLOBAL_ORG_AUDITORS_TEAM_NAME='$value' exited 0"
	fi
	assert_eq 0 "$(writes)" "revoke with GLOBAL_ORG_AUDITORS_TEAM_NAME='$value': /write calls"
done

# Grant: with every team variable exported, only staff and global-org-auditors
# are written; the withdrawn contractor grant is never re-created.
mkdir -p "$WORK/in"
for org in o1 o2 o3 o4; do
	jq -nc --arg u "$org" '{uid: $u}'
done >"$WORK/in/b2b-org-uids.jsonl"
all_teams=("LF_STAFF_TEAM_NAME=$STAFF" "GLOBAL_ORG_AUDITORS_TEAM_NAME=$AUDITORS" "LF_CONTRACTOR_TEAM_NAME=$CONTRACTOR")

seed_store
run_script grant-lf-teams-auditor-openfga.sh "${all_teams[@]}" -- store-1 "$WORK/in" --dry-run ||
	fail "grant dry-run exited non-zero: $(cat "$WORK/out.log")"
assert_eq 0 "$(writes)" "grant dry-run: /write calls"

seed_store
run_script grant-lf-teams-auditor-openfga.sh "${all_teams[@]}" -- store-1 "$WORK/in" ||
	fail "grant exited non-zero: $(cat "$WORK/out.log")"
assert_eq 4 "$(held "$STAFF")" "grant: staff grants"
assert_eq 4 "$(held "$AUDITORS")" "grant: global-org-auditors grants"
assert_eq 3 "$(held "$CONTRACTOR")" "grant: contractor grants (seeded, never written)"

echo "PASS: LF team auditor grant/revoke team reach"
