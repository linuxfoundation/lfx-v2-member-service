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
    pr.sfid as id, pr.asset__c as membershipid,
    pr.role__c as role, pr.status__c as rolestatus,
    pr.boardmember__c as boardmember, pr.primarycontact__c as primarycontact,
    pr.contact__c as contactid,
    COALESCE(mu.firstname, '') as firstname,
    COALESCE(mu.lastname, '') as lastname,
    COALESCE(aec.alternate_email_address__c, '') as email,
    COALESCE(mu.title, '') as title,
    COALESCE(ac.sfid, '') as orgid,
    COALESCE(ac.name, '') as orgname,
    COALESCE(ac.logo_url__c, '') as orglogourl,
    COALESCE(ac.website, '') as orgwebsite,
    COALESCE(p.sfid, '') as projectid,
    COALESCE(p.name, '') as projectname,
    COALESCE(p.project_logo__c, '') as projectlogourl,
    pr.createddate as createddate, pr.systemmodstamp as updatedat
FROM salesforce.project_role__c pr
INNER JOIN salesforce.merged_user mu ON mu.sfid = pr.contact__c
INNER JOIN salesforce.alternate_email__c aec
    ON aec.leadorcontactid = mu.sfid AND aec.primary_email__c = true
INNER JOIN salesforce.asset a ON a.sfid = pr.asset__c
INNER JOIN salesforce.project__c p ON p.sfid = a.projects__c
INNER JOIN salesforce.account ac ON ac.sfid = a.accountid
WHERE pr.isdeleted = false
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
