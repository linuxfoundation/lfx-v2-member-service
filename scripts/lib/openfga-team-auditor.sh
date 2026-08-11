#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# openfga-team-auditor.sh — shared helpers for the LFXV2-2937 b2b_org auditor
# team grant scripts. Sourced by grant-lf-teams-auditor-openfga.sh and
# revoke-lf-teams-auditor-openfga.sh; it only defines functions, so running it
# directly does nothing.
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
#   $2 subject, e.g. team:<staff-team>#member
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
	local candidate="$1"
	local usage="$2"

	if [[ -z "$candidate" || "$candidate" == --* ]]; then
		echo "ERROR: the OpenFGA store ID is required as the first argument." >&2
		echo "Usage: $usage" >&2
		exit 1
	fi
}

# fga_team_names echoes the configured team names, one per line, reading only
# the environment variables named in "$@". Any non-empty subset of those is
# accepted; only the empty set is an error.
#
# Required, not defaulted: the defaults live in values.yaml and providers.go,
# and a third copy here would drift. Granting the wrong team name is as
# unreapable as granting the wrong store.
#
# The caller names the variables instead of this helper reading every team
# variable it knows about, because the two callers must not have the same
# reach. Revoke legitimately targets the contractor team — clearing the dev
# contractor tuples needs exactly that, and it has to work while the service
# no longer emits the grant. Grant must not: an operator who exported
# LF_CONTRACTOR_TEAM_NAME for a revoke and then ran the backfill in the same
# shell would blanket-grant contractors before LFXV2-3071 has decided whether
# they get access at all, and no service path can take a team tuple back.
fga_team_names() {
	if [[ $# -eq 0 ]]; then
		echo "ERROR: fga_team_names requires the names of the environment" >&2
		echo "       variables to read, e.g. fga_team_names LF_STAFF_TEAM_NAME." >&2
		exit 1
	fi

	local varnames=("$@")
	local names=()
	local varname name

	for varname in "${varnames[@]}"; do
		name="${!varname:-}"
		# Trim surrounding whitespace so a padded value cannot render as
		# "team: #member", mirroring the trim in providers.go.
		name="${name#"${name%%[![:space:]]*}"}"
		name="${name%"${name##*[![:space:]]}"}"
		[[ -n "$name" ]] && names+=("$name")
	done

	if [[ ${#names[@]} -eq 0 ]]; then
		echo "ERROR: no auditor team configured. Set at least one of" >&2
		echo "       ${varnames[*]}." >&2
		echo "       They are deliberately not defaulted here — the authoritative value" >&2
		echo "       is in charts/lfx-v2-member-service/values.yaml. Export it to match" >&2
		echo "       the environment you are targeting, e.g.:" >&2
		echo "         export ${varnames[0]}=<team-name>" >&2
		exit 1
	fi

	printf '%s\n' "${names[@]}"
}
