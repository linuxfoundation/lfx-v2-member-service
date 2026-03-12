// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
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
    a."Id" as id, a."Name" as name, a."ContactId" as contactid,
    a."Status" as status, COALESCE(a."Year__c", '') as year,
    COALESCE(a."Tier__c", '') as tier, a."RecordTypeId" as membershiptype,
    COALESCE(a."Auto_Renew__c", false) as autorenew,
    COALESCE(a."Renewal_Type__c", '') as renewaltype,
    COALESCE(a."Price", 0) as price,
    COALESCE(a."Annual_Full_Price__c", 0) as annualfullprice,
    COALESCE(a."PaymentFrequency__c", '') as paymentfrequency,
    COALESCE(a."PaymentTerms__c", '') as paymentterms,
    a."Agreement_Date__c" as agreementdate,
    COALESCE(a."PurchaseDate", a."InstallDate", a."CreatedDate") as purchasedate,
    a."InstallDate" as startdate,
    a."UsageEndDate" as enddate,
    COALESCE(a."AccountId", '') as accountid,
    COALESCE(c."FirstName", '') as firstname,
    COALESCE(c."LastName", '') as lastname,
    COALESCE(aec."Alternate_Email_Address__c", '') as email,
    COALESCE(c."Title", '') as contacttitle,
    COALESCE(p."Name", '') as productname,
    COALESCE(p."Family", '') as productfamily,
    COALESCE(p."Type__c", '') as producttype,
    COALESCE(a."Product2Id", '') as productid,
    COALESCE(a."Projects__c", '') as projectid,
    a."CreatedDate" as createddate,
    a."LastModifiedDate" as lastmodifiedat,
    COALESCE(acc."Name", '') as accountname,
    COALESCE(acc."Logo_URL__c", '') as accountlogourl,
    COALESCE(acc."Website", '') as accountwebsite,
    COALESCE(proj."Name", '') as projectname,
    COALESCE(proj."Project_Logo__c", '') as projectlogourl,
    COALESCE(proj."Slug__c", '') as projectslug,
    COALESCE(proj."Status__c", '') as projectstatus
FROM salesforce_b2b."Asset" a
LEFT JOIN salesforce_b2b."Contact" c ON c."Id" = a."ContactId"
LEFT JOIN salesforce_b2b."Alternate_Email__c" aec
    ON aec."Contact_ID__c" = c."Id" AND aec."Primary_Email__c" = true
LEFT JOIN salesforce_b2b."Product2" p ON p."Id" = a."Product2Id"
LEFT JOIN salesforce_b2b."Account" acc ON acc."Id" = a."AccountId"
LEFT JOIN salesforce_b2b."Project__c" proj ON proj."Id" = a."Projects__c"
WHERE p."Family" = 'Membership'
    AND a."IsDeleted" = false
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
		MemberUID:        generateMemberUID(s.AccountID),
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

// generateDeterministicUID generates a deterministic UUID v5 from a Salesforce ID.
// Uses the DNS namespace with the "lfx-membership:" prefix, matching the existing
// sync job convention. Must remain consistent with stored KV data.
func generateDeterministicUID(sfid string) string {
	namespace := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // DNS namespace.
	return uuid.NewSHA1(namespace, []byte("lfx-membership:"+sfid)).String()
}
