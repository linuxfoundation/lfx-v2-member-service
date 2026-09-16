// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"fmt"
	"time"
)

// B2BOrg represents a B2B organization in the LFX v2 domain. It is the
// canonical entity for a member company.
type B2BOrg struct {
	// UID is the canonical 18-char Salesforce Account SFID for this organization.
	UID string `json:"uid"`

	// SFID is the raw Salesforce Account.Id. It is kept internal (not
	// serialized) so it is not exposed in API responses.
	SFID string `json:"-"`

	// Name is the organization's display name.
	Name string `json:"name"`

	// Description is the organization's free-text description (Account.Description).
	Description string `json:"description,omitempty"`

	// Phone is the organization's contact phone number (Account.Phone).
	Phone string `json:"phone,omitempty"`

	// Website is the organization's website URL (Account.Website). Always has a
	// scheme (http or https). Omitted when empty or unparseable.
	// NOTE: the v3 member-service exposes this same field as "accountLink".
	Website string `json:"website,omitempty"`

	// PrimaryDomain is the normalized primary domain for the organization
	// (Account.Account_Domain__c). Expected to be a bare host such as
	// "example.com"; values that do not parse as a valid domain are omitted.
	PrimaryDomain string `json:"primary_domain,omitempty"`

	// DomainAliases is the list of additional normalized domains for the
	// organization (Account.Domain_Alias__c). Each item is normalized with the
	// same rules as PrimaryDomain; invalid items are dropped.
	DomainAliases []string `json:"domain_aliases,omitempty"`

	// LogoURL is the URL of the organization's logo image (Account.Logo_URL__c).
	LogoURL string `json:"logo_url,omitempty"`

	// Industry is the organization's industry classification (Account.Industry,
	// standard Salesforce field).
	Industry string `json:"industry,omitempty"`

	// Sector is the organization's sector classification (Account.Sector__c,
	// custom Salesforce field).
	Sector string `json:"sector,omitempty"`

	// CrunchBaseURL is the organization's CrunchBase profile URL
	// (Account.CrunchBase_URL__c, custom Salesforce field). Nil means not set;
	// empty string means explicitly cleared.
	CrunchBaseURL *string `json:"crunch_base_url,omitempty"`

	// NumberOfEmployees is the organization's employee count
	// (Account.NumberOfEmployees, standard Salesforce Integer field). Nil means
	// not set.
	NumberOfEmployees *int64 `json:"number_of_employees,omitempty"`

	// Status is the LF membership status (Account.LF_Membership_Status__c,
	// custom Salesforce field). Read-only; managed by Salesforce workflows.
	Status string `json:"status,omitempty"`

	// IsMember indicates whether the organization is currently an LF member
	// (Account.IsMember__c, custom Salesforce field). Read-only; managed by
	// Salesforce workflows.
	IsMember bool `json:"is_member"`

	// Slug is the URL-friendly identifier for the organization.
	// The Heroku Connect replica column is "slug" (SF API name Slug__c).
	// TODO: confirm field exists in the Salesforce org schema before exposing.
	Slug string `json:"slug,omitempty"`

	// ParentUID is the canonical 18-char Salesforce Account SFID of the parent
	// organization, derived from Account.ParentId. Omitted when the organization has no parent.
	ParentUID string `json:"parent_uid,omitempty"`

	// ParentDetail carries the parent organization's name and logo for
	// denormalized display. Populated when the fetch path includes the
	// Salesforce Account.Parent relationship sub-object. Omitted when the
	// organization has no parent or the parent could not be resolved.
	ParentDetail *B2BOrgParentDetail `json:"parent_detail,omitempty"`

	// IsParent is true when this org has at least one direct member-eligible child.
	// Omitted (false) when the org is a leaf. Consumers can find children by
	// querying the index for parent_uid = this org's UID.
	IsParent bool `json:"is_parent,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// B2BOrgParentDetail carries denormalized parent organization details embedded
// in the indexed document so callers can render parent info without a second lookup.
type B2BOrgParentDetail struct {
	// UID is the canonical 18-char Salesforce Account SFID of the parent organization.
	UID string `json:"uid"`
	// Name is the parent organization's display name.
	Name string `json:"name"`
	// LogoURL is the parent organization's logo URL (Account.Logo_URL__c).
	LogoURL *string `json:"logo_url,omitempty"`
}

// Tags returns the search tags for this organization. The indexer uses these
// to make the record discoverable by UID and by parent relationship.
// Pattern: bare UID + prefixed b2b_org_uid:<uid> + parent ref if set.
func (o *B2BOrg) Tags() []string {
	if o == nil {
		return nil
	}
	var tags []string
	if o.UID != "" {
		tags = append(tags, o.UID)
		tags = append(tags, fmt.Sprintf("b2b_org_uid:%s", o.UID))
	}
	if o.ParentUID != "" {
		tags = append(tags, fmt.Sprintf("parent_b2b_org_uid:%s", o.ParentUID))
	}
	tags = append(tags, fmt.Sprintf("is_member:%v", o.IsMember))
	return tags
}

// B2BOrgInput carries the mutable fields for creating or updating a B2BOrg
// record. All fields are optional on update; a zero value means "leave
// unchanged". LogoURL and CrunchBaseURL use *string so that nil = "don't
// touch" and empty string = "explicitly clear the field".
type B2BOrgInput struct {
	// Name is the organization's display name (Account.Name).
	Name string

	// Description is the organization's free-text description (Account.Description).
	Description string

	// Phone is the organization's contact phone number (Account.Phone).
	Phone string

	// Website is the organization's website URL (Account.Website).
	Website string

	// PrimaryDomain is the canonical primary domain (Account.Account_Domain__c).
	PrimaryDomain string

	// LogoURL is the URL of the organization's logo image (Account.Logo_URL__c).
	// Nil = don't change; empty string = explicitly clear.
	LogoURL *string

	// Industry is the organization's industry classification (Account.Industry).
	Industry string

	// Sector is the organization's sector classification (Account.Sector__c).
	Sector string

	// CrunchBaseURL is the CrunchBase profile URL (Account.CrunchBase_URL__c).
	// Nil = don't change; empty string = explicitly clear.
	CrunchBaseURL *string

	// NumberOfEmployees is the employee count (Account.NumberOfEmployees).
	// Nil = don't change.
	NumberOfEmployees *int64

	// IfUnmodifiedSince is the SF LastModifiedDate forwarded as If-Unmodified-Since
	// to the Salesforce PATCH endpoint for server-side concurrency protection.
	// ETag (If-Match) validation is performed in the service layer before this is set.
	IfUnmodifiedSince string
}

// HasChanges reports whether any mutable field is set. IfUnmodifiedSince is an
// infrastructure concern and does not count as a change.
func (i B2BOrgInput) HasChanges() bool {
	return i.Name != "" || i.Description != "" || i.Phone != "" ||
		i.Website != "" || i.PrimaryDomain != "" || i.LogoURL != nil ||
		i.Industry != "" || i.Sector != "" || i.CrunchBaseURL != nil ||
		i.NumberOfEmployees != nil
}
