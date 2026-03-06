// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// KeyContactRepo handles PostgreSQL queries for key contacts
type KeyContactRepo struct {
	db *sqlx.DB
}

// NewKeyContactRepo creates a new KeyContactRepo
func NewKeyContactRepo(db *sqlx.DB) *KeyContactRepo {
	return &KeyContactRepo{db: db}
}

const keyContactQuery = `
SELECT
    pr."Id" as id, pr."Asset__c" as membershipid,
    pr."Role__c" as role, pr."Status__c" as rolestatus,
    pr."BoardMember__c" as boardmember, pr."PrimaryContact__c" as primarycontact,
    pr."Contact__c" as contactid,
    COALESCE(c."FirstName", '') as firstname,
    COALESCE(c."LastName", '') as lastname,
    COALESCE(aec."Alternate_Email_Address__c", '') as email,
    COALESCE(c."Title", '') as title,
    COALESCE(ac."Id", '') as orgid,
    COALESCE(ac."Name", '') as orgname,
    COALESCE(ac."Logo_URL__c", '') as orglogourl,
    COALESCE(ac."Website", '') as orgwebsite,
    COALESCE(p."Id", '') as projectid,
    COALESCE(p."Name", '') as projectname,
    COALESCE(p."Project_Logo__c", '') as projectlogourl,
    pr."CreatedDate" as createddate, pr."SystemModstamp" as updatedat
FROM salesforce_b2b."Project_Role__c" pr
INNER JOIN salesforce_b2b."Contact" c ON c."Id" = pr."Contact__c"
INNER JOIN salesforce_b2b."Alternate_Email__c" aec
    ON aec."Contact_ID__c" = c."Id" AND aec."Primary_Email__c" = true
INNER JOIN salesforce_b2b."Asset" a ON a."Id" = pr."Asset__c"
INNER JOIN salesforce_b2b."Project__c" p ON p."Id" = a."Projects__c"
INNER JOIN salesforce_b2b."Account" ac ON ac."Id" = a."AccountId"
WHERE pr."IsDeleted" = false
`

// FetchAllKeyContacts fetches all key contacts from PostgreSQL
func (r *KeyContactRepo) FetchAllKeyContacts(ctx context.Context) ([]*model.KeyContact, error) {
	slog.InfoContext(ctx, "fetching all key contacts from PostgreSQL")

	var sqlContacts []SQLKeyContact
	err := r.db.SelectContext(ctx, &sqlContacts, keyContactQuery)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch key contacts from PostgreSQL", "error", err)
		return nil, fmt.Errorf("failed to fetch key contacts: %w", err)
	}

	slog.InfoContext(ctx, "fetched key contacts from PostgreSQL", "count", len(sqlContacts))

	contacts := make([]*model.KeyContact, 0, len(sqlContacts))
	for _, sqlC := range sqlContacts {
		c := convertSQLToKeyContact(sqlC)
		contacts = append(contacts, c)
	}

	return contacts, nil
}

// convertSQLToKeyContact converts a SQL result to a domain KeyContact
func convertSQLToKeyContact(s SQLKeyContact) *model.KeyContact {
	c := &model.KeyContact{
		UID:            generateDeterministicUID(s.ID),
		MembershipUID:  generateDeterministicUID(s.MembershipID),
		Role:           nullStringValue(s.Role),
		Status:         nullStringValue(s.RoleStatus),
		BoardMember:    s.BoardMember.Valid && s.BoardMember.Bool,
		PrimaryContact: s.PrimaryContact.Valid && s.PrimaryContact.Bool,
		Contact: model.Contact{
			ID:        nullStringValue(s.ContactID),
			FirstName: s.FirstName,
			LastName:  s.LastName,
			Email:     s.Email,
			Title:     s.Title,
		},
		Project: model.Project{
			ID:      s.ProjectID,
			Name:    s.ProjectName,
			LogoURL: s.ProjectLogoURL,
		},
		Organization: model.Organization{
			ID:      s.OrgID,
			Name:    s.OrgName,
			LogoURL: s.OrgLogoURL,
			Website: s.OrgWebsite,
		},
	}

	if s.CreatedDate.Valid {
		c.CreatedAt = s.CreatedDate.Time
	} else {
		c.CreatedAt = time.Now()
	}

	if s.UpdatedAt.Valid {
		c.UpdatedAt = s.UpdatedAt.Time
	} else {
		c.UpdatedAt = c.CreatedAt
	}

	return c
}
