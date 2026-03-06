// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	"goa.design/goa/v3/dsl"
)

// AccountType is the DSL type for an account.
var AccountType = dsl.Type("account-type", func() {
	dsl.Attribute("id", dsl.String, "Account ID", func() {
		dsl.Example("acc-123")
	})
	dsl.Attribute("name", dsl.String, "Account name", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("logo_url", dsl.String, "Account logo URL", func() {
		dsl.Example("https://example.com/logo.png")
	})
	dsl.Attribute("website", dsl.String, "Account website", func() {
		dsl.Example("https://example.com")
	})
})

// ContactType is the DSL type for a contact.
var ContactType = dsl.Type("contact-type", func() {
	dsl.Attribute("id", dsl.String, "Contact ID", func() {
		dsl.Example("con-123")
	})
	dsl.Attribute("first_name", dsl.String, "First name", func() {
		dsl.Example("John")
	})
	dsl.Attribute("last_name", dsl.String, "Last name", func() {
		dsl.Example("Doe")
	})
	dsl.Attribute("email", dsl.String, "Email address", func() {
		dsl.Example("john.doe@example.com")
	})
	dsl.Attribute("title", dsl.String, "Job title", func() {
		dsl.Example("CTO")
	})
})

// ProductType is the DSL type for a product.
var ProductType = dsl.Type("product-type", func() {
	dsl.Attribute("id", dsl.String, "Product ID", func() {
		dsl.Example("prod-123")
	})
	dsl.Attribute("name", dsl.String, "Product name", func() {
		dsl.Example("Gold Membership")
	})
	dsl.Attribute("family", dsl.String, "Product family", func() {
		dsl.Example("Membership")
	})
	dsl.Attribute("type", dsl.String, "Product type", func() {
		dsl.Example("Corporate")
	})
})

// ProjectType is the DSL type for a project.
var ProjectType = dsl.Type("project-type", func() {
	dsl.Attribute("id", dsl.String, "Project ID", func() {
		dsl.Example("proj-123")
	})
	dsl.Attribute("name", dsl.String, "Project name", func() {
		dsl.Example("Linux Foundation")
	})
	dsl.Attribute("logo_url", dsl.String, "Project logo URL", func() {
		dsl.Example("https://example.com/project-logo.png")
	})
	dsl.Attribute("slug", dsl.String, "Project slug", func() {
		dsl.Example("linux-foundation")
	})
	dsl.Attribute("status", dsl.String, "Project status", func() {
		dsl.Example("Active")
	})
})

// OrganizationType is the DSL type for an organization.
var OrganizationType = dsl.Type("organization-type", func() {
	dsl.Attribute("id", dsl.String, "Organization ID", func() {
		dsl.Example("org-123")
	})
	dsl.Attribute("name", dsl.String, "Organization name", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("logo_url", dsl.String, "Organization logo URL", func() {
		dsl.Example("https://example.com/org-logo.png")
	})
	dsl.Attribute("website", dsl.String, "Organization website", func() {
		dsl.Example("https://example.com")
	})
})

// MembershipSummaryItemType is the DSL type for a membership summary item.
var MembershipSummaryItemType = dsl.Type("membership-summary-item-type", func() {
	dsl.Attribute("uid", dsl.String, "Membership UID", func() {
		dsl.Example("7cad5a8d-19d0-41a4-81a6-043453daf9ee")
		dsl.Format(dsl.FormatUUID)
	})
	dsl.Attribute("name", dsl.String, "Membership name", func() {
		dsl.Example("Gold Membership - Example Corp")
	})
	dsl.Attribute("status", dsl.String, "Membership status", func() {
		dsl.Example("Active")
	})
	dsl.Attribute("year", dsl.String, "Membership year", func() {
		dsl.Example("2025")
	})
	dsl.Attribute("tier", dsl.String, "Membership tier", func() {
		dsl.Example("Gold")
	})
	dsl.Attribute("membership_type", dsl.String, "Membership type", func() {
		dsl.Example("Corporate")
	})
	dsl.Attribute("auto_renew", dsl.Boolean, "Whether auto-renew is enabled", func() {
		dsl.Example(true)
	})
	dsl.Attribute("start_date", dsl.String, "Start date", func() {
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("end_date", dsl.String, "End date", func() {
		dsl.Example("2025-12-31T23:59:59Z")
	})
	dsl.Attribute("product", ProductType, "Product information")
	dsl.Attribute("project", ProjectType, "Project information")
})

// MembershipSummaryType is the DSL type for a membership summary.
var MembershipSummaryType = dsl.Type("membership-summary-type", func() {
	dsl.Description("Summary of memberships for a member")
	dsl.Attribute("active_count", dsl.Int, "Number of active memberships", func() {
		dsl.Example(1)
	})
	dsl.Attribute("total_count", dsl.Int, "Total number of memberships", func() {
		dsl.Example(2)
	})
	dsl.Attribute("memberships", dsl.ArrayOf(MembershipSummaryItemType), "List of membership details")
})

// MemberResponse is the DSL type for a member response.
var MemberResponse = dsl.Type("member-response", func() {
	dsl.Description("A member (account/organization) resource")
	dsl.Attribute("uid", dsl.String, "Member UID", func() {
		dsl.Example("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
		dsl.Format(dsl.FormatUUID)
	})
	dsl.Attribute("name", dsl.String, "Member name", func() {
		dsl.Example("Example Corp")
	})
	dsl.Attribute("logo_url", dsl.String, "Member logo URL", func() {
		dsl.Example("https://example.com/logo.png")
	})
	dsl.Attribute("website", dsl.String, "Member website", func() {
		dsl.Example("https://example.com")
	})
	dsl.Attribute("membership_summary", MembershipSummaryType, "Membership summary")
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
})

// MembershipAttributes defines the attributes for a membership.
func MembershipAttributes() {
	dsl.Attribute("uid", dsl.String, "Membership UID", func() {
		dsl.Example("7cad5a8d-19d0-41a4-81a6-043453daf9ee")
		dsl.Format(dsl.FormatUUID)
	})
	dsl.Attribute("name", dsl.String, "Membership name", func() {
		dsl.Example("Gold Membership - Example Corp")
	})
	dsl.Attribute("status", dsl.String, "Membership status", func() {
		dsl.Example("Active")
	})
	dsl.Attribute("year", dsl.String, "Membership year", func() {
		dsl.Example("2025")
	})
	dsl.Attribute("tier", dsl.String, "Membership tier", func() {
		dsl.Example("Gold")
	})
	dsl.Attribute("membership_type", dsl.String, "Membership type", func() {
		dsl.Example("Corporate")
	})
	dsl.Attribute("auto_renew", dsl.Boolean, "Whether auto-renew is enabled", func() {
		dsl.Example(true)
	})
	dsl.Attribute("renewal_type", dsl.String, "Renewal type", func() {
		dsl.Example("Annual")
	})
	dsl.Attribute("price", dsl.Float64, "Membership price", func() {
		dsl.Example(50000.0)
	})
	dsl.Attribute("annual_full_price", dsl.Float64, "Annual full price", func() {
		dsl.Example(50000.0)
	})
	dsl.Attribute("payment_frequency", dsl.String, "Payment frequency", func() {
		dsl.Example("Annual")
	})
	dsl.Attribute("payment_terms", dsl.String, "Payment terms", func() {
		dsl.Example("Net 30")
	})
	dsl.Attribute("agreement_date", dsl.String, "Agreement date", func() {
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("purchase_date", dsl.String, "Purchase date", func() {
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("start_date", dsl.String, "Start date", func() {
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("end_date", dsl.String, "End date", func() {
		dsl.Example("2025-12-31T23:59:59Z")
	})
	dsl.Attribute("account", AccountType, "Account information")
	dsl.Attribute("contact", ContactType, "Contact information")
	dsl.Attribute("product", ProductType, "Product information")
	dsl.Attribute("project", ProjectType, "Project information")
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
}

// MembershipResponse is the DSL type for a membership response.
var MembershipResponse = dsl.Type("membership-response", func() {
	dsl.Description("A membership resource")
	MembershipAttributes()
})

// KeyContactAttributes defines the attributes for a key contact.
func KeyContactAttributes() {
	dsl.Attribute("uid", dsl.String, "Key contact UID", func() {
		dsl.Example("2200b646-fbb2-4de7-ad80-fd195a874baf")
		dsl.Format(dsl.FormatUUID)
	})
	dsl.Attribute("membership_uid", dsl.String, "Membership UID", func() {
		dsl.Example("7cad5a8d-19d0-41a4-81a6-043453daf9ee")
		dsl.Format(dsl.FormatUUID)
	})
	dsl.Attribute("role", dsl.String, "Contact role", func() {
		dsl.Example("Primary Contact")
	})
	dsl.Attribute("status", dsl.String, "Contact status", func() {
		dsl.Example("Active")
	})
	dsl.Attribute("board_member", dsl.Boolean, "Whether this is a board member", func() {
		dsl.Example(false)
	})
	dsl.Attribute("primary_contact", dsl.Boolean, "Whether this is a primary contact", func() {
		dsl.Example(true)
	})
	dsl.Attribute("contact", ContactType, "Contact details")
	dsl.Attribute("project", ProjectType, "Project information")
	dsl.Attribute("organization", OrganizationType, "Organization information")
	dsl.Attribute("created_at", dsl.String, "Creation timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-01-01T00:00:00Z")
	})
	dsl.Attribute("updated_at", dsl.String, "Last update timestamp", func() {
		dsl.Format(dsl.FormatDateTime)
		dsl.Example("2025-06-01T12:00:00Z")
	})
}

// KeyContactResponse is the DSL type for a key contact response.
var KeyContactResponse = dsl.Type("key-contact-response", func() {
	dsl.Description("A key contact resource")
	KeyContactAttributes()
})

// ListMetadata is the DSL type for list metadata.
var ListMetadata = dsl.Type("list-metadata", func() {
	dsl.Description("Pagination metadata for list responses")
	dsl.Attribute("total_size", dsl.Int, "Total number of items", func() {
		dsl.Example(100)
	})
	dsl.Attribute("page_size", dsl.Int, "Number of items per page", func() {
		dsl.Example(10)
	})
	dsl.Attribute("offset", dsl.Int, "Offset into the total list", func() {
		dsl.Example(0)
	})
	dsl.Required("total_size", "page_size", "offset")
})

// BearerTokenAttribute is the DSL attribute for bearer token.
func BearerTokenAttribute() {
	dsl.Token("bearer_token", dsl.String, func() {
		dsl.Description("JWT token issued by Heimdall")
		dsl.Example("eyJhbGci...")
	})
}

// VersionAttribute is the DSL attribute for API version.
func VersionAttribute() {
	dsl.Attribute("version", dsl.String, "Version of the API", func() {
		dsl.Example("1")
		dsl.Enum("1")
	})
}

// ETagAttribute is the DSL attribute for ETag header.
func ETagAttribute() {
	dsl.Attribute("etag", dsl.String, "ETag header value", func() {
		dsl.Example("123")
	})
}

// MemberIDAttribute is the DSL attribute for member ID.
func MemberIDAttribute() {
	dsl.Attribute("member_id", dsl.String, "Member UID", func() {
		dsl.Example("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
		dsl.Format(dsl.FormatUUID)
	})
}

// MembershipIDAttribute is the DSL attribute for membership ID.
func MembershipIDAttribute() {
	dsl.Attribute("id", dsl.String, "Membership UID", func() {
		dsl.Example("7cad5a8d-19d0-41a4-81a6-043453daf9ee")
		dsl.Format(dsl.FormatUUID)
	})
}

// PageSizeAttribute is the DSL attribute for page size.
func PageSizeAttribute() {
	dsl.Attribute("pageSize", dsl.Int, "Number of items per page", func() {
		dsl.Default(25)
		dsl.Minimum(1)
		dsl.Maximum(100)
		dsl.Example(25)
	})
}

// OffsetAttribute is the DSL attribute for offset.
func OffsetAttribute() {
	dsl.Attribute("offset", dsl.Int, "Offset into the total list", func() {
		dsl.Default(0)
		dsl.Minimum(0)
		dsl.Example(0)
	})
}

// FilterAttribute is the DSL attribute for filter.
func FilterAttribute() {
	dsl.Attribute("filter", dsl.String, "Filter expression (key=value pairs separated by semicolons)", func() {
		dsl.Example("status=Active;tier=Gold")
	})
}

// SearchAttribute is the DSL attribute for free-text search.
func SearchAttribute() {
	dsl.Attribute("search", dsl.String, "Free-text search across member name, project names, and tiers", func() {
		dsl.Example("Linux")
	})
}

// Error types

// BadRequestError is the DSL type for a bad request error.
var BadRequestError = dsl.Type("bad-request-error", func() {
	dsl.Attribute("message", dsl.String, "Error message", func() {
		dsl.Example("The request was invalid.")
	})
	dsl.Required("message")
})

// NotFoundError is the DSL type for a not found error.
var NotFoundError = dsl.Type("not-found-error", func() {
	dsl.Attribute("message", dsl.String, "Error message", func() {
		dsl.Example("The resource was not found.")
	})
	dsl.Required("message")
})

// InternalServerError is the DSL type for an internal server error.
var InternalServerError = dsl.Type("internal-server-error", func() {
	dsl.Attribute("message", dsl.String, "Error message", func() {
		dsl.Example("An internal server error occurred.")
	})
	dsl.Required("message")
})

// ServiceUnavailableError is the DSL type for a service unavailable error.
var ServiceUnavailableError = dsl.Type("service-unavailable-error", func() {
	dsl.Attribute("message", dsl.String, "Error message", func() {
		dsl.Example("The service is unavailable.")
	})
	dsl.Required("message")
})
