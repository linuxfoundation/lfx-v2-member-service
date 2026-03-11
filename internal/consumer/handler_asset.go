// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// corporateRecordTypeIDs maps Salesforce RecordTypeId values to known membership
// categories. Only assets with a RecordTypeId present in this map are indexed.
// Non-corporate record types (individual, supporter, non-membership products)
// are expected to become distinct v2 resource types and are filtered out here.
var corporateRecordTypeIDs = map[string]string{
	"01241000001E1jAAAS": "Corporate",
}

// handleAssetUpsert processes a salesforce_b2b-asset upsert event. It resolves
// the linked Account, Product2, and Project records, maintains the forward-lookup
// mapping indexes, and publishes a project_members_b2b document to the indexer.
// Returns true if the message should be retried.
func (c *Consumer) handleAssetUpsert(ctx context.Context, sfid string, data map[string]any) bool {
	var asset SFAsset
	if err := decodeTyped(data, &asset); err != nil {
		slog.ErrorContext(ctx, "b2b handler_asset: failed to decode Asset record",
			"sfid", sfid,
			"error", err,
		)
		return false
	}

	// Only index assets whose product family is "Membership".
	if asset.ProductFamily != "Membership" {
		slog.DebugContext(ctx, "b2b handler_asset: Asset product family is not Membership, skipping indexing",
			"sfid", sfid,
			"product_family", asset.ProductFamily,
		)
		return false
	}

	// Only index assets with a recognized corporate RecordTypeId.
	if _, ok := corporateRecordTypeIDs[asset.RecordTypeID]; !ok {
		slog.DebugContext(ctx, "b2b handler_asset: Asset RecordTypeId is not a corporate membership type, skipping indexing",
			"sfid", sfid,
			"record_type_id", asset.RecordTypeID,
		)
		return false
	}

	if asset.Product2ID == "" {
		slog.WarnContext(ctx, "b2b handler_asset: Asset has no Product2Id, skipping indexing",
			"sfid", sfid,
		)
		return false
	}

	if asset.ProjectsSFID == "" {
		slog.WarnContext(ctx, "b2b handler_asset: Asset has no Projects__c, skipping indexing",
			"sfid", sfid,
		)
		return false
	}

	// Projects__c may be a multi-select picklist separated by semicolons. We only
	// support single-project assets; warn and skip if multiple values are present.
	if strings.Contains(asset.ProjectsSFID, ";") {
		slog.WarnContext(ctx, "b2b handler_asset: Asset has multiple Projects__c values (semicolon), skipping indexing",
			"sfid", sfid,
			"projects__c", asset.ProjectsSFID,
		)
		return false
	}

	if asset.AccountID == "" {
		slog.WarnContext(ctx, "b2b handler_asset: Asset has no AccountId, skipping indexing",
			"sfid", sfid,
		)
		return false
	}

	// Resolve the v2 project.
	proj, retry := c.resolveProject(ctx, asset.ProjectsSFID)
	if retry {
		return true
	}
	if proj == nil {
		slog.WarnContext(ctx, "b2b handler_asset: could not resolve project for Asset, skipping",
			"sfid", sfid,
			"project_sfid", asset.ProjectsSFID,
		)
		return false
	}

	// Resolve the linked Account from v1-objects KV.
	account, retry := c.resolveAccount(ctx, asset.AccountID)
	if retry {
		return true
	}
	if account == nil {
		slog.ErrorContext(ctx, "b2b handler_asset: could not resolve account for Asset, skipping",
			"sfid", sfid,
			"account_sfid", asset.AccountID,
		)
		return false
	}

	// Resolve the linked Product2 from v1-objects KV.
	product, retry := c.resolveProduct2(ctx, asset.Product2ID)
	if retry {
		return true
	}
	if product == nil {
		slog.ErrorContext(ctx, "b2b handler_asset: could not resolve product2 for Asset, skipping",
			"sfid", sfid,
			"product2_sfid", asset.Product2ID,
		)
		return false
	}

	membershipUID := generateDeterministicUID(sfid)
	productUID := generateDeterministicUID(asset.Product2ID)

	// Build a display name for typeahead search in the format "{company_name} - {product_name}".
	displayName := buildMembershipName(account.Name, product.Name)

	doc := IndexedProjectMemberB2B{
		UID:             membershipUID,
		Name:            displayName,
		Aliases:         []string{displayName},
		Status:          asset.Status,
		Year:            asset.Year,
		Tier:            asset.Tier,

		AnnualFullPrice: asset.AnnualFullPrice,
		AgreementDate:   asset.AgreementDate,
		PurchaseDate:    coalesceDate(asset.PurchaseDate, asset.InstallDate, asset.CreatedDate),
		StartDate:       asset.InstallDate,
		EndDate:         asset.UsageEndDate,
		CompanyName:     account.Name,
		CompanyLogoURL:  account.LogoURL,
		CompanyWebsite:  account.Website,
		ProductName:     product.Name,
		ProductFamily:   product.Family,
		ProductType:     product.Type,
		ProductUID:      productUID,
		ProjectUID:      proj.uid,
		ProjectName:     proj.name,
		ProjectSlug:     proj.slug,
		Parents: []Parent{
			{Type: "project", UID: proj.uid},
			{Type: "project_products_b2b", UID: productUID},
		},
		CreatedAt: parseTimestampOrNow(asset.CreatedDate),
		UpdatedAt: parseTimestampOrNow(asset.LastModifiedDate),
	}

	if err := c.indexer.publishUpsert(ctx, constants.IndexProjectMembersB2BSubject, doc); err != nil {
		slog.ErrorContext(ctx, "b2b handler_asset: failed to publish upsert to indexer",
			"sfid", sfid,
			"membership_uid", membershipUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_asset: indexed project_members_b2b",
		"sfid", sfid,
		"membership_uid", membershipUID,
		"project_uid", proj.uid,
	)

	// Maintain forward-lookup indexes so that account and product updates can fan out
	// to this asset without a full KV scan. Mapping errors are logged but do not cause
	// a retry — the indexer message has already been sent successfully.
	if err := c.mapping.addAssetToAccount(ctx, asset.AccountID, sfid); err != nil {
		slog.WarnContext(ctx, "b2b handler_asset: failed to update account→asset mapping",
			"sfid", sfid,
			"account_sfid", asset.AccountID,
			"error", err,
		)
	}

	if err := c.mapping.addAssetToProduct2(ctx, asset.Product2ID, sfid); err != nil {
		slog.WarnContext(ctx, "b2b handler_asset: failed to update product2→asset mapping",
			"sfid", sfid,
			"product2_sfid", asset.Product2ID,
			"error", err,
		)
	}

	return false
}

// handleAssetDelete processes a salesforce_b2b-asset delete event. It publishes a delete
// message to the indexer and returns true if the message should be retried.
// Note: deletion cascades to key_contact records are not implemented (logged as warning).
func (c *Consumer) handleAssetDelete(ctx context.Context, sfid string) bool {
	membershipUID := generateDeterministicUID(sfid)

	if err := c.indexer.publishDelete(ctx, constants.IndexProjectMembersB2BSubject, membershipUID); err != nil {
		slog.ErrorContext(ctx, "b2b handler_asset: failed to publish delete to indexer",
			"sfid", sfid,
			"membership_uid", membershipUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_asset: deleted project_members_b2b from index",
		"sfid", sfid,
		"membership_uid", membershipUID,
	)

	// Cascade deletion of associated key_contact records is not implemented.
	// Child project_role__c records will be removed when their own delete events arrive.
	slog.WarnContext(ctx, "b2b handler_asset: key_contact cascade delete not implemented; child records will be removed on their own delete events",
		"sfid", sfid,
	)

	return false
}

// handleAssetDeleteWithCleanup wraps handleAssetDelete and additionally cleans up
// forward-lookup indexes when old data is available (soft deletions).
// Returns true if the message should be retried.
func (c *Consumer) handleAssetDeleteWithCleanup(ctx context.Context, sfid string, oldData map[string]any) bool {
	retry := c.handleAssetDelete(ctx, sfid)

	// Clean up forward-lookup indexes when old data is available (soft deletion).
	if oldData != nil {
		var asset SFAsset
		if err := decodeTyped(oldData, &asset); err == nil {
			if asset.AccountID != "" {
				if err := c.mapping.removeAssetFromAccount(ctx, asset.AccountID, sfid); err != nil {
					slog.WarnContext(ctx, "b2b handler_asset: failed to remove asset from account→asset mapping on delete",
						"sfid", sfid,
						"account_sfid", asset.AccountID,
						"error", err,
					)
				}
			}
			if asset.Product2ID != "" {
				if err := c.mapping.removeAssetFromProduct2(ctx, asset.Product2ID, sfid); err != nil {
					slog.WarnContext(ctx, "b2b handler_asset: failed to remove asset from product2→asset mapping on delete",
						"sfid", sfid,
						"product2_sfid", asset.Product2ID,
						"error", err,
					)
				}
			}
		}
	}

	return retry
}

// reindexAssetsForAccount re-indexes all project_members_b2b documents linked to the
// given account SFID via the account → assets forward-lookup index. Soft-deleted or
// missing (hard-deleted) assets are skipped with debug logging.
func (c *Consumer) reindexAssetsForAccount(ctx context.Context, accountSFID string) bool {
	assetSFIDs, err := c.mapping.getAssetsForAccount(ctx, accountSFID)
	if err != nil {
		slog.ErrorContext(ctx, "b2b handler_asset: failed to retrieve account→asset mapping for fan-out",
			"account_sfid", accountSFID,
			"error", err,
		)
		return false
	}

	if len(assetSFIDs) == 0 {
		slog.DebugContext(ctx, "b2b handler_asset: no assets linked to account, skipping fan-out",
			"account_sfid", accountSFID,
		)
		return false
	}

	slog.InfoContext(ctx, "b2b handler_asset: fanning out to linked assets",
		"account_sfid", accountSFID,
		"asset_count", len(assetSFIDs),
	)

	shouldRetry := false

	for _, assetSFID := range assetSFIDs {
		assetData, fetchErr := c.fetchKVRecord(ctx, fmt.Sprintf("salesforce_b2b-asset.%s", assetSFID))
		if fetchErr != nil {
			// Stale forward-index reference from a hard-deleted asset.
			slog.DebugContext(ctx, "b2b handler_asset: stale asset reference in account index, skipping",
				"account_sfid", accountSFID,
				"asset_sfid", assetSFID,
				"error", fetchErr,
			)
			continue
		}

		// Skip soft-deleted assets.
		if isSoftDeleted(assetData) {
			slog.DebugContext(ctx, "b2b handler_asset: skipping soft-deleted asset during fan-out",
				"account_sfid", accountSFID,
				"asset_sfid", assetSFID,
			)
			continue
		}

		if retry := c.handleAssetUpsert(ctx, assetSFID, assetData); retry {
			shouldRetry = true
		}
	}

	return shouldRetry
}

// reindexProjectRolesForAssets re-indexes all key_contact documents linked to the given
// asset SFIDs via the asset → project_roles forward-lookup index. Soft-deleted or missing
// (hard-deleted) project_role records are skipped with debug logging.
func (c *Consumer) reindexProjectRolesForAssets(ctx context.Context, contextSFID string, assetSFIDs []string) bool {
	shouldRetry := false

	for _, assetSFID := range assetSFIDs {
		roleSFIDs, roleErr := c.mapping.getProjectRolesForAsset(ctx, assetSFID)
		if roleErr != nil {
			slog.WarnContext(ctx, "b2b handler_asset: failed to retrieve asset→project_role mapping for fan-out",
				"context_sfid", contextSFID,
				"asset_sfid", assetSFID,
				"error", roleErr,
			)
			continue
		}

		for _, roleSFID := range roleSFIDs {
			roleData, fetchErr := c.fetchKVRecord(ctx, fmt.Sprintf("salesforce_b2b-project_role__c.%s", roleSFID))
			if fetchErr != nil {
				// Stale forward-index reference from a hard-deleted project_role.
				slog.DebugContext(ctx, "b2b handler_asset: stale project_role reference in asset index, skipping",
					"context_sfid", contextSFID,
					"role_sfid", roleSFID,
					"error", fetchErr,
				)
				continue
			}

			// Skip soft-deleted project_role records.
			if isSoftDeleted(roleData) {
				slog.DebugContext(ctx, "b2b handler_asset: skipping soft-deleted project_role during fan-out",
					"context_sfid", contextSFID,
					"role_sfid", roleSFID,
				)
				continue
			}

			if retry := c.handleProjectRoleUpsert(ctx, roleSFID, roleData); retry {
				shouldRetry = true
			}
		}
	}

	return shouldRetry
}
