// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// handleProduct2Upsert processes a salesforce_b2b-Product2 upsert event. It resolves
// the associated v2 project UID, publishes a project_membership_tier document to the
// indexer, and returns true if the message should be retried.
func (c *Consumer) handleProduct2Upsert(ctx context.Context, sfid string, data map[string]any) bool {
	var product SFProduct2
	if err := decodeTyped(data, &product); err != nil {
		slog.ErrorContext(ctx, "b2b handler_product2: failed to decode Product2 record",
			"sfid", sfid,
			"error", err,
		)
		return false
	}

	// Resolve the associated v2 project from Product2.Project__c.
	projectSFID := product.ProjectSFID
	if projectSFID == "" {
		slog.WarnContext(ctx, "b2b handler_product2: Product2 has no Project__c, skipping indexing",
			"sfid", sfid,
		)
		return false
	}

	proj, retry := c.resolveProject(ctx, projectSFID)
	if retry {
		return true
	}
	if proj == nil {
		slog.InfoContext(ctx, "b2b handler_product2: could not resolve project for Product2, skipping (project may not be in v2)",
			"sfid", sfid,
			"project_sfid", projectSFID,
		)
		return false
	}

	productUID := generateDeterministicUID(sfid)

	doc := IndexedProjectMembershipTier{
		UID:         productUID,
		Name:        product.Name,
		Aliases:     []string{product.Name},
		Family:      product.Family,
		Type:        product.Type,
		ProjectUID:  proj.uid,
		ProjectName: proj.name,
		ProjectSlug: proj.slug,
		CreatedAt:   parseTimestampOrNow(product.CreatedDate),
		UpdatedAt:   parseTimestampOrNow(product.LastModifiedDate),
	}

	cfg := &IndexingConfig{
		ObjectID:             productUID,
		AccessCheckObject:    fmt.Sprintf("project:%s", proj.uid),
		AccessCheckRelation:  "auditor",
		HistoryCheckObject:   fmt.Sprintf("project:%s", proj.uid),
		HistoryCheckRelation: "auditor",
		NameAndAliases:       []string{product.Name},
		SortName:             product.Name,
		ParentRefs:           []string{fmt.Sprintf("project:%s", proj.uid)},
	}

	if err := c.indexer.publishUpsert(ctx, constants.IndexProjectMembershipTierSubject, doc, cfg); err != nil {
		slog.ErrorContext(ctx, "b2b handler_product2: failed to publish upsert to indexer",
			"sfid", sfid,
			"product_uid", productUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_product2: indexed project_membership_tier",
		"sfid", sfid,
		"product_uid", productUID,
		"project_uid", proj.uid,
	)

	// Fan out to re-index all project_members_b2b documents that reference this product,
	// so that denormalized product name/family/type changes are propagated.
	if retry := c.reindexAssetsForProduct2(ctx, sfid); retry {
		return true
	}

	return false
}

// reindexAssetsForProduct2 re-indexes all project_membership documents linked to the
// given product2 SFID via the product2 → assets forward-lookup index. Soft-deleted or
// missing (hard-deleted) assets are skipped with debug logging.
func (c *Consumer) reindexAssetsForProduct2(ctx context.Context, product2SFID string) bool {
	assetSFIDs, err := c.mapping.getAssetsForProduct2(ctx, product2SFID)
	if err != nil {
		slog.ErrorContext(ctx, "b2b handler_product2: failed to retrieve product2→asset mapping for fan-out",
			"product2_sfid", product2SFID,
			"error", err,
		)
		return false
	}

	if len(assetSFIDs) == 0 {
		slog.DebugContext(ctx, "b2b handler_product2: no assets linked to product2, skipping fan-out",
			"product2_sfid", product2SFID,
		)
		return false
	}

	slog.InfoContext(ctx, "b2b handler_product2: fanning out to linked assets",
		"product2_sfid", product2SFID,
		"asset_count", len(assetSFIDs),
	)

	shouldRetry := false

	for _, assetSFID := range assetSFIDs {
		assetData, fetchErr := c.fetchKVRecord(ctx, fmt.Sprintf("salesforce_b2b-Asset.%s", assetSFID))
		if fetchErr != nil {
			// Stale forward-index reference from a hard-deleted asset.
			slog.DebugContext(ctx, "b2b handler_product2: stale asset reference in product2 index, skipping",
				"product2_sfid", product2SFID,
				"asset_sfid", assetSFID,
				"error", fetchErr,
			)
			continue
		}

		// Skip soft-deleted assets.
		if isSoftDeleted(assetData) {
			slog.DebugContext(ctx, "b2b handler_product2: skipping soft-deleted asset during fan-out",
				"product2_sfid", product2SFID,
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

// handleProduct2Delete processes a salesforce_b2b-Product2 delete event. It publishes
// a delete message to the indexer and returns true if the message should be retried.
func (c *Consumer) handleProduct2Delete(ctx context.Context, sfid string) bool {
	productUID := generateDeterministicUID(sfid)

	if err := c.indexer.publishDelete(ctx, constants.IndexProjectMembershipTierSubject, productUID); err != nil {
		slog.ErrorContext(ctx, "b2b handler_product2: failed to publish delete to indexer",
			"sfid", sfid,
			"product_uid", productUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_product2: deleted project_membership_tier from index",
		"sfid", sfid,
		"product_uid", productUID,
	)

	return false
}
