#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# setup-global-org-admin-team.sh
#
# Seeds (or updates) the global org-admin team membership tuples in OpenFGA.
# Each user in ADMIN_USERS gets a `member` relation on `team:<TEAM_NAME>`,
# which transitively grants `writer` and `auditor` access to every b2b_org.
#
# Prerequisites:
#   - kubectl port-forward to the target OpenFGA on localhost:8080, OR
#     run from inside the cluster (lfx-platform-nats-box has curl + jq)
#   - jq installed
#
# Required env vars:
#   OPENFGA_STORE_ID     — OpenFGA store ID for the target environment
#   TEAM_NAME            — stable global org-admin team name
#   ADMIN_USERS          — comma-separated LFID usernames (e.g. "alice,bob")
#
# Optional env vars:
#   OPENFGA_URL          — default: http://localhost:8080
#
# IMPORTANT — user format:
#   Heimdall passes the LFID username as the FGA principal. ADMIN_USERS entries
#   must be plain LFID usernames (no "auth0|" prefix) or the Heimdall check will
#   never match and all callers will receive 403.
#
# Usage:
#   # Port-forward first (local):
#   kubectl -n lfx port-forward svc/lfx-platform-openfga 8080:8080
#
#   OPENFGA_STORE_ID=<store-id> \
#   TEAM_NAME=global_org_admin \
#   ADMIN_USERS="alice,bob" \
#   ./scripts/setup-global-org-admin-team.sh [--dry-run]
#

set -euo pipefail

BASE_URL="${OPENFGA_URL:-http://localhost:8080}"
STORE_ID="${OPENFGA_STORE_ID:?OPENFGA_STORE_ID must be set}"
TEAM_NAME="${TEAM_NAME:?TEAM_NAME must be set to the unqualified team name}"
ADMIN_USERS="${ADMIN_USERS:?ADMIN_USERS must be set (comma-separated LFID usernames)}"
DRY_RUN=false

if [[ "$TEAM_NAME" != "global_org_admin" ]]; then
	echo "TEAM_NAME must be exactly global_org_admin" >&2
	exit 2
fi

for arg in "$@"; do
	case "$arg" in
		--dry-run) DRY_RUN=true ;;
		*) echo "Unknown argument: $arg"; exit 1 ;;
	esac
done

if [[ "$DRY_RUN" == true ]]; then
	echo "=== DRY RUN MODE — no writes will be performed ==="
fi

echo "Store:    $STORE_ID"
echo "Team name: $TEAM_NAME"
echo "Base URL: $BASE_URL"
echo ""

TEAM_OBJECT="team:${TEAM_NAME}"

# fga_read paginates through /read for a given tuple_key JSON fragment,
# collecting all results into stdout as newline-separated user strings.
fga_read() {
	local filter_json="$1"
	local token=""
	while true; do
		local body
		if [[ -z "$token" ]]; then
			body=$(jq -n --argjson tk "$filter_json" '{"tuple_key":$tk,"page_size":100}')
		else
			body=$(jq -n --argjson tk "$filter_json" --arg ct "$token" \
				'{"tuple_key":$tk,"page_size":100,"continuation_token":$ct}')
		fi
		local resp
		if ! resp=$(curl -sf --show-error -X POST "${BASE_URL}/stores/${STORE_ID}/read" \
			-H 'Content-Type: application/json' \
			-d "$body" 2>&1); then
			echo "ERROR: curl failed: $resp" >&2
			exit 1
		fi
		if echo "$resp" | jq -e '.code' >/dev/null 2>&1; then
			echo "ERROR reading tuples: $(echo "$resp" | jq -r '.message')" >&2
			exit 1
		fi
		echo "$resp" | jq -r '.tuples[]?.key.user'
		token=$(echo "$resp" | jq -r '.continuation_token // ""')
		[[ -z "$token" ]] && break
	done
}

# ---------------------------------------------------------------------------
# Step 1: Read existing members (paginated)
# ---------------------------------------------------------------------------

echo "=== Step 1: Reading existing team members ==="

existing_users=$(fga_read "$(jq -n --arg obj "$TEAM_OBJECT" '{"object":$obj,"relation":"member"}')")

if [[ -n "$existing_users" ]]; then
	echo "Existing members:"
	while IFS= read -r line; do echo "  $line"; done <<< "$existing_users"
else
	echo "  (none)"
fi
echo ""

# ---------------------------------------------------------------------------
# Step 2: Determine which users need to be written
# ---------------------------------------------------------------------------

echo "=== Step 2: Computing writes ==="

IFS=',' read -ra users <<< "$ADMIN_USERS"

tuples_to_write="[]"
for raw_user in "${users[@]}"; do
	username=$(echo "$raw_user" | tr -d '[:space:]')
	[[ -z "$username" ]] && continue
	fga_user="user:${username}"

	if echo "$existing_users" | grep -qxF "$fga_user"; then
		echo "  SKIP (already member): $fga_user"
	else
		echo "  WRITE: $fga_user -> member -> $TEAM_OBJECT"
		tuples_to_write=$(echo "$tuples_to_write" | jq \
			--arg u "$fga_user" --arg r "member" --arg o "$TEAM_OBJECT" \
			'. + [{"user":$u,"relation":$r,"object":$o}]')
	fi
done
echo ""

write_count=$(echo "$tuples_to_write" | jq 'length')
if [[ "$write_count" -eq 0 ]]; then
	echo "All users are already members — nothing to do."
	exit 0
fi

# ---------------------------------------------------------------------------
# Step 3: Write new member tuples
# ---------------------------------------------------------------------------

echo "=== Step 3: Writing $write_count tuple(s) ==="

if [[ "$DRY_RUN" == true ]]; then
	echo "[DRY RUN] Would write:"
	echo "$tuples_to_write" | jq -r '.[] | "  \(.user) -> \(.relation) -> \(.object)"'
	exit 0
fi

payload=$(jq -n --argjson keys "$tuples_to_write" '{"writes":{"tuple_keys":$keys}}')
write_resp=""
if ! write_resp=$(curl -sf --show-error -X POST "${BASE_URL}/stores/${STORE_ID}/write" \
	-H 'Content-Type: application/json' \
	-d "$payload" 2>&1); then
	echo "ERROR: curl failed: $write_resp"
	exit 1
fi
if echo "$write_resp" | jq -e '.code' >/dev/null 2>&1; then
	echo "ERROR writing tuples: $(echo "$write_resp" | jq -r '.message')"
	exit 1
fi

echo "  Done — $write_count tuple(s) written."
echo ""

# ---------------------------------------------------------------------------
# Step 4: Verify (paginated)
# ---------------------------------------------------------------------------

echo "=== Step 4: Verifying ==="

echo "Current members of $TEAM_OBJECT:"
fga_read "$(jq -n --arg obj "$TEAM_OBJECT" '{"object":$obj,"relation":"member"}')" | \
	while IFS= read -r line; do echo "  $line"; done
echo ""
echo "=== Done ==="
