// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// handleProjectRoleUpsert processes a salesforce_b2b-Project_Role__c upsert event.
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

	// A key contact without a linked Contact record is not useful — personal info is
	// the whole point of a key_contact document.
	if role.ContactSFID == "" {
		slog.WarnContext(ctx, "b2b handler_project_role: Project_Role__c has no Contact__c, skipping indexing",
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

	// Resolve the linked Contact for personal fields. A key contact without resolved
	// personal info is not useful, so we skip indexing if the contact cannot be found.
	contact, retry := c.resolveContact(ctx, role.ContactSFID)
	if retry {
		return true
	}
	if contact == nil {
		slog.WarnContext(ctx, "b2b handler_project_role: could not resolve contact for Project_Role__c, skipping",
			"sfid", sfid,
			"contact_sfid", role.ContactSFID,
		)
		return false
	}

	// Resolve the primary email address for the contact from Alternate_Email__c.
	email := c.resolvePrimaryEmail(ctx, role.ContactSFID)

	keyContactUID := generateDeterministicUID(sfid)
	membershipUID := generateDeterministicUID(role.AssetSFID)
	productUID := generateDeterministicUID(asset.Product2ID)

	doc := IndexedKeyContact{
		UID:            keyContactUID,
		MembershipUID:  membershipUID,
		ProductUID:     productUID,
		Role:           role.Role,
		Status:         role.Status,
		BoardMember:    role.BoardMember,
		PrimaryContact: role.PrimaryContact,
		FirstName:      contact.FirstName,
		LastName:       contact.LastName,
		Title:          contact.Title,
		Email:          email,
		ProjectUID:     proj.uid,
		ProjectName:    proj.name,
		ProjectSlug:    proj.slug,
		Parents: []Parent{
			{Type: "project", UID: proj.uid},
			{Type: "project_members_b2b", UID: membershipUID},
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
	if err := c.mapping.addProjectRoleToContact(ctx, role.ContactSFID, sfid); err != nil {
		slog.WarnContext(ctx, "b2b handler_project_role: failed to update contact→project_role mapping",
			"sfid", sfid,
			"contact_sfid", role.ContactSFID,
			"error", err,
		)
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

// handleProjectRoleDeleteWithCleanup processes a salesforce_b2b-Project_Role__c delete event.
// It publishes a delete message to the indexer, and when old data is available (soft deletions)
// it also cleans up the contact → project-roles and asset → project-roles forward-lookup indexes.
// Returns true if the message should be retried.
func (c *Consumer) handleProjectRoleDeleteWithCleanup(ctx context.Context, sfid string, oldData map[string]any) bool {
	keyContactUID := generateDeterministicUID(sfid)

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

	// For soft deletions we have the old record data and can clean up forward-lookup
	// indexes. For hard deletions (oldData == nil), the indexes will contain stale
	// references that are tolerated on next read (KV fetch returns key-not-found).
	if oldData != nil {
		var role SFProjectRole
		if err := decodeTyped(oldData, &role); err == nil {
			if role.ContactSFID != "" {
				if err := c.mapping.removeProjectRoleFromContact(ctx, role.ContactSFID, sfid); err != nil {
					slog.WarnContext(ctx, "b2b handler_project_role: failed to remove contact→project_role mapping on delete",
						"sfid", sfid,
						"contact_sfid", role.ContactSFID,
						"error", err,
					)
				}
			}

			if role.AssetSFID != "" {
				if err := c.mapping.removeProjectRoleFromAsset(ctx, role.AssetSFID, sfid); err != nil {
					slog.WarnContext(ctx, "b2b handler_project_role: failed to remove asset→project_role mapping on delete",
						"sfid", sfid,
						"asset_sfid", role.AssetSFID,
						"error", err,
					)
				}
			}
		}
	}

	return false
}
