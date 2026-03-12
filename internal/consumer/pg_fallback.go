// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jmoiron/sqlx"
)

// pgFallback defines the interface for PostgreSQL point-lookup fallback queries used
// by the dependency resolvers when a record is not yet present in the v1-objects KV
// bucket. This is expected during Meltano incremental backfills, which can take several
// hours and process tables in arbitrary order.
type pgFallback interface {
	// FetchAccount retrieves a single salesforce_b2b Account by its Salesforce ID.
	// Returns nil when the record does not exist.
	FetchAccount(ctx context.Context, sfid string) (*SFAccount, error)

	// FetchProduct2 retrieves a single salesforce_b2b Product2 by its Salesforce ID.
	// Returns nil when the record does not exist.
	FetchProduct2(ctx context.Context, sfid string) (*SFProduct2, error)

	// FetchContact retrieves a single salesforce_b2b Contact by its Salesforce ID.
	// Returns nil when the record does not exist.
	FetchContact(ctx context.Context, sfid string) (*SFContact, error)

	// FetchPrimaryEmail retrieves the primary email address for the given contact SFID
	// from the salesforce_b2b Alternate_Email__c table. Returns an empty string when no
	// primary email is found.
	FetchPrimaryEmail(ctx context.Context, contactSFID string) (string, error)
}

// pgFallbackRepo implements pgFallback using direct PostgreSQL point-lookup queries
// against the salesforce_b2b schema. All queries are read-only SELECT statements with
// indexed WHERE clauses on the "Id" column.
type pgFallbackRepo struct {
	db *sqlx.DB
}

// newPGFallback creates a new pgFallbackRepo backed by the given database connection.
func newPGFallback(db *sqlx.DB) pgFallback {
	return &pgFallbackRepo{db: db}
}

// pgAccount is the SQL scan struct for a single Account point-lookup.
type pgAccount struct {
	ID               string         `db:"id"`
	Name             string         `db:"name"`
	LogoURL          sql.NullString `db:"logo_url"`
	Website          sql.NullString `db:"website"`
	SFIDB2B          sql.NullString `db:"sfid_b2b"`
	IsDeleted        bool           `db:"is_deleted"`
	CreatedDate      sql.NullString `db:"created_date"`
	LastModifiedDate sql.NullString `db:"last_modified_date"`
}

// FetchAccount retrieves a single Account record by SFID.
func (r *pgFallbackRepo) FetchAccount(ctx context.Context, sfid string) (*SFAccount, error) {
	const query = `
		SELECT
			"Id" AS id,
			COALESCE("Name", '') AS name,
			"Logo_URL__c" AS logo_url,
			"Website" AS website,
			"SFID_B2B__c" AS sfid_b2b,
			COALESCE("IsDeleted", false) AS is_deleted,
			"CreatedDate"::text AS created_date,
			"LastModifiedDate"::text AS last_modified_date
		FROM salesforce_b2b."Account"
		WHERE "Id" = $1
		LIMIT 1
	`

	var row pgAccount
	if err := r.db.GetContext(ctx, &row, query, sfid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pg fallback FetchAccount %s: %w", sfid, err)
	}

	slog.DebugContext(ctx, "pg fallback: resolved account from PostgreSQL",
		"sfid", sfid,
		"name", row.Name,
	)

	return &SFAccount{
		ID:               row.ID,
		Name:             row.Name,
		LogoURL:          nullStr(row.LogoURL),
		Website:          nullStr(row.Website),
		SFIDB2B:          nullStr(row.SFIDB2B),
		IsDeleted:        row.IsDeleted,
		CreatedDate:      nullStr(row.CreatedDate),
		LastModifiedDate: nullStr(row.LastModifiedDate),
	}, nil
}

// pgProduct2 is the SQL scan struct for a single Product2 point-lookup.
type pgProduct2 struct {
	ID               string         `db:"id"`
	Name             string         `db:"name"`
	Family           sql.NullString `db:"family"`
	Type             sql.NullString `db:"type"`
	ProjectSFID      sql.NullString `db:"project_sfid"`
	IsDeleted        bool           `db:"is_deleted"`
	CreatedDate      sql.NullString `db:"created_date"`
	LastModifiedDate sql.NullString `db:"last_modified_date"`
}

// FetchProduct2 retrieves a single Product2 record by SFID.
func (r *pgFallbackRepo) FetchProduct2(ctx context.Context, sfid string) (*SFProduct2, error) {
	const query = `
		SELECT
			"Id" AS id,
			COALESCE("Name", '') AS name,
			"Family" AS family,
			"Type__c" AS type,
			"Project__c" AS project_sfid,
			COALESCE("IsDeleted", false) AS is_deleted,
			"CreatedDate"::text AS created_date,
			"LastModifiedDate"::text AS last_modified_date
		FROM salesforce_b2b."Product2"
		WHERE "Id" = $1
		LIMIT 1
	`

	var row pgProduct2
	if err := r.db.GetContext(ctx, &row, query, sfid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pg fallback FetchProduct2 %s: %w", sfid, err)
	}

	slog.DebugContext(ctx, "pg fallback: resolved product2 from PostgreSQL",
		"sfid", sfid,
		"name", row.Name,
	)

	return &SFProduct2{
		ID:               row.ID,
		Name:             row.Name,
		Family:           nullStr(row.Family),
		Type:             nullStr(row.Type),
		ProjectSFID:      nullStr(row.ProjectSFID),
		IsDeleted:        row.IsDeleted,
		CreatedDate:      nullStr(row.CreatedDate),
		LastModifiedDate: nullStr(row.LastModifiedDate),
	}, nil
}

// pgContact is the SQL scan struct for a single Contact point-lookup.
type pgContact struct {
	ID               string         `db:"id"`
	FirstName        sql.NullString `db:"first_name"`
	LastName         sql.NullString `db:"last_name"`
	Title            sql.NullString `db:"title"`
	IsDeleted        bool           `db:"is_deleted"`
	CreatedDate      sql.NullString `db:"created_date"`
	LastModifiedDate sql.NullString `db:"last_modified_date"`
}

// FetchContact retrieves a single Contact record by SFID.
func (r *pgFallbackRepo) FetchContact(ctx context.Context, sfid string) (*SFContact, error) {
	const query = `
		SELECT
			"Id" AS id,
			"FirstName" AS first_name,
			"LastName" AS last_name,
			"Title" AS title,
			COALESCE("IsDeleted", false) AS is_deleted,
			"CreatedDate"::text AS created_date,
			"LastModifiedDate"::text AS last_modified_date
		FROM salesforce_b2b."Contact"
		WHERE "Id" = $1
		LIMIT 1
	`

	var row pgContact
	if err := r.db.GetContext(ctx, &row, query, sfid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pg fallback FetchContact %s: %w", sfid, err)
	}

	slog.DebugContext(ctx, "pg fallback: resolved contact from PostgreSQL",
		"sfid", sfid,
	)

	return &SFContact{
		ID:               row.ID,
		FirstName:        nullStr(row.FirstName),
		LastName:         nullStr(row.LastName),
		Title:            nullStr(row.Title),
		IsDeleted:        row.IsDeleted,
		CreatedDate:      nullStr(row.CreatedDate),
		LastModifiedDate: nullStr(row.LastModifiedDate),
	}, nil
}

// FetchPrimaryEmail retrieves the primary email address for a contact. It queries
// Alternate_Email__c where Primary_Email__c is true; if no primary email exists it
// falls back to the first active non-deleted email.
func (r *pgFallbackRepo) FetchPrimaryEmail(ctx context.Context, contactSFID string) (string, error) {
	const query = `
		SELECT
			COALESCE("Alternate_Email_Address__c", '') AS email,
			COALESCE("Primary_Email__c", false) AS is_primary
		FROM salesforce_b2b."Alternate_Email__c"
		WHERE "Contact_ID__c" = $1
			AND COALESCE("IsDeleted", false) = false
			AND COALESCE("Active__c", false) = true
			AND "Alternate_Email_Address__c" IS NOT NULL
			AND "Alternate_Email_Address__c" != ''
		ORDER BY "Primary_Email__c" DESC, "CreatedDate" ASC
		LIMIT 1
	`

	type emailRow struct {
		Email     string `db:"email"`
		IsPrimary bool   `db:"is_primary"`
	}

	var row emailRow
	if err := r.db.GetContext(ctx, &row, query, contactSFID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("pg fallback FetchPrimaryEmail for contact %s: %w", contactSFID, err)
	}

	slog.DebugContext(ctx, "pg fallback: resolved primary email from PostgreSQL",
		"contact_sfid", contactSFID,
		"is_primary", row.IsPrimary,
	)

	return row.Email, nil
}

// nullStr extracts the string value from a sql.NullString, returning an empty string
// when the value is null.
func nullStr(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}
