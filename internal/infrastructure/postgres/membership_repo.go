// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// MembershipRepo handles PostgreSQL queries for memberships
type MembershipRepo struct {
	db *sqlx.DB
}

// NewMembershipRepo creates a new MembershipRepo
func NewMembershipRepo(db *sqlx.DB) *MembershipRepo {
	return &MembershipRepo{db: db}
}

const membershipQuery = `
SELECT
    a.sfid as id, a.name as name, a.contactid as contactid,
    a.status as status, COALESCE(a.year__c, '') as year,
    COALESCE(a.tier__c, '') as tier, a.recordtypeid as membershiptype,
    COALESCE(a.auto_renew__c, false) as autorenew,
    COALESCE(a.renewal_type__c, '') as renewaltype,
    COALESCE(a.price, 0) as price,
    COALESCE(a.annual_full_price__c, 0) as annualfullprice,
    COALESCE(a.paymentfrequency__c, '') as paymentfrequency,
    COALESCE(a.paymentterms__c, '') as paymentterms,
    a.agreement_date__c as agreementdate,
    COALESCE(a.purchasedate, a.installdate, a.createddate) as purchasedate,
    a.installdate as startdate,
    a.usageenddate as enddate,
    COALESCE(a.accountid, '') as accountid,
    COALESCE(mu.firstname, '') as firstname,
    COALESCE(mu.lastname, '') as lastname,
    COALESCE(aec.alternate_email_address__c, '') as email,
    COALESCE(mu.title, '') as contacttitle,
    COALESCE(p.name, '') as productname,
    COALESCE(p.family, '') as productfamily,
    COALESCE(p.type__c, '') as producttype,
    COALESCE(a.product2id, '') as productid,
    COALESCE(a.projects__c, '') as projectid,
    a.createddate as createddate,
    a.lastmodifieddate as lastmodifiedat,
    COALESCE(acc.name, '') as accountname,
    COALESCE(acc.logo_url__c, '') as accountlogourl,
    COALESCE(acc.website, '') as accountwebsite,
    COALESCE(proj.name, '') as projectname,
    COALESCE(proj.project_logo__c, '') as projectlogourl,
    COALESCE(proj.slug__c, '') as projectslug,
    COALESCE(proj.status__c, '') as projectstatus
FROM salesforce.asset a
LEFT JOIN salesforce.merged_user mu ON mu.sfid = a.contactid
LEFT JOIN salesforce.alternate_email__c aec
    ON aec.leadorcontactid = mu.sfid AND aec.primary_email__c = true
LEFT JOIN salesforce.product2 p ON p.sfid = a.product2id
LEFT JOIN salesforce.account acc ON acc.sfid = a.accountid
LEFT JOIN salesforce.project__c proj ON proj.sfid = a.projects__c
WHERE p.family IN ('Membership', 'Training', 'Alternate Funding')
    AND a.isdeleted = false
`

// FetchAllMemberships fetches all memberships from PostgreSQL
func (r *MembershipRepo) FetchAllMemberships(ctx context.Context) ([]*model.Membership, error) {
	slog.InfoContext(ctx, "fetching all memberships from PostgreSQL")

	var sqlMemberships []SQLMembership
	err := r.db.SelectContext(ctx, &sqlMemberships, membershipQuery)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch memberships from PostgreSQL", "error", err)
		return nil, fmt.Errorf("failed to fetch memberships: %w", err)
	}

	slog.InfoContext(ctx, "fetched memberships from PostgreSQL", "count", len(sqlMemberships))

	memberships := make([]*model.Membership, 0, len(sqlMemberships))
	for _, sqlM := range sqlMemberships {
		m := convertSQLToMembership(sqlM)
		memberships = append(memberships, m)
	}

	return memberships, nil
}

// convertSQLToMembership converts a SQL result to a domain Membership
func convertSQLToMembership(s SQLMembership) *model.Membership {
	m := &model.Membership{
		UID:              generateDeterministicUID(s.ID),
		Name:             s.Name,
		Status:           nullStringValue(s.Status),
		Year:             s.Year,
		Tier:             s.Tier,
		MembershipType:   nullStringValue(s.MembershipType),
		AutoRenew:        s.AutoRenew,
		RenewalType:      s.RenewalType,
		Price:            s.Price,
		AnnualFullPrice:  s.AnnualFullPrice,
		PaymentFrequency: s.PaymentFrequency,
		PaymentTerms:     s.PaymentTerms,
		AgreementDate:    timeToString(s.AgreementDate),
		PurchaseDate:     timeToString(s.PurchaseDate),
		StartDate:        timeToString(s.StartDate),
		EndDate:          timeToString(s.EndDate),
		Account: model.Account{
			ID:      s.AccountID,
			Name:    nullStringValue(s.AccountName),
			LogoURL: nullStringValue(s.AccountLogoURL),
			Website: nullStringValue(s.AccountWebsite),
		},
		Contact: model.Contact{
			ID:        nullStringValue(s.ContactID),
			FirstName: s.FirstName,
			LastName:  s.LastName,
			Email:     s.Email,
			Title:     nullStringValue(s.ContactTitle),
		},
		Product: model.Product{
			ID:     s.ProductID,
			Name:   s.ProductName,
			Family: s.ProductFamily,
			Type:   s.ProductType,
		},
		Project: model.Project{
			ID:      s.ProjectID,
			Name:    nullStringValue(s.ProjectName),
			LogoURL: nullStringValue(s.ProjectLogoURL),
			Slug:    nullStringValue(s.ProjectSlug),
			Status:  nullStringValue(s.ProjectStatus),
		},
	}

	if s.CreatedDate.Valid {
		m.CreatedAt = s.CreatedDate.Time
	} else {
		m.CreatedAt = time.Now()
	}

	if s.LastModifiedAt.Valid {
		m.UpdatedAt = s.LastModifiedAt.Time
	} else {
		m.UpdatedAt = m.CreatedAt
	}

	return m
}

// generateDeterministicUID generates a deterministic UUID v5 from a Salesforce ID
func generateDeterministicUID(sfid string) string {
	namespace := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // DNS namespace
	hash := sha256.Sum256([]byte("lfx-membership:" + sfid))
	return uuid.NewSHA1(namespace, hash[:]).String()
}
