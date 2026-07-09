// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// ─── Cache key prefix constants ───────────────────────────────────────────────

// sObject cache key prefix constants for the member-service-cache bucket.
// Keys follow the pattern "{prefix}.{uid}" as required by the architecture.
const (
	sobjectKeyPrefixB2BOrgLegacy      = "b2b_org"
	sobjectKeyPrefixB2BOrg            = "b2b_org_v2"
	sobjectKeyPrefixB2BOrgFlat        = "b2b_org_flat"
	sobjectKeyPrefixB2BOrgParentBrief = "b2b_org_parent_brief"
	sobjectKeyPrefixProjectMembership = "project_membership"
	sobjectKeyPrefixKeyContact        = "key_contact"
	sobjectKeyPrefixMembershipTier    = "membership_tier"
	sobjectKeyPrefixContact           = "contact"
)

// sobjectCacheKey returns the "{prefix}.{uid}" cache key for the
// member-service-cache bucket.
func sobjectCacheKey(prefix, uid string) string {
	return prefix + "." + uid
}

// normalizeUID normalizes uid to its canonical 18-char SFID form, returning a
// validation error that names the Salesforce entity type on failure.
func normalizeUID(entityType, uid string) (string, error) {
	sfid, err := sfuuid.Normalize18(uid)
	if err != nil {
		return "", errs.NewValidation(fmt.Sprintf("invalid %s UID %q: %v", entityType, uid, err))
	}
	return sfid, nil
}

// ─── sObject REST API JSON types ──────────────────────────────────────────────

// These types mirror the Salesforce sObject REST API JSON field names. They are
// distinct from the soql* types (which use `salesforce:""` struct tags for the
// SOQL library) because the sObject REST endpoint returns standard JSON keys.

// sobjectAccountParent is the JSON shape of the parent Account sub-object.
// Populated by a secondary sObject REST fetch when Account.ParentId is set;
// sObject REST ?fields= cannot return relationship sub-fields directly.
type sobjectAccountParent struct {
	ID      string  `json:"Id"`
	Name    string  `json:"Name"`
	LogoURL *string `json:"Logo_URL__c"`
}

// sobjectAccount is the JSON shape of a Salesforce Account record from the
// sObject REST API. Used for B2BOrg (member company) lookups.
type sobjectAccount struct {
	ID                string                `json:"Id"`
	Name              string                `json:"Name"`
	LogoURL           *string               `json:"Logo_URL__c"`
	Website           *string               `json:"Website"`
	PrimaryDomain     *string               `json:"Account_Domain__c"`
	DomainAlias       *string               `json:"Domain_Alias__c"`
	Description       *string               `json:"Description"`
	Phone             *string               `json:"Phone"`
	ParentID          *string               `json:"ParentId"`
	Parent            *sobjectAccountParent `json:"-"` // populated by secondary fetch in FetchB2BOrg
	Industry          *string               `json:"Industry"`
	Sector            *string               `json:"Sector__c"`
	CrunchBaseURL     *string               `json:"CrunchBase_URL__c"`
	NumberOfEmployees *int64                `json:"NumberOfEmployees"`
	Status            *string               `json:"LF_Membership_Status__c"`
	IsMember          *bool                 `json:"IsMember__c"`
	Slug              *string               `json:"Slug__c"`
	CreatedDate       string                `json:"CreatedDate"`
	LastModifiedDate  string                `json:"LastModifiedDate"`
	SystemModstamp    string                `json:"SystemModstamp"`
}

// sobjectAsset is the JSON shape of a Salesforce Asset record from the sObject
// REST API. Used for ProjectMembership (membership / Asset) single-record reads.
type sobjectAsset struct {
	ID               string  `json:"Id"`
	Name             string  `json:"Name"`
	Status           *string `json:"Status"`
	AccountID        string  `json:"AccountId"`
	Product2ID       string  `json:"Product2Id"`
	Year             *string `json:"Year__c"`
	Tier             *string `json:"Tier__c"`
	RecordTypeID     *string `json:"RecordTypeId"`
	AutoRenew        bool    `json:"Auto_Renew__c"`
	RenewalType      *string `json:"Renewal_Type__c"`
	Price            float64 `json:"Price"`
	AnnualFullPrice  float64 `json:"Annual_Full_Price__c"`
	PaymentFrequency *string `json:"PaymentFrequency__c"`
	PaymentTerms     *string `json:"PaymentTerms__c"`
	AgreementDate    *string `json:"Agreement_Date__c"`
	PurchaseDate     *string `json:"PurchaseDate"`
	InstallDate      *string `json:"InstallDate"`
	UsageEndDate     *string `json:"UsageEndDate"`
	ProjectsID       *string `json:"Projects__c"`
	CreatedDate      string  `json:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate"`
	SystemModstamp   string  `json:"SystemModstamp"`
}

// sobjectProduct2 is the JSON shape of a Salesforce Product2 record from the
// sObject REST API. Used for MembershipTier single-record reads.
type sobjectProduct2 struct {
	ID               string  `json:"Id"`
	Name             string  `json:"Name"`
	Family           *string `json:"Family"`
	Type             *string `json:"Type__c"`
	ProjectID        *string `json:"Project__c"`
	CreatedDate      string  `json:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate"`
	SystemModstamp   string  `json:"SystemModstamp"`
}

// sobjectProjectRole is the JSON shape of a Salesforce Project_Role__c record
// from the sObject REST API. Used for KeyContact single-record reads.
type sobjectProjectRole struct {
	ID               string  `json:"Id"`
	AssetID          string  `json:"Asset__c"`
	ContactID        *string `json:"Contact__c"`
	Role             *string `json:"Role__c"`
	Status           *string `json:"Status__c"`
	BoardMember      bool    `json:"BoardMember__c"`
	PrimaryContact   bool    `json:"PrimaryContact__c"`
	CreatedDate      string  `json:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate"`
	SystemModstamp   string  `json:"SystemModstamp"`
}

// sobjectContact is the JSON shape of a Salesforce Contact record from the
// sObject REST API. Used to populate FirstName, LastName, Title, and Email on
// an assembled KeyContact.
type sobjectContact struct {
	ID               string  `json:"Id"`
	FirstName        *string `json:"FirstName"`
	LastName         *string `json:"LastName"`
	Title            *string `json:"Title"`
	Email            *string `json:"Email"`
	LastModifiedDate string  `json:"LastModifiedDate"`
}

// ─── sObject field lists ──────────────────────────────────────────────────────

// Field lists sent in the ?fields= query parameter for each sObject type.
// Using explicit field selection avoids fetching unneeded fields and keeps
// responses small.
const (
	accountFields = "Id,Name,Logo_URL__c,Website,CreatedDate,LastModifiedDate,SystemModstamp"

	// b2bOrgFields is the full field list for B2BOrg fetches via FetchB2BOrg. It
	// includes all fields required to populate model.B2BOrg.
	// TODO(LFXV2-1363): add Slug__c once the custom field is confirmed to exist in all
	// target SF orgs (absent from partial sandbox, causing 400 INVALID_FIELD on fetch).
	b2bOrgFields = "Id,Name,Logo_URL__c,Website,Account_Domain__c,Domain_Alias__c," +
		"Description,Phone,ParentId,Industry,Sector__c,CrunchBase_URL__c," +
		"NumberOfEmployees,LF_Membership_Status__c,IsMember__c," +
		"CreatedDate,LastModifiedDate,SystemModstamp"

	assetFields = "Id,Name,Status,AccountId,Product2Id,Year__c,Tier__c,RecordTypeId," +
		"Auto_Renew__c,Renewal_Type__c,Price,Annual_Full_Price__c,PaymentFrequency__c," +
		"PaymentTerms__c,Agreement_Date__c,PurchaseDate,InstallDate,UsageEndDate," +
		"Projects__c,CreatedDate,LastModifiedDate,SystemModstamp"

	product2Fields = "Id,Name,Family,Type__c,Project__c,CreatedDate,LastModifiedDate,SystemModstamp"

	projectRoleFields = "Id,Asset__c,Contact__c,Role__c,Status__c,BoardMember__c," +
		"PrimaryContact__c,CreatedDate,LastModifiedDate,SystemModstamp"

	contactFields = "Id,FirstName,LastName,Title,Email,LastModifiedDate"
)

// ─── FetchAccount ─────────────────────────────────────────────────────────────

// AccountRecord is a flat representation of a Salesforce Account record as
// returned by the sObject REST API. It carries only the fields used by the
// member service (name, logo, website). Relationship fields (e.g. Contacts,
// Assets) are not included — those require SOQL joins.
type AccountRecord struct {
	UID       string
	Name      string
	LogoURL   string
	Domain    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FetchAccount fetches a single Salesforce Account (B2BOrg) record by its UID.
// The SFID is derived from the UID; the cache key is "b2b_org_flat.{uid}",
// distinct from FetchB2BOrg's "b2b_org_v2.{uid}" since this fetch requests a
// different, narrower field list (accountFields) for the same Account.
//
// Because the Account sObject has no natural project association in the returned
// JSON (relationship fields require SOQL sub-selects), the returned AccountRecord
// carries only the flat Account fields.
func (c *SObjectClient) FetchAccount(ctx context.Context, uid string) (*AccountRecord, error) {
	sfid, err := normalizeUID("Account", uid)
	if err != nil {
		return nil, err
	}

	cacheKey := sobjectCacheKey(sobjectKeyPrefixB2BOrgFlat, sfid)
	result, err := c.FetchSObject(ctx, "Account", sfid, cacheKey, accountFields)
	if err != nil {
		return nil, err
	}

	var raw sobjectAccount
	if unmarshalErr := json.Unmarshal(result.Body, &raw); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal Account sObject response: %w", unmarshalErr)
	}

	return sobjectAccountToRecord(&raw, sfid), nil
}

// sobjectAccountToRecord converts a raw sobjectAccount to an AccountRecord.
func sobjectAccountToRecord(raw *sobjectAccount, uid string) *AccountRecord {
	return &AccountRecord{
		UID:       uid,
		Name:      raw.Name,
		LogoURL:   derefString(raw.LogoURL),
		Domain:    derefString(raw.Website),
		CreatedAt: parseSOQLTime(raw.CreatedDate),
		UpdatedAt: parseSOQLTime(raw.LastModifiedDate),
	}
}

// FetchB2BOrg fetches a single Salesforce Account (B2BOrg) record by its UID
// using the full b2bOrgFields field list. The returned model.B2BOrg is fully
// populated including industry, sector, domains, and status fields.
// The cache key is "b2b_org_v2.{uid}"; the versioned prefix ensures a deploy
// never trusts a legacy poisoned "b2b_org.{uid}" entry written before the
// field-list split in LFXV2-2654. The FetchResult carries ETag and Last-Modified
// for use by callers that need to set response headers.
func (c *SObjectClient) FetchB2BOrg(ctx context.Context, uid string) (*model.B2BOrg, *FetchResult, error) {
	sfid, err := normalizeUID("Account", uid)
	if err != nil {
		return nil, nil, err
	}

	cacheKey := sobjectCacheKey(sobjectKeyPrefixB2BOrg, sfid)
	result, err := c.FetchSObject(ctx, "Account", sfid, cacheKey, b2bOrgFields)
	if err != nil {
		return nil, nil, err
	}

	var raw sobjectAccount
	if unmarshalErr := json.Unmarshal(result.Body, &raw); unmarshalErr != nil {
		return nil, nil, fmt.Errorf("unmarshal Account sObject response: %w", unmarshalErr)
	}

	// Fetch parent Account details when ParentId is set. sObject REST ?fields=
	// cannot return relationship sub-fields, so a secondary fetch is required.
	if parentSFID := derefString(raw.ParentID); parentSFID != "" {
		parentDetail, fetchErr := c.fetchParentAccountDetail(ctx, parentSFID)
		if fetchErr != nil {
			slog.WarnContext(ctx, "failed to fetch parent account detail, proceeding without it",
				"uid", sfid, "parent_sfid", parentSFID, "error", fetchErr)
		} else {
			raw.Parent = parentDetail
		}
	}

	org, err := sobjectAccountToB2BOrg(ctx, &raw, sfid)
	if err != nil {
		return nil, nil, err
	}
	return org, result, nil
}

// fetchParentAccountDetail fetches a minimal set of fields (Id, Name, Logo_URL__c)
// for the parent Account identified by parentSFID. Failures are non-fatal to the
// caller; the parent detail is best-effort.
//
// Uses a distinct cache key prefix ("b2b_org_parent_brief") from FetchB2BOrg's
// "b2b_org_v2.{uid}" so that this narrow, 3-field lookup can never be read back as
// satisfying a full B2BOrg fetch for the same Account (see LFXV2-2654).
func (c *SObjectClient) fetchParentAccountDetail(ctx context.Context, parentSFID string) (*sobjectAccountParent, error) {
	parentUID, err := sfuuid.Normalize18(parentSFID)
	if err != nil {
		return nil, fmt.Errorf("invalid parent Account SFID %q: %w", parentSFID, err)
	}
	cacheKey := sobjectCacheKey(sobjectKeyPrefixB2BOrgParentBrief, parentUID)
	result, err := c.FetchSObject(ctx, "Account", parentSFID, cacheKey, "Id,Name,Logo_URL__c")
	if err != nil {
		return nil, err
	}
	var raw sobjectAccount
	if unmarshalErr := json.Unmarshal(result.Body, &raw); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal parent Account: %w", unmarshalErr)
	}
	return &sobjectAccountParent{
		ID:      raw.ID,
		Name:    raw.Name,
		LogoURL: raw.LogoURL,
	}, nil
}

// sobjectAccountToB2BOrg converts a raw sobjectAccount (from the sObject REST
// API) to a fully-populated model.B2BOrg. Domain normalization matches
// convertSOQLToB2BOrg so that both paths produce equivalent output.
func sobjectAccountToB2BOrg(ctx context.Context, raw *sobjectAccount, uid string) (*model.B2BOrg, error) {
	org := &model.B2BOrg{
		UID:     uid,
		SFID:    raw.ID,
		Name:    raw.Name,
		LogoURL: derefString(raw.LogoURL),
	}

	// Normalize Website.
	if rawSite := derefString(raw.Website); rawSite != "" {
		if u, parseErr := url.Parse(rawSite); parseErr == nil {
			if u.Scheme == "" {
				u.Scheme = "http"
			}
			org.Website = u.String()
		} else {
			slog.WarnContext(ctx, "account website could not be parsed, omitting",
				"uid", uid, "raw_value", rawSite)
		}
	}

	// Normalize primary domain.
	if rawDomain := derefString(raw.PrimaryDomain); rawDomain != "" {
		if normalized, ok := normalizeDomain(rawDomain); ok {
			org.PrimaryDomain = normalized
		} else {
			slog.WarnContext(ctx, "account primary domain invalid, omitting",
				"uid", uid, "raw_value", rawDomain)
		}
	}

	// Normalize domain aliases using the shared parser that also handles
	// account-merge CRLF artifacts.
	org.DomainAliases = parseDomainAliases(ctx, uid, derefString(raw.DomainAlias))

	org.Description = derefString(raw.Description)
	org.Phone = derefString(raw.Phone)
	org.Industry = derefString(raw.Industry)
	org.Sector = derefString(raw.Sector)
	org.CrunchBaseURL = raw.CrunchBaseURL
	org.NumberOfEmployees = raw.NumberOfEmployees
	org.Status = derefString(raw.Status)
	if raw.IsMember != nil {
		org.IsMember = *raw.IsMember
	}
	org.Slug = derefString(raw.Slug)

	if parentSFID := derefString(raw.ParentID); parentSFID != "" {
		parentUID, convErr := sfuuid.Normalize18(parentSFID)
		if convErr != nil {
			slog.WarnContext(ctx, "account parent SFID could not be normalized, omitting",
				"uid", uid, "parent_sfid", parentSFID)
		} else {
			org.ParentUID = parentUID
			if raw.Parent != nil {
				org.ParentDetail = &model.B2BOrgParentDetail{
					UID:     parentUID,
					Name:    raw.Parent.Name,
					LogoURL: raw.Parent.LogoURL,
				}
			}
		}
	}

	org.CreatedAt = parseSOQLTime(raw.CreatedDate)
	org.UpdatedAt = parseSOQLTime(raw.LastModifiedDate)
	if org.UpdatedAt.IsZero() {
		org.UpdatedAt = org.CreatedAt
	}
	if org.CreatedAt.IsZero() {
		org.CreatedAt = time.Now()
	}

	return org, nil
}

// ─── FetchAsset ───────────────────────────────────────────────────────────────

// FetchAsset fetches a single Salesforce Asset (ProjectMembership) record by
// its UID using the sObject REST API. The returned model carries only the flat
// Asset fields; relationship fields (Account name/logo, Product2 name, Project
// slug) are not populated — callers that need those should use the SOQL-backed
// MemberReader instead.
//
// This method is intended for cache-validation use cases where a caller already
// has the denormalized data and only needs to check whether the record has
// changed.
func (c *SObjectClient) FetchAsset(ctx context.Context, uid string) (*model.ProjectMembership, error) {
	sfid, err := normalizeUID("Asset", uid)
	if err != nil {
		return nil, err
	}

	cacheKey := sobjectCacheKey(sobjectKeyPrefixProjectMembership, sfid)
	result, err := c.FetchSObject(ctx, "Asset", sfid, cacheKey, assetFields)
	if err != nil {
		return nil, err
	}

	var raw sobjectAsset
	if unmarshalErr := json.Unmarshal(result.Body, &raw); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal Asset sObject response: %w", unmarshalErr)
	}

	return sobjectAssetToModel(&raw, sfid)
}

// sobjectAssetToModel converts a raw sobjectAsset to a minimal
// model.ProjectMembership. Relationship-sourced fields (CompanyName, TierName,
// ProjectUID, etc.) are left at their zero values; the caller is responsible
// for enriching the record if needed.
func sobjectAssetToModel(raw *sobjectAsset, uid string) (*model.ProjectMembership, error) {
	tierUID, err := sfuuid.Normalize18(raw.Product2ID)
	if err != nil && raw.Product2ID != "" {
		return nil, fmt.Errorf("normalize Asset.Product2Id %q: %w", raw.Product2ID, err)
	}

	purchaseDate := coalesceDate(raw.PurchaseDate, raw.InstallDate, &raw.CreatedDate)

	return &model.ProjectMembership{
		UID:              uid,
		TierUID:          tierUID,
		Status:           derefString(raw.Status),
		Year:             derefString(raw.Year),
		Tier:             derefString(raw.Tier),
		AutoRenew:        raw.AutoRenew,
		RenewalType:      derefString(raw.RenewalType),
		Price:            raw.Price,
		AnnualFullPrice:  raw.AnnualFullPrice,
		PaymentFrequency: derefString(raw.PaymentFrequency),
		PaymentTerms:     derefString(raw.PaymentTerms),
		AgreementDate:    parseSOQLDateTime(raw.AgreementDate),
		PurchaseDate:     parseSOQLDateTime(&purchaseDate),
		StartDate:        parseSOQLDateTime(raw.InstallDate),
		EndDate:          parseSOQLDateTime(raw.UsageEndDate),
		CreatedAt:        parseSOQLTime(raw.CreatedDate),
		UpdatedAt:        parseSOQLTime(raw.LastModifiedDate),
	}, nil
}

// coalesceDate returns the first non-nil, non-empty date string from the
// supplied pointers. Returns empty string when all are nil or empty.
func coalesceDate(candidates ...*string) string {
	for _, s := range candidates {
		if s != nil && *s != "" {
			return *s
		}
	}
	return ""
}

// ─── FetchProduct2 ────────────────────────────────────────────────────────────

// FetchProduct2 fetches a single Salesforce Product2 (MembershipTier) record by
// its UID using the sObject REST API. ProjectUID is not populated because the
// Product2 sObject has no project relationship field accessible without a SOQL
// join.
func (c *SObjectClient) FetchProduct2(ctx context.Context, uid string) (*model.MembershipTier, error) {
	sfid, err := normalizeUID("Product2", uid)
	if err != nil {
		return nil, err
	}

	cacheKey := sobjectCacheKey(sobjectKeyPrefixMembershipTier, sfid)
	result, err := c.FetchSObject(ctx, "Product2", sfid, cacheKey, product2Fields)
	if err != nil {
		return nil, err
	}

	var raw sobjectProduct2
	if unmarshalErr := json.Unmarshal(result.Body, &raw); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal Product2 sObject response: %w", unmarshalErr)
	}

	return sobjectProduct2ToModel(&raw, sfid), nil
}

// sobjectProduct2ToModel converts a raw sobjectProduct2 to a model.MembershipTier.
// ProjectUID is left empty; callers that need it should enrich via the resolver.
func sobjectProduct2ToModel(raw *sobjectProduct2, uid string) *model.MembershipTier {
	return &model.MembershipTier{
		UID:         uid,
		Name:        raw.Name,
		Family:      derefString(raw.Family),
		ProductType: derefString(raw.Type),
		CreatedAt:   parseSOQLTime(raw.CreatedDate),
		UpdatedAt:   parseSOQLTime(raw.LastModifiedDate),
	}
}

// ─── FetchProjectRole ─────────────────────────────────────────────────────────

// FetchProjectRole fetches a single Salesforce Project_Role__c (KeyContact)
// record by its UID using the sObject REST API. Contact and Account fields
// (name, email, company) are not populated because they require SOQL joins;
// the returned model carries only the Project_Role__c record's own fields.
func (c *SObjectClient) FetchProjectRole(ctx context.Context, uid string) (*model.KeyContact, error) {
	sfid, err := normalizeUID("Project_Role__c", uid)
	if err != nil {
		return nil, err
	}

	cacheKey := sobjectCacheKey(sobjectKeyPrefixKeyContact, sfid)
	result, err := c.FetchSObject(ctx, "Project_Role__c", sfid, cacheKey, projectRoleFields)
	if err != nil {
		return nil, err
	}

	var raw sobjectProjectRole
	if unmarshalErr := json.Unmarshal(result.Body, &raw); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal Project_Role__c sObject response: %w", unmarshalErr)
	}

	return sobjectProjectRoleToModel(&raw, sfid)
}

// sobjectProjectRoleToModel converts a raw sobjectProjectRole to a minimal
// model.KeyContact. Contact-sourced fields (FirstName, LastName, Email,
// Title, CompanyName, etc.) are left at their zero values.
func sobjectProjectRoleToModel(raw *sobjectProjectRole, uid string) (*model.KeyContact, error) {
	membershipUID, err := sfuuid.Normalize18(raw.AssetID)
	if err != nil && raw.AssetID != "" {
		return nil, fmt.Errorf("normalize Project_Role__c.Asset__c %q: %w", raw.AssetID, err)
	}

	return &model.KeyContact{
		UID:            uid,
		MembershipUID:  membershipUID,
		Role:           derefString(raw.Role),
		Status:         derefString(raw.Status),
		BoardMember:    raw.BoardMember,
		PrimaryContact: raw.PrimaryContact,
		CreatedAt:      parseSOQLTime(raw.CreatedDate),
		UpdatedAt:      parseSOQLTime(raw.LastModifiedDate),
	}, nil
}
