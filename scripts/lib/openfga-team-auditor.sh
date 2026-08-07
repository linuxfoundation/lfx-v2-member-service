#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# openfga-team-auditor.sh — shared helpers for the LFXV2-2937 b2b_org auditor
# team grant scripts. Sourced by grant-lf-teams-auditor-openfga.sh and
# revoke-lf-teams-auditor-openfga.sh; not executable on its own.
#
# The two scripts were near-identical copies. They are shared here because the
# revoke script is the rollback path that runs under incident pressure, and a
# rollback that has silently drifted from the tested grant path is worse than no
# rollback at all.
#
# Callers must set, before sourcing or before the first call:
#   BASE_URL   OpenFGA base URL
#   STORE_ID   OpenFGA store ID
#   BATCH_SIZE tuples per /write request

# fga_curl POSTs a JSON body to an OpenFGA endpoint and fails loudly.
#
# -sf --show-error makes curl exit non-zero on an HTTP error rather than
# printing the error body to stdout and returning 0, which would otherwise be
# parsed as a successful empty result. The API-level error check is separate and
# still needed: OpenFGA returns 200 with a {"code":...} body for some failures.
fga_curl() {
	local endpoint="$1"
	local body="$2"
	local resp

	if ! resp=$(curl -sf --show-error -X POST "${BASE_URL}/stores/${STORE_ID}/${endpoint}" \
		-H 'Content-Type: application/json' \
		-d "$body" 2>&1); then
		echo "ERROR: curl to /${endpoint} failed: $resp" >&2
		return 1
	fi

	if echo "$resp" | jq -e '.code' >/dev/null 2>&1; then
		echo "ERROR from /${endpoint}: $(echo "$resp" | jq -r '.message')" >&2
		return 1
	fi

	echo "$resp"
}

# fga_read_org_uids pages /read to exhaustion for one subject's auditor tuples
# on b2b_org, emitting bare org UIDs one per line.
fga_read_org_uids() {
	local team_subject="$1"
	local token=""

	while true; do
		local body
		body=$(jq -n --arg u "$team_subject" --arg ct "$token" \
			'{"tuple_key": {"user": $u, "relation": "auditor", "object": "b2b_org:"}, "page_size": 100}
			 + (if $ct == "" then {} else {"continuation_token": $ct} end)')

		local resp
		resp=$(fga_curl "read" "$body") || exit 1

		# Positive assertion, not defensive tidying. curl -sf rejects 4xx/5xx and
		# fga_curl rejects a {"code":…} body, but a 200 carrying an empty or
		# non-OpenFGA payload — a stale port-forward, or a proxy answering in
		# OpenFGA's place — passes both and then reads as zero tuples. That makes
		# revoke print "Nothing to do." for a rollback that removed nothing. An
		# empty store legitimately answers {"tuples":[]}, so require the key.
		if ! echo "$resp" | jq -e 'has("tuples")' >/dev/null 2>&1; then
			echo "ERROR: /read returned a body without a 'tuples' field — is BASE_URL pointing at OpenFGA?" >&2
			exit 1
		fi

		echo "$resp" | jq -r '.tuples[]?.key.object | sub("^b2b_org:"; "")'

		token=$(echo "$resp" | jq -r '.continuation_token // ""')
		[[ -z "$token" ]] && break
	done
}

# fga_apply_batch writes or deletes one batch of auditor tuples for a subject.
#
#   $1 mode: "writes" or "deletes"
#   $2 subject, e.g. team:lf-staff#member
#   $3 JSON array of bare org UIDs
#
# on_duplicate/on_missing "ignore" is load-bearing, not defensive. /write is
# transactional per request: by default a duplicate insert, or a delete of an
# already-absent tuple, fails the entire batch. Both are reachable here — the
# grant backfill runs after rollout so CDC upserts assert the same tuples
# concurrently, and the revoke reads a paginated list that can go stale mid-run.
# fga-sync sets both options on every batch it writes
# (writeCollisionIgnoreOptions, fga.go:494-499). Available from OpenFGA v1.10.0;
# dev and prod both run v1.14.0.
#
# Only the field matching the mode is set. A request mixing ignore and error
# semantics reverts to error for the whole request, so a future caller sending
# writes and deletes together must set both.
fga_apply_batch() {
	local mode="$1"
	local team_subject="$2"
	local uids_json="$3"

	local ignore_key
	case "$mode" in
	writes) ignore_key="on_duplicate" ;;
	deletes) ignore_key="on_missing" ;;
	*)
		echo "ERROR: fga_apply_batch mode must be writes or deletes, got '$mode'" >&2
		exit 1
		;;
	esac

	local count
	count=$(echo "$uids_json" | jq 'length')

	local payload
	payload=$(jq -n --arg u "$team_subject" --arg mode "$mode" --arg ik "$ignore_key" --argjson uids "$uids_json" \
		'{($mode): {"tuple_keys": [$uids[] | {user: $u, relation: "auditor", object: ("b2b_org:" + .)}], ($ik): "ignore"}}')

	if ! fga_curl "write" "$payload" >/dev/null; then
		echo "Failing batch:" >&2
		echo "$uids_json" | jq . >&2
		exit 1
	fi

	echo "  Applied ($mode) $count tuples"
}

# fga_apply_uid_file batches every UID in a file through fga_apply_batch,
# reporting progress against the expected total.
fga_apply_uid_file() {
	local mode="$1"
	local team_subject="$2"
	local uid_file="$3"
	local total="$4"

	local applied=0
	local batch=()

	flush() {
		[[ ${#batch[@]} -eq 0 ]] && return 0
		local batch_json
		batch_json=$(printf '%s\n' "${batch[@]}" | jq -R -s -c 'split("\n") | map(select(length > 0))')
		fga_apply_batch "$mode" "$team_subject" "$batch_json"
		applied=$((applied + ${#batch[@]}))
		echo "  Progress: $applied / $total"
		batch=()
	}

	while IFS= read -r uid; do
		[[ -z "$uid" ]] && continue
		batch+=("$uid")
		if [[ ${#batch[@]} -ge $BATCH_SIZE ]]; then
			flush
		fi
	done <"$uid_file"
	flush
}

# fga_require_store_id validates the first positional argument.
#
# The store ID is required with no default: a default target on a script whose
# writes cannot be undone is a foot-gun, not a convenience.
fga_require_store_id() {
	local store_id="$1"
	local usage="$2"

	if [[ -z "$store_id" || "$store_id" == --* ]]; then
		echo "ERROR: the OpenFGA store ID is required as the first argument." >&2
		echo "Usage: $usage" >&2
		exit 1
	fi
}

# fga_team_names echoes the configured team names, one per line.
#
# Required, not defaulted: the defaults live in values.yaml and providers.go,
# and a third copy here would drift. Granting the wrong team name is as
# unreapable as granting the wrong store.
fga_team_names() {
	if [[ -z "${LF_STAFF_TEAM_NAME:-}" || -z "${LF_CONTRACTOR_TEAM_NAME:-}" ]]; then
		echo "ERROR: LF_STAFF_TEAM_NAME and LF_CONTRACTOR_TEAM_NAME must both be set." >&2
		echo "       They are deliberately not defaulted here — the authoritative values" >&2
		echo "       are in charts/lfx-v2-member-service/values.yaml. Export them to match" >&2
		echo "       the environment you are targeting, e.g.:" >&2
		echo "         export LF_STAFF_TEAM_NAME=lf-staff" >&2
		echo "         export LF_CONTRACTOR_TEAM_NAME=lf-contractor" >&2
		exit 1
	fi
	echo "$LF_STAFF_TEAM_NAME"
	echo "$LF_CONTRACTOR_TEAM_NAME"
}
