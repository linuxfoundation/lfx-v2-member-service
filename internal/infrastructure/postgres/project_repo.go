// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jmoiron/sqlx"
)

// ProjectRepo handles PostgreSQL queries for project ID mappings
type ProjectRepo struct {
	db *sqlx.DB
}

// NewProjectRepo creates a new ProjectRepo
func NewProjectRepo(db *sqlx.DB) *ProjectRepo {
	return &ProjectRepo{db: db}
}

const projectIDMappingQuery = `
SELECT sfid, COALESCE(saleforce_id, '') as saleforce_id
FROM salesforce.project__c
WHERE saleforce_id IS NOT NULL AND saleforce_id != ''
`

// FetchProjectIDMapping returns a map of PCC sfid → Salesforce saleforce_id
func (r *ProjectRepo) FetchProjectIDMapping(ctx context.Context) (map[string]string, error) {
	slog.InfoContext(ctx, "fetching project ID mapping from PostgreSQL")

	type row struct {
		SFID        string `db:"sfid"`
		SaleforceID string `db:"saleforce_id"`
	}

	var rows []row
	if err := r.db.SelectContext(ctx, &rows, projectIDMappingQuery); err != nil {
		return nil, fmt.Errorf("failed to fetch project ID mapping: %w", err)
	}

	mapping := make(map[string]string, len(rows))
	for _, r := range rows {
		mapping[r.SFID] = r.SaleforceID
	}

	slog.InfoContext(ctx, "fetched project ID mapping", "count", len(mapping))
	return mapping, nil
}
