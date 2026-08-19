#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Shared OpenFGA HTTP, pagination, batching, hashing, checkpoint, and
# authorization-check primitives for the global org-admin migration.
# Source this library from the migration command or its fixture suite; it does
# not select an environment or perform a migration by itself.

FGA_BATCH_SIZE=100
FGA_STABLE_TEAM="global_org_admin"

fga_error() {
	echo "ERROR: $*" >&2
}

fga_validate_team_name() {
	local name="$1"
	if [[ -z "$name" || ! "$name" =~ ^[A-Za-z0-9._-]+$ ||
		"$name" == team:* || "$name" == *"#member" ]]; then
		fga_error "team value must be an unqualified, non-empty identifier"
		return 1
	fi
	if [[ "$name" == "$FGA_STABLE_TEAM" ]]; then
		fga_error "legacy team must differ from $FGA_STABLE_TEAM"
		return 1
	fi
}

fga_request() {
	local endpoint="$1"
	local body="$2"
	local response
	if ! response=$(curl -sf --show-error -X POST \
		--connect-timeout 10 --max-time 120 \
		"${BASE_URL}/stores/${STORE_ID}/${endpoint}" \
		-H 'Content-Type: application/json' \
		-d "$body" 2>&1); then
		fga_error "/$endpoint request failed: $response"
		return 5
	fi
	if printf '%s' "$response" | jq -e '.code' >/dev/null 2>&1; then
		fga_error "/$endpoint returned $(printf '%s' "$response" | jq -r '.message // .code')"
		return 5
	fi
	printf '%s\n' "$response"
}

fga_remember_token() {
	local seen_file="$1"
	local token="$2"
	if grep -qxF "$token" "$seen_file"; then
		fga_error "/read repeated continuation token"
		return 3
	fi
	printf '%s\n' "$token" >>"$seen_file"
}

fga_read_all() {
	local filter_json="$1"
	local token=""
	local seen_tokens
	seen_tokens=$(mktemp)

	while true; do
		local body response
		body=$(jq -n --argjson filter "$filter_json" --arg token "$token" \
			'{"tuple_key":$filter,"page_size":100}
			 + (if $token == "" then {} else {"continuation_token":$token} end)')
		response=$(fga_request read "$body") || {
			local status=$?
			rm -f "$seen_tokens"
			return "$status"
		}
		if ! printf '%s' "$response" | jq -e '.tuples | type == "array"' >/dev/null 2>&1; then
			rm -f "$seen_tokens"
			fga_error "/read response does not contain a tuples array"
			return 3
		fi
		printf '%s' "$response" | jq -c '.tuples[].key'
		token=$(printf '%s' "$response" | jq -r '.continuation_token // ""')
		if [[ -z "$token" ]]; then
			rm -f "$seen_tokens"
			break
		fi
		fga_remember_token "$seen_tokens" "$token" || {
			local status=$?
			rm -f "$seen_tokens"
			return "$status"
		}
	done
}

fga_apply_batch() {
	local mode="$1"
	local tuple_file="$2"
	local tuples count payload option
	tuples=$(jq -s -c '.' "$tuple_file")
	count=$(printf '%s' "$tuples" | jq 'length')
	[[ "$count" -eq 0 ]] && return 0
	if [[ "$count" -gt "$FGA_BATCH_SIZE" ]]; then
		fga_error "batch contains $count tuples; maximum is $FGA_BATCH_SIZE"
		return 2
	fi

	case "$mode" in
	writes) option="on_duplicate" ;;
	deletes) option="on_missing" ;;
	*)
		fga_error "operation must be writes or deletes"
		return 2
		;;
	esac
	payload=$(jq -n --arg mode "$mode" --arg option "$option" --argjson tuples "$tuples" \
		'{($mode):{"tuple_keys":$tuples,($option):"ignore"}}')
	fga_request write "$payload" >/dev/null
}

fga_apply_tuple_file() {
	local mode="$1"
	local tuple_file="$2"
	local dry_run="$3"
	local batch_file count total

	[[ -f "$tuple_file" ]] || {
		fga_error "tuple file not found: $tuple_file"
		return 2
	}
	total=$(awk 'NF { count++ } END { print count + 0 }' "$tuple_file")
	if [[ "$dry_run" == true ]]; then
		echo "[dry-run] would apply $total $mode"
		return 0
	fi

	batch_file=$(mktemp)
	count=0
	while IFS= read -r tuple; do
		[[ -z "$tuple" ]] && continue
		printf '%s\n' "$tuple" >>"$batch_file"
		count=$((count + 1))
		if [[ "$count" -eq "$FGA_BATCH_SIZE" ]]; then
			fga_apply_batch "$mode" "$batch_file" || {
				local status=$?
				rm -f "$batch_file"
				return "$status"
			}
			: >"$batch_file"
			count=0
		fi
	done <"$tuple_file"
	if [[ "$count" -gt 0 ]]; then
		fga_apply_batch "$mode" "$batch_file" || {
			local status=$?
			rm -f "$batch_file"
			return "$status"
		}
	fi
	rm -f "$batch_file"
}

fga_hash_file() {
	local file="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
	else
		shasum -a 256 "$file" | awk '{print $1}'
	fi
}

fga_write_checkpoint() {
	local directory="$1"
	local manifest_hash summary_hash
	manifest_hash=$(fga_hash_file "$directory/manifest.json")
	summary_hash=$(fga_hash_file "$directory/summary.json")
	jq -n --arg manifest "$manifest_hash" --arg summary "$summary_hash" \
		'{"manifest_sha256":$manifest,"summary_sha256":$summary}' \
		>"$directory/verified.checkpoint"
}

fga_verify_checkpoint() {
	local directory="$1"
	local expected_manifest expected_summary actual_manifest actual_summary
	[[ -f "$directory/verified.checkpoint" ]] || {
		fga_error "verified checkpoint is missing"
		return 6
	}
	expected_manifest=$(jq -r '.manifest_sha256' "$directory/verified.checkpoint")
	expected_summary=$(jq -r '.summary_sha256' "$directory/verified.checkpoint")
	actual_manifest=$(fga_hash_file "$directory/manifest.json")
	actual_summary=$(fga_hash_file "$directory/summary.json")
	if [[ "$expected_manifest" != "$actual_manifest" || "$expected_summary" != "$actual_summary" ]]; then
		fga_error "verified checkpoint does not match current artifacts"
		return 6
	fi
}

fga_check() {
	local user="$1"
	local relation="$2"
	local object="$3"
	local body response
	body=$(jq -n --arg user "$user" --arg relation "$relation" --arg object "$object" \
		'{"tuple_key":{"user":$user,"relation":$relation,"object":$object}}')
	response=$(fga_request check "$body") || return $?
	if ! printf '%s' "$response" | jq -e '.allowed | type == "boolean"' >/dev/null 2>&1; then
		fga_error "/check response does not contain allowed boolean"
		return 3
	fi
	printf '%s' "$response" | jq -r '.allowed'
}
