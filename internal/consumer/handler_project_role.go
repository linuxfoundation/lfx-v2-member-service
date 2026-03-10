// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// handleProjectRoleUpsert processes a salesforce_b2b-project_role__c upsert event.
// It resolves the linked Asset (for membership/product/project context), Contact (for
// personal fields), and primary Alternate_Email__c (for email), maintains the forward-lookup
// mapping indexes, and publishes a key_contact document to the indexer.
// Returns true if the message should be retried.
func (c *Consumer) handleProjectRoleUpsert(ctx context.Context, sfid string, data map[string]any) bool {
	var role SFProjectRole
	if err := decodeTyped(data, &role); err != nil {
		slog.ErrorContext(ctx, "b2b handler_project_role: failed to decode Project_Role__c record",
			"sfid", sfid,
			"error", err,
		)
		return false
	}

	if role.AssetSFID == "" {
		slog.WarnContext(ctx, "b2b handler_project_role: Project_Role__c has no Asset__c, skipping indexing",
			"sfid", sfid,
		)
		return false
	}

	// Resolve the linked Asset to obtain membership/product/project context.
	asset, retry := c.resolveAsset(ctx, role.AssetSFID)
	if retry {
		return true
	}
	if asset == nil {
		slog.WarnContext(ctx, "b2b handler_project_role: could not resolve asset for Project_Role__c, skipping",
			"sfid", sfid,
			"asset_sfid", role.AssetSFID,
		)
		return false
	}

	if asset.ProjectsSFID == "" {
		slog.WarnContext(ctx, "b2b handler_project_role: linked Asset has no Projects__c, skipping indexing",
			"sfid", sfid,
			"asset_sfid", role.AssetSFID,
		)
		return false
	}

	// Resolve the v2 project via the asset's project SFID.
	proj, retry := c.resolveProject(ctx, asset.ProjectsSFID)
	if retry {
		return true
	}
	if proj == nil {
		slog.WarnContext(ctx, "b2b handler_project_role: could not resolve project for Project_Role__c, skipping",
			"sfid", sfid,
			"project_sfid", asset.ProjectsSFID,
		)
		return false
	}

	// Resolve the linked Account for company denormalization.
	account, retry := c.resolveAccount(ctx, asset.AccountID)
	if retry {
		return true
	}

	// Resolve the linked Contact for personal fields.
	var contact *SFContact
	if role.ContactSFID != "" {
		contact, retry = c.resolveContact(ctx, role.ContactSFID)
		if retry {
			return true
		}
	}

	// Resolve the primary email address for the contact from Alternate_Email__c.
	email := ""
	if role.ContactSFID != "" {
		email = c.resolvePrimaryEmail(ctx, role.ContactSFID)
	}

	keyContactUID := generateProjectRoleUID(sfid)
	membershipUID := generateAssetUID(role.AssetSFID)
	productUID := generateProduct2UID(asset.Product2ID)

	doc := IndexedKeyContact{
		UID:            keyContactUID,
		MembershipUID:  membershipUID,
		ProductUID:     productUID,
		Role:           role.Role,
		Status:         role.Status,
		BoardMember:    role.BoardMember,
		PrimaryContact: role.PrimaryContact,
		Email:          email,
		ProjectUID:     proj.uid,
		ProjectName:    proj.name,
		ProjectSlug:    proj.slug,
		Parents: []Parent{
			{Type: "project", UID: proj.uid},
			{Type: "project_members_b2b", UID: membershipUID},
			{Type: "project_products_b2b", UID: productUID},
		},
		CreatedAt: parseTimestampOrNow(role.CreatedDate),
		UpdatedAt: parseTimestampOrNow(role.LastModifiedDate),
	}

	// Denormalize Account fields when the account record was resolved.
	if account != nil {
		doc.CompanyName = account.Name
		doc.CompanyLogoURL = account.LogoURL
		doc.CompanyWebsite = account.Website
	}

	// Denormalize Contact fields when the contact record was resolved.
	if contact != nil {
		doc.FirstName = contact.FirstName
		doc.LastName = contact.LastName
		doc.Title = contact.Title
	}

	if err := c.indexer.publishUpsert(ctx, constants.IndexKeyContactSubject, doc); err != nil {
		slog.ErrorContext(ctx, "b2b handler_project_role: failed to publish upsert to indexer",
			"sfid", sfid,
			"key_contact_uid", keyContactUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_project_role: indexed key_contact",
		"sfid", sfid,
		"key_contact_uid", keyContactUID,
		"membership_uid", membershipUID,
		"project_uid", proj.uid,
	)

	// Maintain forward-lookup indexes so that contact and asset updates can fan out
	// to this project_role without a full KV scan. Mapping errors are logged but do
	// not cause a retry — the indexer message has already been sent successfully.
	if role.ContactSFID != "" {
		if err := c.mapping.addProjectRoleToContact(ctx, role.ContactSFID, sfid); err != nil {
			slog.WarnContext(ctx, "b2b handler_project_role: failed to update contact→project_role mapping",
				"sfid", sfid,
				"contact_sfid", role.ContactSFID,
				"error", err,
			)
		}
	}

	if err := c.mapping.addProjectRoleToAsset(ctx, role.AssetSFID, sfid); err != nil {
		slog.WarnContext(ctx, "b2b handler_project_role: failed to update asset→project_role mapping",
			"sfid", sfid,
			"asset_sfid", role.AssetSFID,
			"error", err,
		)
	}

	return false
}

// handleProjectRoleDelete processes a salesforce_b2b-project_role__c delete event.
// It publishes a delete message to the indexer and returns true if the message should
// be retried.
func (c *Consumer) handleProjectRoleDelete(ctx context.Context, sfid string) bool {
	keyContactUID := generateProjectRoleUID(sfid)

	if err := c.indexer.publishDelete(ctx, constants.IndexKeyContactSubject, keyContactUID); err != nil {
		slog.ErrorContext(ctx, "b2b handler_project_role: failed to publish delete to indexer",
			"sfid", sfid,
			"key_contact_uid", keyContactUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_project_role: deleted key_contact from index",
		"sfid", sfid,
		"key_contact_uid", keyContactUID,
	)

	return false
}

// handleContactUpdate fans out re-indexing of all key_contact documents linked to
// the updated Contact record. Returns true if the message should be retried.
func (c *Consumer) handleContactUpdate(ctx context.Context, sfid string, _ map[string]any) bool {
	roleSFIDs, err := c.mapping.getProjectRolesForContact(ctx, sfid)
	if err != nil {
		slog.ErrorContext(ctx, "b2b handler_project_role: failed to retrieve contact→project_role mapping for fan-out",
			"contact_sfid", sfid,
			"error", err,
		)
		return false
	}

	if len(roleSFIDs) == 0 {
		slog.DebugContext(ctx, "b2b handler_project_role: no project_roles linked to updated contact, skipping fan-out",
			"contact_sfid", sfid,
		)
		return false
	}

	slog.InfoContext(ctx, "b2b handler_project_role: fanning out contact update to linked project_roles",
		"contact_sfid", sfid,
		"role_count", len(roleSFIDs),
	)

	shouldRetry := false

	for _, roleSFID := range roleSFIDs {
		roleData, fetchErr := c.fetchKVRecord(ctx, fmt.Sprintf("salesforce_b2b-project_role__c.%s", roleSFID))
		if fetchErr != nil {
			slog.WarnContext(ctx, "b2b handler_project_role: failed to fetch project_role for contact fan-out, skipping",
				"contact_sfid", sfid,
				"role_sfid", roleSFID,
				"error", fetchErr,
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
// so that the next lookup will re-fetch the latest name/slug from the KV bucket.
// Returns true if the message should be retried.
func (c *Consumer) handleProjectUpdate(ctx context.Context, sfid string, _ map[string]any) bool {
	c.projectCache.delete(sfid)

	slog.DebugContext(ctx, "b2b handler_project_role: evicted project cache entry on update",
		"project_sfid", sfid,
	)

	return false
}
