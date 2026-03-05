#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Bootstrap the membership-auditors team in OpenFGA via fga-sync.
#
# This script adds one or more users as members of a team object in OpenFGA.
# It publishes messages to the fga-sync service via NATS using the generic
# member_put subject. The fga-sync service must be running and connected to
# the same NATS server.
#
# The "team" object is implicit in OpenFGA — it is created on the first write.
#
# Usage:
#   NATS_URL=nats://localhost:4222 ./scripts/create-membership-auditors-team.sh <principal> [<principal2> ...]
#
# The <principal> values must match the principal claim in the Heimdall JWT
# for the users you want to grant access. Typically this is the OIDC subject
# (e.g. "user@example.com" or a UUID depending on your IdP).
#
# Example:
#   ./scripts/create-membership-auditors-team.sh alice@example.com bob@example.com
#
# Environment variables:
#   NATS_URL    NATS server URL (default: nats://localhost:4222)
#   TEAM_ID     Team identifier in OpenFGA (default: membership-auditors)

set -euo pipefail

NATS_URL="${NATS_URL:-nats://localhost:4222}"
TEAM_ID="${TEAM_ID:-membership-auditors}"

if [ "$#" -lt 1 ]; then
  echo "Usage: $0 <principal> [<principal2> ...]"
  echo ""
  echo "Environment variables:"
  echo "  NATS_URL   NATS server URL (default: nats://localhost:4222)"
  echo "  TEAM_ID    Team ID in OpenFGA (default: membership-auditors)"
  exit 1
fi

if ! command -v nats &>/dev/null; then
  echo "Error: 'nats' CLI not found. Install it from https://github.com/nats-io/natscli"
  exit 1
fi

echo "Team ID:  $TEAM_ID"
echo "NATS URL: $NATS_URL"
echo ""

for principal in "$@"; do
  payload=$(printf \
    '{"object_type":"team","operation":"member_put","data":{"uid":"%s","username":"%s","relations":["member"]}}' \
    "$TEAM_ID" \
    "$principal"
  )
  echo "Adding principal '$principal' to team '$TEAM_ID'..."
  nats pub --server "$NATS_URL" "lfx.fga-sync.member_put" "$payload"
done

echo ""
echo "Done. Verify with:"
echo "  nats pub --server $NATS_URL lfx.fga-sync.member_put '{\"object_type\":\"team\",\"operation\":\"member_put\",...}'"
