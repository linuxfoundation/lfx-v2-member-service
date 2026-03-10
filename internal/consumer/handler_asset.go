// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

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

	// Resolve the linked Product2 from v1-objects KV.
	product, retry := c.resolveProduct2(ctx, asset.Product2ID)
	if retry {
		return true
	}

	membershipUID := generateAssetUID(sfid)
	productUID := generateProduct2UID(asset.Product2ID)

	// Build a display name in the format "{company_name} - {product_name}".
	companyName := ""
	if account != nil {
		companyName = account.Name
	}
	productName := ""
	if product != nil {
		productName = product.Name
	}
	displayName := buildMembershipName(companyName, productName)

	doc := IndexedProjectMemberB2B{
		UID:              membershipUID,
		Name:             displayName,
		Aliases:          []string{displayName},
		Status:           asset.Status,
		Year:             asset.Year,
		Tier:             asset.Tier,
		MembershipType:   asset.RecordTypeID,
		AutoRenew:        asset.AutoRenew,
		RenewalType:      asset.RenewalType,
		Price:            asset.Price,
		AnnualFullPrice:  asset.AnnualFullPrice,
		PaymentFrequency: asset.PaymentFrequency,
		PaymentTerms:     asset.PaymentTerms,
		AgreementDate:    asset.AgreementDate,
		PurchaseDate:     coalesceDate(asset.PurchaseDate, asset.InstallDate, asset.CreatedDate),
		StartDate:        asset.InstallDate,
		EndDate:          asset.UsageEndDate,
		ProductUID:       productUID,
		ProjectUID:       proj.uid,
		ProjectName:      proj.name,
		ProjectSlug:      proj.slug,
		Parents: []Parent{
			{Type: "project", UID: proj.uid},
			{Type: "project_products_b2b", UID: productUID},
		},
		CreatedAt: parseTimestampOrNow(asset.CreatedDate),
		UpdatedAt: parseTimestampOrNow(asset.LastModifiedDate),
	}

	// Denormalize Account fields when the account record was resolved.
	if account != nil {
		doc.CompanyName = account.Name
		doc.CompanyLogoURL = account.LogoURL
		doc.CompanyWebsite = account.Website
	}

	// Denormalize Product2 fields when the product record was resolved.
	if product != nil {
		doc.ProductName = product.Name
		doc.ProductFamily = product.Family
		doc.ProductType = product.Type
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
	if asset.AccountID != "" {
		if err := c.mapping.addAssetToAccount(ctx, asset.AccountID, sfid); err != nil {
			slog.WarnContext(ctx, "b2b handler_asset: failed to update account→asset mapping",
				"sfid", sfid,
				"account_sfid", asset.AccountID,
				"error", err,
			)
		}
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
	membershipUID := generateAssetUID(sfid)

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

// handleAccountUpdate fans out re-indexing of all project_members_b2b and key_contact
// documents linked to the updated Account record. Returns true if the message should be retried.
func (c *Consumer) handleAccountUpdate(ctx context.Context, sfid string, data map[string]any) bool {
	var account SFAccount
	if err := decodeTyped(data, &account); err != nil {
		slog.ErrorContext(ctx, "b2b handler_asset: failed to decode Account record for fan-out",
			"sfid", sfid,
			"error", err,
		)
		return false
	}

	// Fan out to all assets associated with this account.
	assetSFIDs, err := c.mapping.getAssetsForAccount(ctx, sfid)
	if err != nil {
		slog.ErrorContext(ctx, "b2b handler_asset: failed to retrieve account→asset mapping for fan-out",
			"sfid", sfid,
			"error", err,
		)
		// Non-retryable: the mapping lookup failure is unlikely to be transient at this
		// level; the next account update will trigger a fresh fan-out attempt.
		return false
	}

	if len(assetSFIDs) == 0 {
		slog.DebugContext(ctx, "b2b handler_asset: no assets linked to updated account, skipping fan-out",
			"sfid", sfid,
		)
		return false
	}

	slog.InfoContext(ctx, "b2b handler_asset: fanning out account update to linked assets",
		"sfid", sfid,
		"asset_count", len(assetSFIDs),
	)

	shouldRetry := false

	for _, assetSFID := range assetSFIDs {
		assetData, fetchErr := c.fetchKVRecord(ctx, fmt.Sprintf("salesforce_b2b-asset.%s", assetSFID))
		if fetchErr != nil {
			slog.WarnContext(ctx, "b2b handler_asset: failed to fetch asset for account fan-out, skipping",
				"account_sfid", sfid,
				"asset_sfid", assetSFID,
				"error", fetchErr,
			)
			continue
		}

		if retry := c.handleAssetUpsert(ctx, assetSFID, assetData); retry {
			shouldRetry = true
		}
	}

	// Fan out to all project_role records linked to this account via their assets.
	// Each asset's project_role fan-out will be triggered when the asset is re-indexed above,
	// but contact fields are not account-derived — only company fields on key_contact need
	// refreshing. Re-index via project_role handler for each role linked to each asset.
	for _, assetSFID := range assetSFIDs {
		roleSFIDs, roleErr := c.mapping.getProjectRolesForAsset(ctx, assetSFID)
		if roleErr != nil {
			slog.WarnContext(ctx, "b2b handler_asset: failed to retrieve asset→project_role mapping for account fan-out",
				"account_sfid", sfid,
				"asset_sfid", assetSFID,
				"error", roleErr,
			)
			continue
		}

		for _, roleSFID := range roleSFIDs {
			roleData, fetchErr := c.fetchKVRecord(ctx, fmt.Sprintf("salesforce_b2b-project_role__c.%s", roleSFID))
			if fetchErr != nil {
				slog.WarnContext(ctx, "b2b handler_asset: failed to fetch project_role for account fan-out, skipping",
					"account_sfid", sfid,
					"role_sfid", roleSFID,
					"error", fetchErr,
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

// ---- Shared helpers ----

// buildMembershipName constructs the display name for a project_members_b2b document
// in the format "{company_name} - {product_name}". Falls back gracefully when either
// part is empty.
func buildMembershipName(companyName, productName string) string {
	switch {
	case companyName != "" && productName != "":
		return fmt.Sprintf("%s - %s", companyName, productName)
	case companyName != "":
		return companyName
	case productName != "":
		return productName
	default:
		return ""
	}
}

// coalesceDate returns the first non-empty string from the provided values.
func coalesceDate(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
