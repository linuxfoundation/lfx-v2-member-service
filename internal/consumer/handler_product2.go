// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// handleProduct2Upsert processes a salesforce_b2b-product2 upsert event. It resolves
// the associated v2 project UID, publishes a project_products_b2b document to the
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
		slog.WarnContext(ctx, "b2b handler_product2: could not resolve project for Product2, skipping",
			"sfid", sfid,
			"project_sfid", projectSFID,
		)
		return false
	}

	productUID := generateDeterministicUID(sfid)

	doc := IndexedProjectProductB2B{
		UID:         productUID,
		Name:        product.Name,
		Aliases:     []string{product.Name},
		Family:      product.Family,
		Type:        product.Type,
		ProjectUID:  proj.uid,
		ProjectName: proj.name,
		ProjectSlug: proj.slug,
		Parents: []Parent{
			{Type: "project", UID: proj.uid},
		},
		CreatedAt: parseTimestampOrNow(product.CreatedDate),
		UpdatedAt: parseTimestampOrNow(product.LastModifiedDate),
	}

	if err := c.indexer.publishUpsert(ctx, constants.IndexProjectProductsB2BSubject, doc); err != nil {
		slog.ErrorContext(ctx, "b2b handler_product2: failed to publish upsert to indexer",
			"sfid", sfid,
			"product_uid", productUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_product2: indexed project_products_b2b",
		"sfid", sfid,
		"product_uid", productUID,
		"project_uid", proj.uid,
	)

	return false
}

// handleProduct2Delete processes a salesforce_b2b-product2 delete event. It publishes
// a delete message to the indexer and returns true if the message should be retried.
func (c *Consumer) handleProduct2Delete(ctx context.Context, sfid string) bool {
	productUID := generateDeterministicUID(sfid)

	if err := c.indexer.publishDelete(ctx, constants.IndexProjectProductsB2BSubject, productUID); err != nil {
		slog.ErrorContext(ctx, "b2b handler_product2: failed to publish delete to indexer",
			"sfid", sfid,
			"product_uid", productUID,
			"error", err,
		)
		return true
	}

	slog.InfoContext(ctx, "b2b handler_product2: deleted project_products_b2b from index",
		"sfid", sfid,
		"product_uid", productUID,
	)

	return false
}
