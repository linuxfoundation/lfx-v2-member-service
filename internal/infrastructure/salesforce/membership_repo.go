// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// membershipSOQL fetches all Asset records with a Membership product family,
// including related Account, Product2, and Project__c data via relationship
// sub-selects. The Contact sub-select is omitted here — the primary contact on
// an Asset is a billing contact, not a key contact; key contacts are fetched
// separately via Project_Role__c.
//
// Note: Alternate_Email__c is not queried here. Email resolution for the
// membership's billing contact is performed in the key contact repo.
const membershipSOQL = `
SELECT
    Id, Name, Status, AccountId, Product2Id,
    Year__c, Tier__c, Auto_Renew__c,
    Renewal_Type__c, Price, Annual_Full_Price__c,
    PaymentFrequency__c, PaymentTerms__c,
    Agreement_Date__c, PurchaseDate, InstallDate, UsageEndDate,
    Projects__c, CreatedDate, LastModifiedDate,
    Account.Id, Account.Name, Account.Logo_URL__c, Account.Website,
    Product2.Id, Product2.Name, Product2.Family, Product2.Type__c,
    Projects__r.Id, Projects__r.Name, Projects__r.Project_Logo__c,
    Projects__r.Slug__c, Projects__r.Status__c
FROM Asset
WHERE Product2.Family = 'Membership'
    AND IsDeleted = false
`

// membershipByIDSOQL fetches a single Asset record by its Salesforce ID.
// The caller must substitute a quoteSOQL-escaped ID for the %s placeholder.
const membershipByIDSOQL = `
SELECT
    Id, Name, Status, AccountId, Product2Id,
    Year__c, Tier__c, Auto_Renew__c,
    Renewal_Type__c, Price, Annual_Full_Price__c,
    PaymentFrequency__c, PaymentTerms__c,
    Agreement_Date__c, PurchaseDate, InstallDate, UsageEndDate,
    Projects__c, CreatedDate, LastModifiedDate,
    Account.Id, Account.Name, Account.Logo_URL__c, Account.Website,
    Product2.Id, Product2.Name, Product2.Family, Product2.Type__c,
    Projects__r.Id, Projects__r.Name, Projects__r.Project_Logo__c,
    Projects__r.Slug__c, Projects__r.Status__c
FROM Asset
WHERE Id = %s
    AND Product2.Family = 'Membership'
    AND IsDeleted = false
`

// membershipsByAccountSOQLBase is the SELECT and fixed WHERE base for
// FetchMembershipsByAccountSFID. The caller appends additional AND clauses for
// any active MembershipFilters, then an ORDER BY clause, before executing the query.
const membershipsByAccountSOQLBase = `
SELECT
    Id, Name, Status, AccountId, Product2Id,
    Year__c, Tier__c, Auto_Renew__c,
    Renewal_Type__c, Price, Annual_Full_Price__c,
    PaymentFrequency__c, PaymentTerms__c,
    Agreement_Date__c, PurchaseDate, InstallDate, UsageEndDate,
    Projects__c, CreatedDate, LastModifiedDate,
    Account.Id, Account.Name, Account.Logo_URL__c, Account.Website,
    Product2.Id, Product2.Name, Product2.Family, Product2.Type__c,
    Projects__r.Id, Projects__r.Name, Projects__r.Project_Logo__c,
    Projects__r.Slug__c, Projects__r.Status__c
FROM Asset
WHERE AccountId = %s
    AND Product2.Family = 'Membership'
    AND IsDeleted = false`

// membershipsByProjectSOQLBase is the SELECT and fixed WHERE base for
// FetchMembershipsByProjectSFID and FetchMembershipPage. The caller appends
// additional AND clauses for any active MembershipFilters, then an ORDER BY
// clause, before executing the query.
const membershipsByProjectSOQLBase = `
SELECT
    Id, Name, Status, AccountId, Product2Id,
    Year__c, Tier__c, Auto_Renew__c,
    Renewal_Type__c, Price, Annual_Full_Price__c,
    PaymentFrequency__c, PaymentTerms__c,
    Agreement_Date__c, PurchaseDate, InstallDate, UsageEndDate,
    Projects__c, CreatedDate, LastModifiedDate,
    Account.Id, Account.Name, Account.Logo_URL__c, Account.Website,
    Product2.Id, Product2.Name, Product2.Family, Product2.Type__c,
    Projects__r.Id, Projects__r.Name, Projects__r.Project_Logo__c,
    Projects__r.Slug__c, Projects__r.Status__c
FROM Asset
WHERE Projects__c = %s
    AND Product2.Family = 'Membership'
    AND Status = 'Active'
    AND IsDeleted = false`

// soqlOrderByClause returns the ORDER BY fragment for the given SortOrder.
// An unrecognised or empty sort order falls back to the default (newest first).
func soqlOrderByClause(order model.SortOrder) string {
	switch order {
	case model.SortOrderName:
		return "\nORDER BY Account.Name ASC NULLS LAST"
	case model.SortOrderLastModified:
		return "\nORDER BY LastModifiedDate DESC NULLS LAST"
	default:
		// SortOrderNewest and any unrecognised value.
		return "\nORDER BY CreatedDate DESC NULLS LAST"
	}
}

// MembershipRepo handles Salesforce SOQL queries for project memberships (Assets).
type MembershipRepo struct {
	client *sf.Salesforce
}

// NewMembershipRepo creates a new MembershipRepo backed by the given Salesforce client.
func NewMembershipRepo(client *sf.Salesforce) *MembershipRepo {
	return &MembershipRepo{client: client}
}

// FetchMembershipBySFID fetches a single membership Asset by its Salesforce ID.
// Returns nil if the asset is not found or is not a membership.
func (r *MembershipRepo) FetchMembershipBySFID(ctx context.Context, assetSFID string) (*model.ProjectMembership, error) {
	slog.DebugContext(ctx, "fetching membership from Salesforce by SFID", "sfid", assetSFID)

	var assets []soqlAsset
	if err := r.client.Query(fmt.Sprintf(membershipByIDSOQL, quoteSOQL(assetSFID)), &assets); err != nil {
		return nil, fmt.Errorf("fetching membership by SFID %s: %w", assetSFID, err)
	}

	if len(assets) == 0 {
		return nil, nil
	}

	return convertSOQLToProjectMembership(assets[0])
}

// membershipsByIDsSOQL fetches multiple Asset records by a set of Salesforce
// IDs. The caller must substitute a buildSOQLInClause result for the %s
// placeholder. Applies the same IsDeleted = false and
// Product2.Family = 'Membership' filters as the single-record path.
const membershipsByIDsSOQL = `
SELECT
    Id, Name, Status, AccountId, Product2Id,
    Year__c, Tier__c, Auto_Renew__c,
    Renewal_Type__c, Price, Annual_Full_Price__c,
    PaymentFrequency__c, PaymentTerms__c,
    Agreement_Date__c, PurchaseDate, InstallDate, UsageEndDate,
    Projects__c, CreatedDate, LastModifiedDate,
    Account.Id, Account.Name, Account.Logo_URL__c, Account.Website,
    Product2.Id, Product2.Name, Product2.Family, Product2.Type__c,
    Projects__r.Id, Projects__r.Name, Projects__r.Project_Logo__c,
    Projects__r.Slug__c, Projects__r.Status__c
FROM Asset
WHERE Id IN (%s)
    AND Product2.Family = 'Membership'
    AND IsDeleted = false
`

// FetchMembershipsBySFIDs fetches multiple membership Assets by their
// Salesforce IDs in a single batched SOQL query. Applies the same
// IsDeleted = false and Product2.Family = 'Membership' filters as the
// single-record path so that soft-deleted or non-membership IDs are absent
// from the result. Returns an empty slice when none are found.
func (r *MembershipRepo) FetchMembershipsBySFIDs(ctx context.Context, sfids []string) ([]*model.ProjectMembership, []string, error) {
	if len(sfids) == 0 {
		return []*model.ProjectMembership{}, nil, nil
	}
	slog.DebugContext(ctx, "fetching memberships by SFIDs from Salesforce (batch)", "count", len(sfids))

	const batchSize = 200
	all := make([]*model.ProjectMembership, 0, len(sfids))
	var conversionErrorSFIDs []string
	for start := 0; start < len(sfids); start += batchSize {
		end := start + batchSize
		if end > len(sfids) {
			end = len(sfids)
		}
		chunk := sfids[start:end]

		var assets []soqlAsset
		if err := r.client.Query(fmt.Sprintf(membershipsByIDsSOQL, buildSOQLInClause(chunk)), &assets); err != nil {
			return nil, nil, fmt.Errorf("batch fetching memberships (chunk %d-%d): %w", start, end, err)
		}
		for _, asset := range assets {
			pm, convErr := convertSOQLToProjectMembership(asset)
			if convErr != nil {
				slog.WarnContext(ctx, "skipping membership with conversion error in batch",
					"sfid", asset.ID, "error", convErr)
				conversionErrorSFIDs = append(conversionErrorSFIDs, asset.ID)
				continue
			}
			all = append(all, pm)
		}
	}
	return all, conversionErrorSFIDs, nil
}

// Ensure MembershipRepo satisfies the port at compile time.
var _ port.MembershipBatchReader = (*MembershipRepo)(nil)

// FetchMembershipsByAccountSFID fetches all membership Assets for a given
// Salesforce Account ID and returns them as ProjectMembership domain objects.
func (r *MembershipRepo) FetchMembershipsByAccountSFID(ctx context.Context, accountSFID string) ([]*model.ProjectMembership, error) {
	slog.DebugContext(ctx, "fetching memberships for account from Salesforce",
		"account_sfid", accountSFID,
	)

	query := fmt.Sprintf(membershipsByAccountSOQLBase, quoteSOQL(accountSFID)) +
		soqlOrderByClause(model.SortOrderDefault)
	var assets []soqlAsset
	if err := r.client.Query(query, &assets); err != nil {
		return nil, fmt.Errorf("fetching memberships for account %s: %w", accountSFID, err)
	}

	memberships := make([]*model.ProjectMembership, 0, len(assets))
	for _, asset := range assets {
		m, err := convertSOQLToProjectMembership(asset)
		if err != nil {
			slog.WarnContext(ctx, "skipping membership with invalid SFID",
				"sfid", asset.ID,
				"error", err,
			)
			continue
		}
		memberships = append(memberships, m)
	}

	return memberships, nil
}

// buildMembershipsByProjectSOQL assembles the full SOQL query string for
// FetchMembershipsByProjectSFID and FetchMembershipPage, appending optional
// filter predicates and an ORDER BY clause. All interpolated values are passed
// through quoteSOQL to prevent injection.
//
// Tier__c is intentionally not used: the field is inaccessible to the connected-
// app user at the field-level read permission level (always null in results).
// Tier filtering uses Product2Id (exact, decoded from TierUID) instead.
func buildMembershipsByProjectSOQL(ctx context.Context, projectSFID string, filters model.MembershipFilters) string {
	var b strings.Builder
	fmt.Fprintf(&b, membershipsByProjectSOQLBase, quoteSOQL(projectSFID))
	if filters.TierUID != "" {
		// Normalize the tier SFID to its canonical 18-char form for an exact-match
		// filter. If normalization fails the UID is used as-is; it will return zero
		// results rather than cause a query error.
		tierSFID, err := sfuuid.Normalize18(filters.TierUID)
		if err != nil {
			slog.WarnContext(ctx, "failed to normalize tier UID to SFID; using raw value",
				"tier_uid", filters.TierUID,
				"error", err,
			)
			tierSFID = filters.TierUID
		}
		fmt.Fprintf(&b, "\n    AND Product2Id = %s", quoteSOQL(tierSFID))
	}
	if filters.CompanyNameSearch != "" {
		// Push the company name search down to SOQL as a LIKE predicate.
		// CompanyNameSearch is always lowercase by contract (normalised by
		// the caller), so the same value is interpolated into both the query
		// and the NATS KV cache key. quoteLikeSOQL handles all escaping and quoting
		// in a single pass, producing a complete '%term%' literal ready for
		// direct interpolation.
		fmt.Fprintf(&b, "\n    AND Account.Name LIKE %s", quoteLikeSOQL(filters.CompanyNameSearch))
	}
	b.WriteString(soqlOrderByClause(filters.EffectiveSortOrder()))
	return b.String()
}

// FetchMembershipsByProjectSFID fetches membership Assets for a given Salesforce
// Project__c ID, applying any non-empty MembershipFilters as additional SOQL
// WHERE predicates. Returns the results as ProjectMembership domain objects.
//
// This method fetches ALL pages automatically (via the go-salesforce library).
// For large projects prefer FetchMembershipPage, which returns a single page
// with a continuation token.
func (r *MembershipRepo) FetchMembershipsByProjectSFID(ctx context.Context, projectSFID string, filters model.MembershipFilters) ([]*model.ProjectMembership, error) {
	slog.DebugContext(ctx, "fetching memberships for project from Salesforce",
		"project_sfid", projectSFID,
		"filter_tier_uid", filters.TierUID,
		"sort_order", filters.EffectiveSortOrder(),
	)

	query := buildMembershipsByProjectSOQL(ctx, projectSFID, filters)
	var assets []soqlAsset
	if err := r.client.Query(query, &assets); err != nil {
		return nil, fmt.Errorf("fetching memberships for project %s: %w", projectSFID, err)
	}

	memberships := make([]*model.ProjectMembership, 0, len(assets))
	for _, asset := range assets {
		m, err := convertSOQLToProjectMembership(asset)
		if err != nil {
			slog.WarnContext(ctx, "skipping membership with invalid SFID",
				"sfid", asset.ID,
				"error", err,
			)
			continue
		}
		memberships = append(memberships, m)
	}

	return memberships, nil
}

// MembershipBatchResult is the return type of FetchMembershipPage. It carries
// both the logical page slice (for the HTTP response) and the full raw SF batch
// (for the NATS KV cache), keeping them separate so the cache layer can store
// the batch and slice it consistently on subsequent reads.
// FirstBatchResult is the return value of FetchFirstMembershipBatch. It carries
// the first sfQueryBatchSize records, the Salesforce locator for the remainder
// (if any), and the total result set size reported by Salesforce.
type FirstBatchResult struct {
	// Records is the full first SF batch (up to sfQueryBatchSize records).
	Records []*model.ProjectMembership

	// SFLocator is the raw Salesforce nextRecordsUrl for the records beyond
	// the first batch. Empty when the first batch contains all results (i.e.
	// the result set is ≤ sfQueryBatchSize records).
	SFLocator string

	// TotalSize is the total record count reported by Salesforce.
	TotalSize int
}

// FetchFirstMembershipBatch issues a single SOQL query for the first
// sfQueryBatchSize membership records for the given project and filters,
// returning the full batch and the Salesforce locator for any remaining
// records. The caller is responsible for following the locator in a background
// goroutine via QueryAllPages if SFLocator is non-empty.
func (r *MembershipRepo) FetchFirstMembershipBatch(ctx context.Context, projectSFID string, filters model.MembershipFilters) (FirstBatchResult, error) {
	slog.DebugContext(ctx, "fetching first membership batch from Salesforce",
		"project_sfid", projectSFID,
		"filter_tier_uid", filters.TierUID,
		"sort_order", filters.EffectiveSortOrder(),
	)

	query := buildMembershipsByProjectSOQL(ctx, projectSFID, filters)
	sfResult, err := QueryPage[soqlAsset](ctx, r.client, query, "")
	if err != nil {
		return FirstBatchResult{}, fmt.Errorf("fetching first membership batch for project %s: %w", projectSFID, err)
	}

	records := make([]*model.ProjectMembership, 0, len(sfResult.Records))
	for _, asset := range sfResult.Records {
		m, convErr := convertSOQLToProjectMembership(asset)
		if convErr != nil {
			slog.WarnContext(ctx, "skipping membership with invalid SFID",
				"sfid", asset.ID,
				"error", convErr,
			)
			continue
		}
		records = append(records, m)
	}

	return FirstBatchResult{
		Records:   records,
		SFLocator: sfResult.NextPageToken,
		TotalSize: sfResult.TotalSize,
	}, nil
}

// convertSOQLToProjectMembership converts a Salesforce Asset SOQL result to the
// domain ProjectMembership model. Account (company) and Product2 (tier) fields
// are denormalized directly onto the struct — no sub-objects are used.
func convertSOQLToProjectMembership(asset soqlAsset) (*model.ProjectMembership, error) {
	membershipUID, err := sfuuid.Normalize18(asset.ID)
	if err != nil {
		return nil, fmt.Errorf("normalizing asset SFID %q: %w", asset.ID, err)
	}

	tierUID, err := sfuuid.Normalize18(asset.Product2ID)
	if err != nil {
		return nil, fmt.Errorf("normalizing product2 SFID %q: %w", asset.Product2ID, err)
	}

	m := &model.ProjectMembership{
		UID:              membershipUID,
		TierUID:          tierUID,
		Status:           derefString(asset.Status),
		Year:             derefString(asset.Year),
		Tier:             derefString(asset.Tier),
		AutoRenew:        asset.AutoRenew,
		RenewalType:      derefString(asset.RenewalType),
		Price:            asset.Price,
		AnnualFullPrice:  asset.AnnualFullPrice,
		PaymentFrequency: derefString(asset.PaymentFrequency),
		PaymentTerms:     derefString(asset.PaymentTerms),
		AgreementDate:    parseSOQLDateTime(asset.AgreementDate),
		PurchaseDate:     resolvePurchaseDate(asset),
		StartDate:        parseSOQLDateTime(asset.InstallDate),
		EndDate:          parseSOQLDateTime(asset.UsageEndDate),
	}

	// AccountSFID is kept as an internal field for the write path (contact
	// association); it is never serialised to API responses.
	m.AccountSFID = asset.AccountID

	// B2BOrgUID is the 18-char SFID of the Salesforce Account.Id.
	// Populated here so callers can link this membership to the B2BOrg entity.
	// Errors are silently ignored because B2BOrgUID is a convenience field.
	if asset.AccountID != "" {
		if orgUID, orgErr := sfuuid.Normalize18(asset.AccountID); orgErr == nil {
			m.B2BOrgUID = orgUID
		}
	}

	// Denormalize Account (company) fields directly onto the membership.
	if asset.Account != nil {
		m.CompanyName = asset.Account.Name
		m.CompanyLogoURL = derefString(asset.Account.LogoURL)
		m.CompanyDomain = derefString(asset.Account.Website)
	}

	// Denormalize Product2 (tier) fields directly onto the membership.
	if asset.Product2 != nil {
		m.TierName = asset.Product2.Name
		m.TierFamily = derefString(asset.Product2.Family)
		m.TierProductType = derefString(asset.Product2.Type)
	}

	// Populate project fields from the Projects__r relationship.
	// ProjectUID is resolved from the slug via project-service (NATS); see
	// MemberReader.fetchMembershipFromSalesforce and backfill_runner for the
	// resolution step. ProjectSFID is the raw Salesforce Project__c.Id.
	if asset.Project != nil {
		m.ProjectSlug = derefString(asset.Project.Slug)
		if asset.Project.ID != "" {
			if projectSFID, normErr := sfuuid.Normalize18(asset.Project.ID); normErr == nil {
				m.ProjectSFID = projectSFID
			}
		}
	}

	// Timestamps.
	m.CreatedAt = parseSOQLTime(asset.CreatedDate)
	m.UpdatedAt = parseSOQLTime(asset.LastModifiedDate)

	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}

	return m, nil
}

// IterProjectMemberships iterates over all memberships from Salesforce, applying
// an optional LastModifiedDate filter when since is provided. Calls fn for each
// page of converted records. Conversion errors are logged and skipped.
func (r *MembershipRepo) IterProjectMemberships(ctx context.Context, since, until *time.Time, fn func([]*model.ProjectMembership) error) error {
	query := membershipSOQL + lastModifiedWindowClause(since, until)
	return IterPages(ctx, r.client, query, convertSOQLToProjectMembership, fn)
}

// resolvePurchaseDate mirrors the PostgreSQL COALESCE(PurchaseDate, InstallDate,
// CreatedDate) logic for determining the membership purchase date.
func resolvePurchaseDate(asset soqlAsset) string {
	if asset.PurchaseDate != nil && *asset.PurchaseDate != "" {
		return parseSOQLDateTime(asset.PurchaseDate)
	}
	if asset.InstallDate != nil && *asset.InstallDate != "" {
		return parseSOQLDateTime(asset.InstallDate)
	}
	return parseSOQLDateTime(&asset.CreatedDate)
}
