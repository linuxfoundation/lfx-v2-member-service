// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"log/slog"
)

// handleAccountUpdate fans out re-indexing of all project_members_b2b and key_contact
// documents linked to the updated Account record. When an account's name, logo, or
// website changes, every denormalized document that references that account must be
// refreshed.
// Returns true if the message should be retried.
func (c *Consumer) handleAccountUpdate(ctx context.Context, sfid string, data map[string]any) bool {
	var account SFAccount
	if err := decodeTyped(data, &account); err != nil {
		slog.ErrorContext(ctx, "b2b handler_account: failed to decode Account record for fan-out",
			"sfid", sfid,
			"error", err,
		)
		return false
	}

	// Retrieve all asset SFIDs linked to this account for downstream fan-out.
	assetSFIDs, err := c.mapping.getAssetsForAccount(ctx, sfid)
	if err != nil {
		slog.ErrorContext(ctx, "b2b handler_account: failed to retrieve account→asset mapping for fan-out",
			"sfid", sfid,
			"error", err,
		)
		// Non-retryable: the mapping lookup failure is unlikely to be transient at this
		// level; the next account update will trigger a fresh fan-out attempt.
		return false
	}

	if len(assetSFIDs) == 0 {
		slog.DebugContext(ctx, "b2b handler_account: no assets linked to updated account, skipping fan-out",
			"sfid", sfid,
		)
		return false
	}

	slog.InfoContext(ctx, "b2b handler_account: fanning out account update to linked assets and project_roles",
		"sfid", sfid,
		"asset_count", len(assetSFIDs),
	)

	shouldRetry := false

	// Fan out to all project_members_b2b documents linked to this account.
	if retry := c.reindexAssetsForAccount(ctx, sfid); retry {
		shouldRetry = true
	}

	// Fan out to all key_contact documents linked to this account's assets, so that
	// company name/logo/website changes propagate to the denormalized key_contact records.
	if retry := c.reindexProjectRolesForAssets(ctx, sfid, assetSFIDs); retry {
		shouldRetry = true
	}

	return shouldRetry
}
