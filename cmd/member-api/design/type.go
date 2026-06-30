// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"goa.design/goa/v3/dsl"
)

// ── Response types ────────────────────────────────────────────────────────────

// MembershipTierResponse is the DSL type for a membership tier (Product2) response.
var MembershipTierResponse = dsl.Type("membership-tier-response", func() {
	dsl.Description("A membership tier (Product2) scoped to a project")
	dsl.Attribute("uid", dsl.String, "Tier UID (Salesforce Product2.Id)", func() {
		dsl.Example("01t2M000009ABCdIAM")
	})
	dsl.Attribute("project_uid", dsl.String, "V2 project UUID", func() {
		dsl.Format(dsl.FormatUUID)
		dsl.Example("a27394a3-7a6c-4d0f-9e0f-692d8753924f")
	})
	dsl.Attribute("name", dsl.String, "Product name, e.g. 'Gold Corporate Membership'", func() {
		dsl.Example("Gold Corporate Membership")
	})
	dsl.Attribute("family", dsl.String, "Product family, e.g. 'Membership'", func() {
		dsl.Example("Membership")
	})
	dsl.Attribute("product_type", dsl.String, "Product type (Type__c)", func() {
		dsl.Example("Corporate")
	})
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
})

// ProjectMembershipResponse is the DSL type for a project membership (Asset) response.
// Account (company) attributes are denormalized directly onto this type.
var ProjectMembershipResponse = dsl.Type("project-membership-response", func() {
	dsl.Description("A membership (Asset) scoped to a project, with denormalized company attributes")
	dsl.Attribute("uid", dsl.String, "Membership UID (Salesforce Asset.Id)", func() {
		dsl.Example("02i2M000009ABCdIAM")
	})
	dsl.Attribute("tier_uid", dsl.String, "UID of the associated membership tier (Product2)", func() {
		dsl.Example("01t2M000009ABCdIAM")
	})
	dsl.Attribute("project_uid", dsl.String, "V2 project UUID resolved from the project slug via project-service", func() {
		dsl.Format(dsl.FormatUUID)
		dsl.Example("a27394a3-7a6c-4d0f-9e0f-692d8753924f")
	})
	dsl.Attribute("project_sfid", dsl.String, "Salesforce Project__c.Id for the project this membership belongs to", func() {
		dsl.Example("a0941000002wBz9AAE")
	})
	dsl.Attribute("project_slug", dsl.String, "URL slug of the project this membership belongs to", func() {
		dsl.Example("kubernetes")
	})
	dsl.Attribute("b2b_org_uid", dsl.String, "UID of the B2B organization (Account) this membership belongs to", func() {
		dsl.Example("001B000000IqhSLIAZ")
	})
	dsl.Attribute("status", dsl.String, "Membership status", func() {
		dsl.Example("Active")
	})
	dsl.Attribute("year", dsl.String, "Membership year", func() {
		dsl.Example("2025")
	})
	dsl.Attribute("tier", dsl.String, "Membership tier label", func() {
		dsl.Example("Gold")
	})
	dsl.Attribute("auto_renew", dsl.Boolean, "Whether automatic renewal is enabled", func() {
		dsl.Example(true)
	})
	dsl.Attribute("renewal_type", dsl.String, "Renewal cadence", func() {
		dsl.Example("Annual")
	})
	dsl.Attribute("price", dsl.Float64, "Current membership price", func() {
		dsl.Example(50000.0)
	})
	dsl.Attribute("annual_full_price", dsl.Float64, "Full annual list price before discounts", func() {
		dsl.Example(50000.0)
	})
	dsl.Attribute("payment_frequency", dsl.String, "Payment frequency", func() {
		dsl.Example("Annual")
	})
	dsl.Attribute("payment_terms", dsl.String, "Payment terms", func() {
		dsl.Example("Net 30")
	})
	dsl.Attribute("agreement_date", dsl.String, "Date the membership agreement was signed", func() {
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("purchase_date", dsl.String, "Effective purchase date", func() {
		dsl.Example("2025-01-15T00:00:00Z")
	})
	dsl.Attribute("start_date", dsl.String, "Membership start date", func() {
		dsl.Example("2025-02-01T00:00:00Z")
	})
	dsl.Attribute("end_date", dsl.String, "Membership end date", func() {
		dsl.Example("2025-12-31T23:59:59Z")
	})
	// Denormalized company (Account) fields.
	dsl.Attribute("company_name", dsl.String, "Member company name (denormalized from Account)", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("company_logo_url", dsl.String, "Member company logo URL (denormalized from Account)", func() {
		dsl.Example("https://example.com/logo.png")
	})
	dsl.Attribute("company_domain", dsl.String, "Member company website/domain (denormalized from Account.Website)", func() {
		dsl.Example("https://example.com")
	})
	// Denormalized tier (Product2) fields.
	dsl.Attribute("tier_name", dsl.String, "Product name (denormalized from Product2)", func() {
		dsl.Example("Gold Corporate Membership")
	})
	dsl.Attribute("tier_family", dsl.String, "Product family (denormalized from Product2)", func() {
		dsl.Example("Membership")
	})
	dsl.Attribute("tier_product_type", dsl.String, "Product type (denormalized from Product2)", func() {
		dsl.Example("Corporate")
	})
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
})

// ProjectKeyContactResponse is the DSL type for a key contact (Project_Role__c) response.
// Contact and company attributes are denormalized directly onto this type — no
// sub-objects, no User Service or Org Service references.
var ProjectKeyContactResponse = dsl.Type("project-key-contact-response", func() {
	dsl.Description("A key contact (Project_Role__c) scoped to a membership, with denormalized contact and company attributes")
	dsl.Attribute("uid", dsl.String, "Key contact UID (Salesforce Project_Role__c.Id)", func() {
		dsl.Example("a0K2M000000ABCdUAG")
	})
	dsl.Attribute("membership_uid", dsl.String, "UID of the associated membership (Asset)", func() {
		dsl.Example("02i2M000009ABCdIAM")
	})
	dsl.Attribute("tier_uid", dsl.String, "UID of the associated membership tier (Product2)", func() {
		dsl.Example("01t2M000009ABCdIAM")
	})
	dsl.Attribute("project_uid", dsl.String, "V2 project UUID resolved from the project slug via project-service", func() {
		dsl.Format(dsl.FormatUUID)
		dsl.Example("a27394a3-7a6c-4d0f-9e0f-692d8753924f")
	})
	dsl.Attribute("project_sfid", dsl.String, "Salesforce Project__c.Id for the project this key contact belongs to", func() {
		dsl.Example("a0941000002wBz9AAE")
	})
	dsl.Attribute("b2b_org_uid", dsl.String, "UID of the B2B organization (Account) this key contact's membership belongs to", func() {
		dsl.Example("001B000000IqhSLIAZ")
	})
	dsl.Attribute("role", dsl.String, "Contact role designation", func() {
		dsl.Example("Technical Contact")
	})
	dsl.Attribute("status", dsl.String, "Role record status", func() {
		dsl.Example("Active")
	})
	dsl.Attribute("board_member", dsl.Boolean, "Whether this contact holds a board member role", func() {
		dsl.Example(false)
	})
	dsl.Attribute("primary_contact", dsl.Boolean, "Whether this is the primary contact for the membership", func() {
		dsl.Example(true)
	})
	// Denormalized Contact fields.
	dsl.Attribute("first_name", dsl.String, "Contact first name (denormalized from Contact)", func() {
		dsl.Example("John")
	})
	dsl.Attribute("last_name", dsl.String, "Contact last name (denormalized from Contact)", func() {
		dsl.Example("Doe")
	})
	dsl.Attribute("title", dsl.String, "Contact job title (denormalized from Contact)", func() {
		dsl.Example("CTO")
	})
	dsl.Attribute("email", dsl.String, "Primary email address from Alternate_Email__c where Primary_Email__c = true", func() {
		dsl.Example("john.doe@example.com")
	})
	// Denormalized company (Account via Asset) fields.
	dsl.Attribute("company_name", dsl.String, "Member company name (denormalized from Asset.Account)", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("company_logo_url", dsl.String, "Member company logo URL (denormalized from Asset.Account)", func() {
		dsl.Example("https://example.com/logo.png")
	})
	dsl.Attribute("company_domain", dsl.String, "Member company website/domain (denormalized from Asset.Account.Website)", func() {
		dsl.Example("https://example.com")
	})
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
})

// ListMetadata is the DSL type for list pagination metadata.
var ListMetadata = dsl.Type("list-metadata", func() {
	dsl.Description("Pagination metadata for list responses")
	dsl.Attribute("total_size", dsl.Int, "Total number of records matching the query. Set on the first page; may be 0 on continuation pages.", func() {
		dsl.Example(100)
	})
	dsl.Attribute("next_page_token", dsl.String, "Opaque cursor for the next page. Pass this value as the page_token query parameter to retrieve the next page. Empty or absent when this is the last page.", func() {
		dsl.Example("")
	})
})

// ── Payload attribute helpers ─────────────────────────────────────────────────

// BearerTokenAttribute is the DSL attribute for the JWT bearer token.
func BearerTokenAttribute() {
	dsl.Token("bearer_token", dsl.String, func() {
		dsl.Description("JWT token issued by Heimdall")
		dsl.Example("eyJhbGci...")
	})
}

// VersionAttribute is the DSL attribute for the API version query parameter.
func VersionAttribute() {
	dsl.Attribute("version", dsl.String, "Version of the API", func() {
		dsl.Example("1")
		dsl.Enum("1")
	})
}

// ETagAttribute is the DSL attribute for the ETag response header.
func ETagAttribute() {
	dsl.Attribute("etag", dsl.String, "ETag header value", func() {
		dsl.Example("123")
	})
}

// IfMatchAttribute is the DSL attribute for the If-Match request header (conditional request).
func IfMatchAttribute() {
	dsl.Attribute("if_match", dsl.String, "If-Match header value for conditional requests", func() {
		dsl.Example("123")
	})
}

// IfNoneMatchAttribute is the DSL attribute for the If-None-Match request header (conditional request).
func IfNoneMatchAttribute() {
	dsl.Attribute("if_none_match", dsl.String, "If-None-Match header value for conditional requests", func() {
		dsl.Example("123")
	})
}

// IfModifiedSinceAttribute is the DSL attribute for the If-Modified-Since request header (conditional request).
func IfModifiedSinceAttribute() {
	dsl.Attribute("if_modified_since", dsl.String, "If-Modified-Since header value for conditional requests (HTTP date format)", func() {
		dsl.Example("Wed, 21 Oct 2025 07:28:00 GMT")
	})
}

// IfUnmodifiedSinceAttribute is the DSL attribute for the If-Unmodified-Since request header (conditional request).
func IfUnmodifiedSinceAttribute() {
	dsl.Attribute("if_unmodified_since", dsl.String, "If-Unmodified-Since header value for conditional requests (HTTP date format)", func() {
		dsl.Example("Wed, 21 Oct 2025 07:28:00 GMT")
	})
}

// LastModifiedAttribute is the DSL attribute for the Last-Modified response header.
func LastModifiedAttribute() {
	dsl.Attribute("last_modified", dsl.String, "Last-Modified header value (HTTP date format)", func() {
		dsl.Example("Wed, 21 Oct 2025 07:28:00 GMT")
	})
}

// B2BOrgUIDAttribute adds the b2b_org_uid path parameter attribute.
func B2BOrgUIDAttribute() {
	dsl.Attribute("b2b_org_uid", dsl.String, "B2BOrg UID", func() {
		dsl.Example("001B000000IqhSLIAZ")
	})
}

// B2BOrgResponse is the DSL type for a B2B organization response.
var B2BOrgResponse = dsl.Type("b2b-org-response", func() {
	dsl.Description("A B2B organization")
	dsl.Attribute("uid", dsl.String, "B2BOrg UID (Salesforce Account.Id)", func() {
		dsl.Example("001B000000IqhSLIAZ")
	})
	dsl.Attribute("name", dsl.String, "Organization name", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("description", dsl.String, "Organization free-text description (Account.Description)", func() {
		dsl.Example("A leading technology company")
	})
	dsl.Attribute("phone", dsl.String, "Organization contact phone number (Account.Phone)", func() {
		dsl.Example("+1-555-000-0000")
	})
	dsl.Attribute("website", dsl.String, "Organization website URL; always has a scheme (http or https)", func() {
		dsl.Format(dsl.FormatURI)
		dsl.Example("https://example.com")
	})
	dsl.Attribute("primary_domain", dsl.String, "Primary domain; bare host only, no scheme or path, e.g. 'example.com'", func() {
		dsl.Example("example.com")
	})
	dsl.Attribute("domain_aliases", dsl.ArrayOf(dsl.String), "Additional domains; each item is a bare host with the same normalization as primary_domain", func() {
		dsl.Example([]string{"example.org", "example.net"})
	})
	dsl.Attribute("logo_url", dsl.String, "URL of the organization logo (Account.Logo_URL__c)", func() {
		dsl.Example("https://example.com/logo.png")
	})
	dsl.Attribute("industry", dsl.String, "Industry classification (Account.Industry, standard Salesforce field)", func() {
		dsl.Example("Technology")
	})
	dsl.Attribute("sector", dsl.String, "Sector classification (Account.Sector__c, custom Salesforce field)", func() {
		dsl.Example("Software")
	})
	dsl.Attribute("crunch_base_url", dsl.String, "CrunchBase profile URL (Account.CrunchBase_URL__c)", func() {
		dsl.Example("https://www.crunchbase.com/organization/example-corp")
	})
	dsl.Attribute("number_of_employees", dsl.Int, "Employee count (Account.NumberOfEmployees)", func() {
		dsl.Example(500)
	})
	dsl.Attribute("status", dsl.String, "LF membership status (Account.LF_Membership_Status__c); read-only, managed by Salesforce workflows", func() {
		dsl.Example("Active")
	})
	dsl.Attribute("is_member", dsl.Boolean, "Whether the organization is currently an LF member (Account.IsMember__c); read-only, managed by Salesforce workflows", func() {
		dsl.Example(true)
	})
	// TODO: slug is reserved for Account.Slug__c once the field is confirmed to exist in the SF org schema.
	dsl.Attribute("slug", dsl.String, "URL-friendly organization identifier; populated when Account.Slug__c is available", func() {
		dsl.Example("example-corp")
	})
	dsl.Attribute("parent_uid", dsl.String, "UID of the parent organization (Account.ParentId); omitted when no parent", func() {
		dsl.Example("001B000000IqhSLIAZ")
	})
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
})

// B2BOrgCreateBody is the DSL type for creating a B2B organization.
// POST /b2b_orgs registers an Account that EasyCLA / LFX Enrollment has already
// created in Salesforce; it does not create a new Account. Idempotent on sfid.
var B2BOrgCreateBody = dsl.Type("b2b-org-create-body", func() {
	dsl.Description("Request body for registering a B2B organization by its Salesforce Account ID")
	dsl.Attribute("sfid", dsl.String, "Salesforce Account.Id (15- or 18-character); used to fetch and cache the org record", func() {
		dsl.Example("001Hs00001AbCdEFAZ")
		dsl.MinLength(15)
		dsl.MaxLength(18)
	})
	dsl.Required("sfid")
})

// B2BOrgUpdateBody is the DSL type for updating a B2B organization.
var B2BOrgUpdateBody = dsl.Type("b2b-org-update-body", func() {
	dsl.Description("Request body for updating mutable fields on a B2B organization")
	dsl.Attribute("name", dsl.String, "Organization name", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("description", dsl.String, "Organization free-text description", func() {
		dsl.Example("A leading technology company")
	})
	dsl.Attribute("phone", dsl.String, "Organization contact phone number", func() {
		dsl.Example("+1-555-000-0000")
	})
	dsl.Attribute("website", dsl.String, "Organization website URL", func() {
		dsl.Example("https://example.com")
	})
	dsl.Attribute("primary_domain", dsl.String, "Primary domain (bare host)", func() {
		dsl.Example("example.com")
	})
	dsl.Attribute("logo_url", dsl.String, "URL of the organization logo (Account.Logo_URL__c)", func() {
		dsl.Example("https://example.com/logo.png")
	})
	dsl.Attribute("industry", dsl.String, "Industry classification (Account.Industry)", func() {
		dsl.Example("Technology")
	})
	dsl.Attribute("sector", dsl.String, "Sector classification (Account.Sector__c)", func() {
		dsl.Example("Software")
	})
	dsl.Attribute("crunch_base_url", dsl.String, "CrunchBase profile URL (Account.CrunchBase_URL__c); pass empty string to explicitly clear", func() {
		dsl.Example("https://www.crunchbase.com/organization/example-corp")
	})
	dsl.Attribute("number_of_employees", dsl.Int, "Employee count (Account.NumberOfEmployees)", func() {
		dsl.Example(500)
	})
})

// KeyContactCreateBody is the DSL type for creating a key contact.
// b2b_org_uid and project_uid are intentionally omitted — they are derived
// from the membership_uid path parameter so callers cannot supply inconsistent values.
var KeyContactCreateBody = dsl.Type("key-contact-create-body", func() {
	dsl.Description("Request body for creating a key contact (Project_Role__c record)")
	dsl.Attribute("email", dsl.String, "Contact email address; used to resolve or create the Salesforce Contact record", func() {
		dsl.Format(dsl.FormatEmail)
		dsl.Example("john.doe@example.com")
	})
	dsl.Attribute("first_name", dsl.String, "Contact first name; used when creating a new Contact on miss", func() {
		dsl.Example("John")
	})
	dsl.Attribute("last_name", dsl.String, "Contact last name; used when creating a new Contact on miss", func() {
		dsl.Example("Doe")
	})
	dsl.Attribute("title", dsl.String, "Contact job title. Only persisted when a new Salesforce Contact is created (email resolves to an unknown address); ignored if the Contact already exists.", func() {
		dsl.Example("CTO")
	})
	dsl.Attribute("role", dsl.String, "Contact role designation", func() {
		dsl.Example("Technical Contact")
		dsl.Enum(constants.KeyContactRoles...)
	})
	dsl.Attribute("status", dsl.String, "Role record status, e.g. 'Active'", func() {
		dsl.Example("Active")
		dsl.Enum(constants.KeyContactStatuses...)
	})
	dsl.Attribute("board_member", dsl.Boolean, "Whether this contact holds a board member role", func() {
		dsl.Example(false)
	})
	dsl.Attribute("primary_contact", dsl.Boolean, "Whether this is the primary contact for the membership", func() {
		dsl.Example(false)
	})
	dsl.Attribute("send_invite", dsl.Boolean, "When true, send a platform invite (unregistered user) or role-assignment email (registered user). Defaults to false — org-dashboard access is still provisioned silently for registered users.", func() {
		dsl.Default(false)
	})
	dsl.Required("email", "first_name", "last_name", "role")
})

// KeyContactUpdateBody is the DSL type for updating a key contact.
var KeyContactUpdateBody = dsl.Type("key-contact-update-body", func() {
	dsl.Description("Request body for updating a key contact (Project_Role__c record)")
	dsl.Attribute("email", dsl.String, "Contact email address; normalized to lowercase before update", func() {
		dsl.Format(dsl.FormatEmail)
		dsl.Example("john.doe@example.com")
	})
	dsl.Attribute("role", dsl.String, "Contact role designation", func() {
		dsl.Example("Technical Contact")
		dsl.Enum(constants.KeyContactRoles...)
	})
	dsl.Attribute("status", dsl.String, "Role record status, e.g. 'Active'", func() {
		dsl.Example("Active")
		dsl.Enum(constants.KeyContactStatuses...)
	})
	dsl.Attribute("board_member", dsl.Boolean, "Whether this contact holds a board member role", func() {
		dsl.Example(false)
	})
	dsl.Attribute("primary_contact", dsl.Boolean, "Whether this is the primary contact for the membership", func() {
		dsl.Example(false)
	})
	dsl.Attribute("title", dsl.String, "Contact job title. Only persisted when the email change resolves to an unknown address and a new Salesforce Contact is created; ignored if the Contact already exists.", func() {
		dsl.Example("CTO")
	})
	dsl.Attribute("send_invite", dsl.Boolean, "When true, send a platform invite (unregistered user) or role-assignment email (registered user) if the email changes. Defaults to false — org-dashboard access is still provisioned silently for registered users.", func() {
		dsl.Default(false)
	})
})

// OrgUserType describes a single principal (writer or auditor) on a b2b_org
// settings list.
// invite_status is returned on GET so callers can distinguish pending / accepted
// / revoked entries, but is NOT required on PUT — the service derives it.
// Deeper invite lifecycle fields (invite_uuid, invited_at/by, accepted_at,
// revoked_at) are maintained internally in KV and are not exposed.
var OrgUserType = dsl.Type("org-user", func() {
	dsl.Description("A writer or auditor principal on a b2b_org settings list")
	dsl.Attribute("avatar", dsl.String, "User avatar URL", func() {
		dsl.Example("https://avatars.githubusercontent.com/u/12345")
	})
	dsl.Attribute("email", dsl.String, "User email address; required to identify the principal", func() {
		dsl.Format(dsl.FormatEmail)
		dsl.Example("alice@example.com")
	})
	dsl.Attribute("name", dsl.String, "User display name", func() {
		dsl.Example("Alice Smith")
	})
	dsl.Attribute("username", dsl.String, "LFID username; absent for pending invites", func() {
		dsl.Example("alice")
	})
	dsl.Attribute("invited_as", dsl.String, "Relation being granted: writer or auditor", func() {
		dsl.Example("writer")
		dsl.Enum("writer", "auditor")
	})
	dsl.Attribute("invite_status", dsl.String, "Invite lifecycle state; returned on GET, derived by service on PUT", func() {
		dsl.Example("accepted")
		dsl.Enum("pending", "accepted", "revoked", "expired")
	})
	dsl.Required("email", "invited_as")
})

// B2BOrgSettingsResponse is the DSL type returned by GET /b2b_orgs/{uid}/settings.
var B2BOrgSettingsResponse = dsl.Type("b2b-org-settings-response", func() {
	dsl.Description("Access-control settings for a b2b_org: writers and auditors")
	dsl.Attribute("writers", dsl.ArrayOf(OrgUserType), "Org administrators (writer relation in FGA). Full-replace on PUT.")
	dsl.Attribute("auditors", dsl.ArrayOf(OrgUserType), "Read-only principals (auditor relation in FGA). Full-replace on PUT.")
	dsl.Attribute("created_at", dsl.String, "Settings record creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Settings record last-update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
})

// B2BOrgSettingsUpdateBody is the request body for PUT /b2b_orgs/{uid}/settings.
// Full-replace semantics: nil = keep existing, [] = clear the list.
var B2BOrgSettingsUpdateBody = dsl.Type("b2b-org-settings-update-body", func() {
	dsl.Description("Request body for replacing the writers and/or auditors list on a b2b_org. Full-replace: nil = keep existing; [] = remove all.")
	dsl.Attribute("writers", dsl.ArrayOf(OrgUserType), "Complete replacement list for org writers. Nil = leave unchanged; [] = remove all.")
	dsl.Attribute("auditors", dsl.ArrayOf(OrgUserType), "Complete replacement list for org auditors. Nil = leave unchanged; [] = remove all.")
})

// OrgUserAddBody is the request body for POST /b2b_orgs/{uid}/settings/users
// (per-principal add/invite). The service derives invite_status from username
// (absent here ⇒ pending) and merges the new entry without touching existing members.
var OrgUserAddBody = dsl.Type("org-user-add-body", func() {
	dsl.Description("Request body to add (invite) a single principal to a b2b_org's writers or auditors")
	dsl.Attribute("email", dsl.String, "Invitee email; identity key for the grant", func() {
		dsl.Format(dsl.FormatEmail)
		dsl.Example("alice@example.com")
	})
	dsl.Attribute("invited_as", dsl.String, "Relation to grant: writer (Admin) or auditor (Viewer)", func() {
		dsl.Example("auditor")
		dsl.Enum("writer", "auditor")
	})
	dsl.Attribute("name", dsl.String, "Optional display name; stored as provided and left empty when omitted (no server-side user lookup)", func() {
		dsl.Example("Alice Smith")
	})
	dsl.Required("email", "invited_as")
})

// OrgUserRoleBody is the request body for PUT /b2b_orgs/{uid}/settings/users/{email}
// (per-principal role change). Moves the principal between writers and auditors,
// preserving its username/invite lifecycle so an accepted grant stays accepted.
var OrgUserRoleBody = dsl.Type("org-user-role-body", func() {
	dsl.Description("Request body to change a single principal's role on a b2b_org")
	dsl.Attribute("invited_as", dsl.String, "Target relation: writer (Admin) or auditor (Viewer)", func() {
		dsl.Example("writer")
		dsl.Enum("writer", "auditor")
	})
	dsl.Required("invited_as")
})

// AdminReindexItem identifies a single entity to reindex in targeted mode.
var AdminReindexItem = dsl.Type("admin-reindex-item", func() {
	dsl.Description("A single entity to reindex (targeted mode)")
	dsl.Attribute("type", dsl.String, "Entity type: b2b_org, project_membership, key_contact, or b2b_org_settings", func() {
		dsl.Example("b2b_org")
	})
	dsl.Attribute("uid", dsl.String, "Entity UID (Salesforce ID)", func() {
		dsl.Example("001B000000IqhSLIAZ")
	})
	dsl.Required("type", "uid")
})

// AdminReindexPayload is the DSL type for the reindex admin action request.
var AdminReindexPayload = dsl.Type("admin-reindex-payload", func() {
	dsl.Description("Request payload for triggering a reindex operation")
	dsl.Attribute("types", dsl.ArrayOf(dsl.String), "Entity types to reindex (optional; default = all in-scope: b2b_org, project_membership, key_contact, b2b_org_settings). Mutually exclusive with items.", func() {
		dsl.Example([]string{"b2b_org", "project_membership"})
	})
	dsl.Attribute("since", dsl.String, "ISO 8601 / RFC 3339 timestamp with explicit zone; only records with LastModifiedDate >= since are reindexed. Mutually exclusive with items. Handler normalises to UTC. For key_contact (high-volume), prefer a ~2-year window (e.g. 2024-06-01T00:00:00Z) to sync only the active set instead of the full ~300k records.", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2026-05-20T00:00:00Z")
	})
	dsl.Attribute("items", dsl.ArrayOf(AdminReindexItem), "Targeted list of entities to reindex (surgical mode). Mutually exclusive with types and since. Max 100 items.", func() {
		dsl.MaxLength(100)
	})
	dsl.Attribute("dry_run", dsl.Boolean, "When true, walk SOQL/live-path but skip publishing. Final log includes would_publish_count.", func() {
		dsl.Default(false)
	})
})

// AdminReindexResult is the DSL type for the reindex admin action result.
var AdminReindexResult = dsl.Type("admin-reindex-result", func() {
	dsl.Description("Result of a reindex operation")
	dsl.Attribute("run_id", dsl.String, "Correlation ID for the reindex run (for log lookups)", func() {
		dsl.Format(dsl.FormatUUID)
		dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
	})
	dsl.Required("run_id")
})

// ── Workspace types ───────────────────────────────────────────────────────────

// WorkspaceProjectResponse is the DSL type for a project associated with a workspace.
var WorkspaceProjectResponse = dsl.Type("workspace-project-response", func() {
	dsl.Description("A project association within a workspace")
	dsl.Attribute("project_uid", dsl.String, "Opaque project reference stored verbatim from the caller (for example, an Org Lens project slug); not validated or resolved to a v2 project UUID", func() {
		dsl.Example("my-project")
	})
	dsl.Attribute("project_sfid", dsl.String, "Reserved project metadata; the workspace write path does not populate it, so it is always empty", func() {
		dsl.Example("")
	})
	dsl.Attribute("project_slug", dsl.String, "Reserved project metadata; the workspace write path does not populate it, so it is always empty", func() {
		dsl.Example("")
	})
	dsl.Attribute("project_name", dsl.String, "Reserved project metadata; the workspace write path does not populate it, so it is always empty", func() {
		dsl.Example("")
	})
	dsl.Attribute("created_by", dsl.String, "LFID username of the principal who added this project", func() {
		dsl.Example("alice")
	})
	dsl.Attribute("updated_by", dsl.String, "LFID username of the principal who last updated this association", func() {
		dsl.Example("alice")
	})
	dsl.Attribute("created_at", dsl.String, "Timestamp when the project was added", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2026-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Timestamp when this association was last updated", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2026-01-01T00:00:00Z")
	})
	dsl.Required("project_uid")
})

// WorkspaceResponse is the DSL type returned by workspace write endpoints.
var WorkspaceResponse = dsl.Type("workspace-response", func() {
	dsl.Description("A named container of project associations within a b2b_org")
	dsl.Attribute("uid", dsl.String, "Workspace UID", func() {
		dsl.Format(dsl.FormatUUID)
		dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
	})
	dsl.Attribute("name", dsl.String, "Workspace display name", func() {
		dsl.Example("My Workspace")
	})
	dsl.Attribute("projects", dsl.ArrayOf(WorkspaceProjectResponse), "Project associations in this workspace")
	dsl.Attribute("created_by", dsl.String, "LFID username of the creator", func() {
		dsl.Example("alice")
	})
	dsl.Attribute("updated_by", dsl.String, "LFID username of the last updater", func() {
		dsl.Example("alice")
	})
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2026-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last-update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2026-06-01T12:00:00Z")
	})
	dsl.Required("uid", "name")
})

// WorkspaceCreateBody is the request body for POST /b2b_orgs/{uid}/workspaces.
var WorkspaceCreateBody = dsl.Type("workspace-create-body", func() {
	dsl.Description("Request body for creating a workspace")
	dsl.Attribute("name", dsl.String, "Workspace display name; must be unique within the org", func() {
		dsl.MinLength(1)
		dsl.MaxLength(255)
		dsl.Example("My Workspace")
	})
	dsl.Required("name")
})

// WorkspaceUpdateBody is the request body for PUT /b2b_orgs/{uid}/workspaces/{workspace_uid}.
var WorkspaceUpdateBody = dsl.Type("workspace-update-body", func() {
	dsl.Description("Request body for renaming a workspace")
	dsl.Attribute("name", dsl.String, "New workspace display name; must be unique within the org", func() {
		dsl.MinLength(1)
		dsl.MaxLength(255)
		dsl.Example("Renamed Workspace")
	})
	dsl.Required("name")
})

// WorkspaceProjectAddBody is the request body for POST /b2b_orgs/{uid}/workspaces/{workspace_uid}/projects.
var WorkspaceProjectAddBody = dsl.Type("workspace-project-add-body", func() {
	dsl.Description("Request body for adding a single project to a workspace")
	dsl.Attribute("project_id", dsl.String, "Opaque project reference string", func() {
		dsl.MaxLength(512)
		dsl.Example("my-project")
	})
	dsl.Required("project_id")
})

// WorkspaceProjectsBulkAddBody is the request body for POST /b2b_orgs/{uid}/workspaces/{workspace_uid}/projects/bulk.
var WorkspaceProjectsBulkAddBody = dsl.Type("workspace-projects-bulk-add-body", func() {
	dsl.Description("Request body for adding multiple projects to a workspace in one operation")
	dsl.Attribute("project_ids", dsl.ArrayOf(dsl.String, func() {
		dsl.MaxLength(512)
	}), "Opaque project reference strings; at most 100 per request, each at most 512 characters", func() {
		dsl.MinLength(1)
		dsl.MaxLength(100)
	})
	dsl.Required("project_ids")
})

// WorkspaceBulkAddResult is the per-item error detail for a bulk-add failure.
var WorkspaceBulkAddItemError = dsl.Type("workspace-bulk-add-item-error", func() {
	dsl.Description("Per-item failure detail in a bulk workspace project add")
	dsl.Attribute("project_id", dsl.String, "The project identifier that failed", func() {
		dsl.Example("unknown-project")
	})
	dsl.Attribute("error", dsl.String, "Reason the project could not be added", func() {
		dsl.Example("unknown project")
	})
	dsl.Required("project_id", "error")
})

// WorkspaceBulkResponse is the result type for POST /…/projects/bulk.
var WorkspaceBulkResponse = dsl.Type("workspace-bulk-response", func() {
	dsl.Description("Result of a bulk workspace project add: the updated workspace plus per-item success/failure detail")
	dsl.Attribute("workspace", WorkspaceResponse, "The workspace after all successful additions")
	dsl.Attribute("succeeded", dsl.ArrayOf(dsl.String), "Project references that were successfully added (or were already present)", func() {
		dsl.Example([]string{"a1b2c3d4-e5f6-7890-abcd-ef1234567890"})
	})
	dsl.Attribute("failed", dsl.ArrayOf(WorkspaceBulkAddItemError), "Projects that could not be added with per-item error detail")
	ETagAttribute()
	LastModifiedAttribute()
	dsl.Required("workspace", "succeeded", "failed")
})
