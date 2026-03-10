// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"log/slog"
)

// handleContactUpdate fans out re-indexing of all key_contact documents linked to
// the updated Contact record. Returns true if the message should be retried.
func (c *Consumer) handleContactUpdate(ctx context.Context, sfid string, _ map[string]any) bool {
	roleSFIDs, err := c.mapping.getProjectRolesForContact(ctx, sfid)
	if err != nil {
		slog.ErrorContext(ctx, "b2b handler_contact: failed to retrieve contact→project_role mapping for fan-out",
			"contact_sfid", sfid,
			"error", err,
		)
		return false
	}

	if len(roleSFIDs) == 0 {
		slog.DebugContext(ctx, "b2b handler_contact: no project_roles linked to updated contact, skipping fan-out",
			"contact_sfid", sfid,
		)
		return false
	}

	slog.InfoContext(ctx, "b2b handler_contact: fanning out contact update to linked project_roles",
		"contact_sfid", sfid,
		"role_count", len(roleSFIDs),
	)

	shouldRetry := false

	for _, roleSFID := range roleSFIDs {
		roleData, fetchErr := c.fetchKVRecord(ctx, "salesforce_b2b-project_role__c."+roleSFID)
		if fetchErr != nil {
			// Stale forward-index reference from a hard deletion — the project_role
			// record no longer exists in KV. This is expected and only needs debug
			// logging; the stale entry will be harmlessly skipped on future fan-outs.
			slog.DebugContext(ctx, "b2b handler_contact: stale project_role reference in contact index, skipping",
				"contact_sfid", sfid,
				"role_sfid", roleSFID,
				"error", fetchErr,
			)
			continue
		}

		// Skip soft-deleted project_role records — they should not be re-indexed.
		if isSoftDeleted(roleData) {
			slog.DebugContext(ctx, "b2b handler_contact: skipping soft-deleted project_role during contact fan-out",
				"contact_sfid", sfid,
				"role_sfid", roleSFID,
			)
			continue
		}

		if retry := c.handleProjectRoleUpsert(ctx, roleSFID, roleData); retry {
			shouldRetry = true
		}
	}

	return shouldRetry
}

// handleProjectUpdate invalidates the project cache entry for the given project SFID
// so that the next lookup will re-fetch the latest v1 B2B name/slug from the KV bucket.
// Returns true if the message should be retried.
func (c *Consumer) handleProjectUpdate(ctx context.Context, sfid string, _ map[string]any) bool {
	c.projectCache.delete(sfid)

	slog.DebugContext(ctx, "b2b handler_contact: evicted project cache entry on update",
		"project_sfid", sfid,
	)

	return false
}
