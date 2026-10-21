// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"fmt"
	"strings"
	"time"
)

// KeyContactInput carries the mutable fields for creating or updating a
// Project_Role__c key contact record in Salesforce. All pointer fields are
// optional on update; a nil pointer means "leave unchanged".
type KeyContactInput struct {
	// Email is the contact's email address. Required on create; used to
	// resolve or create the B2B Salesforce Contact record. On update, nil
	// means "leave unchanged".
	Email *string

	// FirstName is the contact's first name. Required on create; used when
	// creating a new Contact on miss.
	FirstName string

	// LastName is the contact's last name. Required on create; used when
	// creating a new Contact on miss.
	LastName string

	// Title is the contact's job title. Optional; used when creating a new
	// Contact on miss.
	Title string

	// MembershipUID is the UID of the parent membership (Asset). Required on
	// create; used for cache invalidation on update/delete.
	MembershipUID string

	// ProjectUID is the v2 UUID of the project this key contact belongs to.
	// When non-empty it is stamped onto the returned contact record so that
	// callers receive a fully-populated project_uid without an additional
	// resolver round-trip.
	ProjectUID string

	// AccountSFID holds the canonical 18-char Salesforce Account.Id (B2BOrgUID from the
	// service layer, which is already the SFID). Passed directly to ResolveOrCreateContact.
	// Optional; only needed when a new Salesforce Contact may be created (i.e. the
	// email resolves to an unknown address).
	AccountSFID string

	// Role is the contact's role designation, e.g. "Voting Representative".
	Role *string

	// Status is the role record status, e.g. "Active".
	Status *string

	// BoardMember indicates whether this contact holds a board member role.
	BoardMember *bool

	// PrimaryContact indicates whether this is the primary contact for the
	// membership.
	PrimaryContact *bool

	// IfUnmodifiedSince is the SF LastModifiedDate forwarded as If-Unmodified-Since
	// to the Salesforce PATCH endpoint for server-side concurrency protection.
	// Set by the service layer after ETag validation; never supplied directly by API callers.
	IfUnmodifiedSince string
}

// KeyContact represents a key contact (Project_Role__c) for a specific
// membership within a project. Contact and company attributes are denormalized
// directly onto this struct — there are no separate Contact or Organization
// sub-objects. This avoids any dependency on the User Service or Org Service.
type KeyContact struct {
	// UID is the canonical 18-char Salesforce Project_Role__c SFID.
	UID string `json:"uid"`

	// MembershipUID is the UID of the associated ProjectMembership (Asset).
	MembershipUID string `json:"membership_uid"`

	// TierUID is the UID of the associated MembershipTier (Product2).
	TierUID string `json:"tier_uid"`

	// ProjectUID is the v2 UUID of the project this key contact belongs to.
	// Resolved from the project slug via project-service over NATS.
	ProjectUID string `json:"project_uid"`

	// ProjectSFID is the 18-char Salesforce Project__c.Id for this key contact's
	// project. Populated directly from the SFDC record. Exposed in API responses
	// and indexer docs so Salesforce-keyed consumers can correlate the project.
	ProjectSFID string `json:"project_sfid,omitempty"`

	// ProjectSlug is the URL slug of the associated project. Used internally
	// by the resolver to populate ProjectUID; not included in API responses.
	ProjectSlug string `json:"-"`

	// ProjectName is the display name of the associated project, embedded so
	// the indexer doc is self-sufficient without a second hop.
	ProjectName string `json:"project_name,omitempty"`

	// ProjectLogoURL is the logo image URL for the associated project.
	ProjectLogoURL string `json:"project_logo_url,omitempty"`

	// B2BOrgUID is the canonical 18-char Salesforce Account SFID of the
	// membership's company. Populated from AccountId on the parent Asset's
	// Account relationship.
	B2BOrgUID string `json:"b2b_org_uid,omitempty"`

	// Role is the contact's role designation, e.g. "Voting Representative".
	Role string `json:"role"`

	// Status is the role record status, e.g. "Active".
	Status string `json:"status"`

	// BoardMember indicates whether this contact holds a board member role.
	BoardMember bool `json:"board_member"`

	// PrimaryContact indicates whether this is the primary contact for the
	// membership.
	PrimaryContact bool `json:"primary_contact"`

	// FirstName is the contact's first name, denormalized from Contact.
	FirstName string `json:"first_name"`

	// LastName is the contact's last name, denormalized from Contact.
	LastName string `json:"last_name"`

	// Title is the contact's job title, denormalized from Contact.
	Title string `json:"title,omitempty"`

	// Email is the contact's primary email address, resolved from
	// Alternate_Email__c where Primary_Email__c = true. Used for email-based
	// lookups (e.g. MCP). No User Service reference is made.
	Email string `json:"email,omitempty"`

	// Username is the resolved LFID username for this contact's email.
	// Salesforce does not store it: every publish path (API write, CDC upsert,
	// /admin/reindex) resolves it from Email via auth-service. Empty when the
	// email has no registered LFID, or when the lookup failed; on a failed
	// lookup the CDC and reindex paths queue a key_contact cdc-repair marker
	// so a cdc_repair drain retries it.
	Username string `json:"username,omitempty"`

	// Emails is the full list of email addresses for this contact (primary +
	// alternates). Used by the indexer ContactBody for search.
	Emails []string `json:"emails,omitempty"`

	// CompanyName is the member company name, denormalized from the Account
	// associated with the membership Asset. No Org Service reference is made.
	CompanyName string `json:"company_name"`

	// CompanyLogoURL is the member company logo URL, denormalized from Account.
	CompanyLogoURL string `json:"company_logo_url,omitempty"`

	// CompanyDomain is the member company website/domain, denormalized from
	// Account.Website. Used for domain-based lookups (e.g. MCP).
	CompanyDomain string `json:"company_domain,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Name returns the contact's full name for use in indexer ContactBody.
func (kc *KeyContact) Name() string {
	return strings.TrimSpace(kc.FirstName + " " + kc.LastName)
}

// Tags returns search tags for this key contact. The indexer uses these to make
// the record discoverable by UID and by parent relationships.
func (kc *KeyContact) Tags() []string {
	if kc == nil {
		return nil
	}
	var tags []string
	if kc.UID != "" {
		tags = append(tags, kc.UID)
		tags = append(tags, fmt.Sprintf("key_contact_uid:%s", kc.UID))
	}
	if kc.MembershipUID != "" {
		tags = append(tags, fmt.Sprintf("project_membership_uid:%s", kc.MembershipUID))
	}
	if kc.ProjectUID != "" {
		tags = append(tags, fmt.Sprintf("project_uid:%s", kc.ProjectUID))
	}
	if kc.ProjectSFID != "" {
		tags = append(tags, fmt.Sprintf("project_sfid:%s", kc.ProjectSFID))
	}
	if kc.B2BOrgUID != "" {
		tags = append(tags, fmt.Sprintf("b2b_org_uid:%s", kc.B2BOrgUID))
	}
	if kc.Role != "" {
		tags = append(tags, fmt.Sprintf("role:%s", kc.Role))
	}
	if kc.Status != "" {
		tags = append(tags, fmt.Sprintf("status:%s", kc.Status))
	}
	return tags
}
