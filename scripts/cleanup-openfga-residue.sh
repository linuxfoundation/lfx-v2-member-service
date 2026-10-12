#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Removes a reviewed list of leftover OpenFGA tuples (UUID-keyed b2b_org and
# project_membership residue from LFXV2-2049, plus reviewed anomalies) in
# explicit verify, delete, and restore phases. The list is produced read-only
# elsewhere and bound by its content hash. Nothing outside the list is ever
# written or deleted.
#
# This script writes principal-bearing artifacts with owner-only permissions.
# Keep output outside the repository and follow the retention guidance in
# docs/openfga-residue-cleanup.md.

set -euo pipefail
umask 077
export LC_ALL=C

RESIDUE_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/openfga-global-org-admin-migration.sh
source "$RESIDUE_SCRIPT_DIR/lib/openfga-global-org-admin-migration.sh"

RESIDUE_ALLOWED_TYPES=("b2b_org" "project_membership")
# Reads are idempotent, so a transient failure (a port-forward or gateway
# hiccup) is retried rather than discarding a full verify pass.
RESIDUE_READ_ATTEMPTS=3
RESIDUE_READ_RETRY_SECONDS=2

residue_usage() {
	cat <<'USAGE'
Usage: cleanup-openfga-residue.sh PHASE --store-id ID --list FILE --expect-sha256 HASH --output-dir DIR [options]

Phases: verify, delete, restore
Options:
  --openfga-url URL
  --concurrency N       parallel OpenFGA readers for verification, 1-32 (default 16)
  --pause-seconds S     pause between delete/restore batches (default 0.2)
  --dry-run
  --confirm

verify reads every listed object in full. Tuples already gone are recorded and
skipped; any tuple on a listed object that the list lacks fails closed. delete runs
verify, records exactly the tuples it is about to delete (hash-bound), then deletes
them in batches of at most 100. restore writes back only that recorded set, then
verifies every restored tuple is present.
USAGE
}

residue_line_count() {
	awk 'NF { count++ } END { print count + 0 }' "$1"
}

# Canonical form: one compact, key-sorted tuple per line, sorted and unique.
# OpenFGA reports unconditional tuples with a null condition; a non-null
# condition is kept, so a conditional tuple never compares equal to its
# unconditional form. The list guard refuses conditional tuples and verify
# reports a conditional tuple on a listed object as unexpected, so a
# conditional grant is never deleted or restored without its condition.
residue_canonicalize() {
	local input="$1"
	local output="$2"
	jq -cS 'if .condition == null then {user, relation, object} else {user, relation, object, condition} end' \
		"$input" | LC_ALL=C sort -u >"$output"
}

residue_require_execution_mode() {
	local phase="$1"
	local dry_run="$2"
	local confirm="$3"
	if [[ "$dry_run" == "$confirm" ]]; then
		fga_error "$phase requires exactly one of --dry-run or --confirm"
		return 2
	fi
}

# Binds the reviewed list to its content hash and rejects anything the list must
# never contain: malformed tuples, object types outside the residue types, and the
# stable global org-admin team (its grants are the live administrator path). The
# canonical copy replaces list.jsonl only after every check passes, so the input
# may itself be a previous run's list.jsonl.
residue_load_list() {
	local list="$1"
	local expected="$2"
	local directory="$3"
	[[ -f "$list" ]] || {
		fga_error "list not found: $list"
		return 2
	}
	local canonical actual
	canonical=$(mktemp "$directory/list.XXXXXX") || return 3
	residue_canonicalize "$list" "$canonical" || {
		rm -f "$canonical"
		fga_error "list is not valid tuple JSONL"
		return 2
	}
	actual=$(fga_hash_file "$canonical") || {
		rm -f "$canonical"
		fga_error "could not hash the list"
		return 3
	}
	if [[ -z "$expected" || "$actual" != "$expected" ]]; then
		rm -f "$canonical"
		fga_error "list content sha256 $actual does not match the reviewed hash"
		return 6
	fi
	[[ -s "$canonical" ]] || {
		rm -f "$canonical"
		fga_error "list is empty"
		return 4
	}
	local allowed_types bad
	allowed_types=$(printf '%s\n' "${RESIDUE_ALLOWED_TYPES[@]}" | jq -R . | jq -sc .)
	bad=$(jq -c --argjson allowed "$allowed_types" --arg stable "team:$FGA_STABLE_TEAM#member" \
		'select(
			([.user, .relation, .object] | any(type != "string" or . == ""))
			or .user == $stable
			or has("condition")
			or (.object | test("^[a-z0-9_]+:[^[:space:]]+$") | not)
			or ((.object | split(":")[0]) as $t | $allowed | index($t) | not))' \
		"$canonical" | residue_line_count /dev/stdin) || {
		rm -f "$canonical"
		fga_error "list guard could not be evaluated"
		return 4
	}
	if [[ "$bad" -gt 0 ]]; then
		rm -f "$canonical"
		fga_error "list has $bad malformed tuple(s), tuple(s) outside ${RESIDUE_ALLOWED_TYPES[*]}, or grants to team:$FGA_STABLE_TEAM; refusing"
		return 4
	fi
	mv "$canonical" "$directory/list.jsonl" || {
		rm -f "$canonical"
		fga_error "could not store the checked list"
		return 3
	}
}

# Reads every listed object in full, sharded across parallel readers; any reader
# failure fails the whole read. Every tuple in the list sits on a listed object,
# so this sees each listed tuple and anything else on those objects. Shards live
# beside the output so the caller's cleanup removes them; the caller's interrupt
# trap stops the readers through the dynamically scoped `pids`.
#
# Callers invoke this in `||` contexts, where errexit is off, so every step is
# checked explicitly, and the run fails unless the readers together confirm
# reading exactly as many objects as were requested.
residue_read_objects() {
	local objects="$1"
	local output="$2"
	local concurrency="$3"
	local shard_dir shard
	local -a pids=()
	shard_dir=$(mktemp -d "$(dirname "$output")/shards.XXXXXX") || return 3
	jq -Rc '{object: .}' "$objects" |
		awk -v n="$concurrency" -v d="$shard_dir" '{ print > (d "/shard." (NR % n)) }' || {
		rm -rf "$shard_dir"
		fga_error "could not shard the object list"
		return 3
	}
	for shard in "$shard_dir"/shard.*; do
		[[ -e "$shard" ]] || continue
		: >"$shard.out"
		: >"$shard.done"
		(
			while IFS= read -r filter; do
				# Another reader failed; the run is already lost.
				[[ -e "$shard_dir/failed" ]] && exit 0
				attempt=1
				# Buffer per object so a retried read never duplicates a page.
				until fga_read_all "$filter" >"$shard.object"; do
					reader_status=$?
					if [[ "$attempt" -ge "$RESIDUE_READ_ATTEMPTS" ]]; then
						printf '%s\n' "$reader_status" >"$shard_dir/failed"
						exit "$reader_status"
					fi
					attempt=$((attempt + 1))
					sleep "$RESIDUE_READ_RETRY_SECONDS"
				done
				if ! { cat "$shard.object" >>"$shard.out" && printf '%s\n' "$filter" >>"$shard.done"; }; then
					printf '3\n' >"$shard_dir/failed"
					exit 3
				fi
			done <"$shard"
		) &
		pids+=("$!")
	done
	local pid status=0
	for pid in ${pids[@]+"${pids[@]}"}; do
		wait "$pid" || status=$?
	done
	if [[ -e "$shard_dir/failed" ]]; then
		status=$(head -n 1 "$shard_dir/failed")
	fi
	if [[ "$status" -eq 0 ]]; then
		local requested read_count
		if ! requested=$(residue_line_count "$objects") ||
			! read_count=$(cat "$shard_dir"/shard.*.done | residue_line_count /dev/stdin); then
			status=3
		elif [[ "$requested" -eq 0 || "$read_count" -ne "$requested" ]]; then
			fga_error "read $read_count of $requested objects"
			status=3
		else
			cat "$shard_dir"/shard.*.out >"$output" || status=3
		fi
	fi
	rm -rf "$shard_dir"
	return "$status"
}

# Reads every object named in a tuple file in full and writes their current
# tuples, canonicalized, to the output file.
residue_read_current() {
	local tuples="$1"
	local output="$2"
	local concurrency="$3"
	local temp_dir status=0
	temp_dir=$(mktemp -d) || {
		fga_error "could not create a temporary directory"
		return 3
	}
	# Background readers ignore SIGINT, so stop them and remove every
	# principal-bearing temp file explicitly when the operator interrupts.
	trap 'kill ${pids[@]+"${pids[@]}"} 2>/dev/null; rm -rf "$temp_dir"; exit 130' INT TERM
	jq -r '.object' "$tuples" | LC_ALL=C sort -u >"$temp_dir/objects" || status=3
	if [[ "$status" -eq 0 ]]; then
		residue_read_objects "$temp_dir/objects" "$temp_dir/read" "$concurrency" || status=$?
	fi
	if [[ "$status" -eq 0 ]]; then
		residue_canonicalize "$temp_dir/read" "$output" || status=3
	fi
	rm -rf "$temp_dir"
	trap - INT TERM
	if [[ "$status" -ne 0 ]]; then
		fga_error "OpenFGA read failed"
		return "$status"
	fi
}

# Re-reads every listed object in full. Writes present.jsonl (still in OpenFGA),
# gone.jsonl (already removed; skipped) and unexpected.jsonl (on a listed object
# but not in the list). Any unexpected tuple fails closed.
residue_verify() {
	local directory="$1"
	local concurrency="$2"
	local list="$directory/list.jsonl"
	residue_read_current "$list" "$directory/current.jsonl" "$concurrency" || return $?
	if ! {
		comm -12 "$list" "$directory/current.jsonl" >"$directory/present.jsonl" &&
			comm -23 "$list" "$directory/current.jsonl" >"$directory/gone.jsonl" &&
			comm -13 "$list" "$directory/current.jsonl" >"$directory/unexpected.jsonl"
	}; then
		fga_error "could not compare the list with OpenFGA"
		return 3
	fi
	echo "Verify: $(residue_line_count "$list") listed, $(residue_line_count "$directory/present.jsonl") present," \
		"$(residue_line_count "$directory/gone.jsonl") already gone," \
		"$(residue_line_count "$directory/unexpected.jsonl") unexpected"
	if [[ -s "$directory/unexpected.jsonl" ]]; then
		fga_error "listed objects carry tuples the reviewed list does not have; review unexpected.jsonl and rebuild the list"
		return 4
	fi
}

# Applies a tuple file in batches of at most FGA_BATCH_SIZE with a pause between
# batches, failing on the first batch error.
residue_apply_paced() {
	local mode="$1"
	local tuple_file="$2"
	local pause="$3"
	local batch_file count=0 batches=0 status=0
	# Inside the owner-only output directory, so an interrupted run leaves no
	# principal-bearing file outside the retention scope.
	batch_file=$(mktemp "$(dirname "$tuple_file")/batch.XXXXXX") || return 3
	while IFS= read -r tuple; do
		[[ -z "$tuple" ]] && continue
		printf '%s\n' "$tuple" >>"$batch_file" || {
			status=3
			break
		}
		count=$((count + 1))
		if [[ "$count" -eq "$FGA_BATCH_SIZE" ]]; then
			fga_apply_batch "$mode" "$batch_file" || {
				status=$?
				break
			}
			batches=$((batches + 1))
			: >"$batch_file"
			count=0
			sleep "$pause"
		fi
	done <"$tuple_file"
	if [[ "$status" -eq 0 && "$count" -gt 0 ]]; then
		fga_apply_batch "$mode" "$batch_file" || status=$?
		[[ "$status" -ne 0 ]] || batches=$((batches + 1))
	fi
	rm -f "$batch_file"
	echo "Applied $batches $mode batch(es)"
	return "$status"
}

residue_delete() {
	local directory="$1"
	local dry_run="$2"
	local confirm="$3"
	local concurrency="$4"
	local pause="$5"
	residue_require_execution_mode delete "$dry_run" "$confirm" || return $?
	residue_verify "$directory" "$concurrency" || return $?
	if [[ "$dry_run" == true ]]; then
		echo "[dry-run] would delete $(residue_line_count "$directory/present.jsonl") tuples"
		return 0
	fi
	if [[ ! -s "$directory/present.jsonl" ]]; then
		echo "Nothing left to delete"
		return 0
	fi
	# Record exactly what this run deletes, sealed, before the first write. A
	# retry merges into the committed record so restore covers every run.
	local previous="" candidate
	if [[ -f "$directory/deleted-set.sha256" ]]; then
		residue_validate_deleted_set "$directory" || return $?
		previous=$(residue_deleted_set_path "$directory")
	fi
	candidate=$(mktemp "$directory/deleted-set.next.XXXXXX") || return 3
	{
		cat "$directory/present.jsonl" &&
			if [[ -n "$previous" ]]; then cat "$previous"; fi
	} | LC_ALL=C sort -u >"$candidate" || {
		rm -f "$candidate"
		fga_error "could not build the deleted-set record"
		return 3
	}
	residue_commit_deleted_set "$directory" "$candidate" || return $?
	residue_apply_paced deletes "$directory/present.jsonl" "$pause" || return $?
	residue_verify "$directory" "$concurrency" || return $?
	if [[ -s "$directory/present.jsonl" ]]; then
		fga_error "$(residue_line_count "$directory/present.jsonl") listed tuples remain after delete"
		return 4
	fi
	echo "Delete complete"
}

# The deleted set is content-addressed (deleted-set.<sha256>.jsonl) and the seal
# deleted-set.sha256 ("<sha256> <store-id>") names the one generation it vouches
# for. Replacing the seal with a single rename is the only commit point, so an
# interrupted commit leaves the previous generation and its seal intact, and an
# output directory reused against another store cannot be merged into or
# restored there.
residue_deleted_set_path() {
	printf '%s/deleted-set.%s.jsonl\n' "$1" "$(awk '{ print $1; exit }' "$1/deleted-set.sha256")"
}

residue_commit_deleted_set() {
	local directory="$1"
	local candidate="$2"
	local hash generation
	if ! hash=$(fga_hash_file "$candidate") || [[ ! "$hash" =~ ^[0-9a-f]{64}$ ]]; then
		rm -f "$candidate"
		fga_error "could not hash the deleted-set record"
		return 3
	fi
	mv "$candidate" "$directory/deleted-set.$hash.jsonl" || return $?
	printf '%s %s\n' "$hash" "$STORE_ID" >"$directory/deleted-set.sha256.next" || return $?
	mv "$directory/deleted-set.sha256.next" "$directory/deleted-set.sha256" || return $?
	for generation in "$directory"/deleted-set.*.jsonl; do
		[[ "$generation" == "$directory/deleted-set.$hash.jsonl" ]] || rm -f "$generation"
	done
}

residue_validate_deleted_set() {
	local directory="$1"
	local set_file
	[[ -f "$directory/deleted-set.sha256" ]] || {
		fga_error "no recorded deleted set in $directory"
		return 6
	}
	set_file=$(residue_deleted_set_path "$directory")
	if [[ ! -f "$set_file" || "$(fga_hash_file "$set_file") $STORE_ID" != "$(cat "$directory/deleted-set.sha256")" ]]; then
		fga_error "the recorded deleted set does not match its seal (content hash and store $STORE_ID)"
		return 6
	fi
}

residue_restore() {
	local directory="$1"
	local dry_run="$2"
	local confirm="$3"
	local concurrency="$4"
	local pause="$5"
	residue_require_execution_mode restore "$dry_run" "$confirm" || return $?
	residue_validate_deleted_set "$directory" || return $?
	local set_file
	set_file=$(residue_deleted_set_path "$directory")
	if [[ "$dry_run" == true ]]; then
		echo "[dry-run] would restore $(residue_line_count "$set_file") tuples"
		return 0
	fi
	residue_apply_paced writes "$set_file" "$pause" || return $?
	# Mirror delete's post-check: an acknowledged write that did not persist
	# must fail the rollback, not report success.
	residue_read_current "$set_file" "$directory/current.jsonl" "$concurrency" || return $?
	comm -23 "$set_file" "$directory/current.jsonl" >"$directory/missing.jsonl" || {
		fga_error "could not compare the restored set with OpenFGA"
		return 3
	}
	if [[ -s "$directory/missing.jsonl" ]]; then
		fga_error "$(residue_line_count "$directory/missing.jsonl") recorded tuples are still missing after restore; see missing.jsonl"
		return 4
	fi
	echo "Restore complete: $(residue_line_count "$set_file") tuples present"
}

residue_parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--store-id | --list | --expect-sha256 | --output-dir | --openfga-url | --concurrency | --pause-seconds)
			[[ $# -ge 2 ]] || {
				fga_error "$1 requires a value"
				return 2
			}
			;;
		esac
		case "$1" in
		--store-id)
			RESIDUE_STORE_ID="$2"
			shift 2
			;;
		--list)
			RESIDUE_LIST="$2"
			shift 2
			;;
		--expect-sha256)
			RESIDUE_EXPECTED="$2"
			shift 2
			;;
		--output-dir)
			RESIDUE_OUTPUT_DIR="$2"
			shift 2
			;;
		--openfga-url)
			RESIDUE_BASE_URL="$2"
			shift 2
			;;
		--concurrency)
			RESIDUE_CONCURRENCY="$2"
			shift 2
			;;
		--pause-seconds)
			RESIDUE_PAUSE="$2"
			shift 2
			;;
		--dry-run)
			RESIDUE_DRY_RUN=true
			shift
			;;
		--confirm)
			RESIDUE_CONFIRM=true
			shift
			;;
		*)
			fga_error "unknown argument: $1"
			residue_usage
			return 2
			;;
		esac
	done
}

residue_main() {
	[[ $# -ge 1 ]] || {
		residue_usage
		return 2
	}
	RESIDUE_PHASE="$1"
	shift
	RESIDUE_STORE_ID="" RESIDUE_LIST="" RESIDUE_EXPECTED="" RESIDUE_OUTPUT_DIR=""
	RESIDUE_BASE_URL="${OPENFGA_URL:-http://localhost:8080}"
	RESIDUE_CONCURRENCY=16 RESIDUE_PAUSE=0.2 RESIDUE_DRY_RUN=false RESIDUE_CONFIRM=false
	residue_parse_args "$@" || return $?
	[[ -n "$RESIDUE_STORE_ID" && -n "$RESIDUE_LIST" && -n "$RESIDUE_OUTPUT_DIR" ]] || {
		fga_error "--store-id, --list, and --output-dir are required"
		return 2
	}
	[[ "$RESIDUE_STORE_ID" =~ ^[A-Za-z0-9_-]+$ ]] || {
		fga_error "--store-id contains unsupported characters"
		return 2
	}
	[[ "$RESIDUE_BASE_URL" =~ ^https?://[^[:space:]]+$ ]] || {
		fga_error "--openfga-url must be an HTTP(S) URL without whitespace"
		return 2
	}
	[[ "$RESIDUE_CONCURRENCY" =~ ^[0-9]+$ && "$RESIDUE_CONCURRENCY" -ge 1 && "$RESIDUE_CONCURRENCY" -le 32 ]] || {
		fga_error "--concurrency must be an integer from 1 to 32"
		return 2
	}
	[[ "$RESIDUE_PAUSE" =~ ^[0-9]+(\.[0-9]+)?$ ]] || {
		fga_error "--pause-seconds must be a non-negative number"
		return 2
	}
	BASE_URL="$RESIDUE_BASE_URL"
	STORE_ID="$RESIDUE_STORE_ID"
	export BASE_URL STORE_ID
	if ! { mkdir -p "$RESIDUE_OUTPUT_DIR" && chmod 700 "$RESIDUE_OUTPUT_DIR"; }; then
		fga_error "could not prepare the owner-only output directory $RESIDUE_OUTPUT_DIR"
		return 3
	fi
	echo "OpenFGA target: $BASE_URL store=$STORE_ID phase=$RESIDUE_PHASE"
	residue_load_list "$RESIDUE_LIST" "$RESIDUE_EXPECTED" "$RESIDUE_OUTPUT_DIR" || return $?
	case "$RESIDUE_PHASE" in
	verify) residue_verify "$RESIDUE_OUTPUT_DIR" "$RESIDUE_CONCURRENCY" ;;
	delete)
		residue_delete "$RESIDUE_OUTPUT_DIR" "$RESIDUE_DRY_RUN" "$RESIDUE_CONFIRM" \
			"$RESIDUE_CONCURRENCY" "$RESIDUE_PAUSE"
		;;
	restore)
		residue_restore "$RESIDUE_OUTPUT_DIR" "$RESIDUE_DRY_RUN" "$RESIDUE_CONFIRM" \
			"$RESIDUE_CONCURRENCY" "$RESIDUE_PAUSE"
		;;
	*)
		fga_error "unknown phase: $RESIDUE_PHASE"
		residue_usage
		return 2
		;;
	esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	residue_main "$@"
fi
