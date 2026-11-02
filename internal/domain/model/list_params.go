// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// ListParams represents pagination and filter parameters.
type ListParams struct {
	PageSize int
	Offset   int
	Filters  map[string]string
	Search   string
}

// SortOrder controls the ordering of list results.
type SortOrder string

const (
	// SortOrderName sorts results alphabetically by company (member) name.
	// Maps to ORDER BY Account.Name ASC in SOQL.
	SortOrderName SortOrder = "name"

	// SortOrderNewest sorts results by membership creation date, newest first.
	// Maps to ORDER BY CreatedDate DESC in SOQL.
	SortOrderNewest SortOrder = "newest"

	// SortOrderLastModified sorts results by most recently modified first.
	// Maps to ORDER BY LastModifiedDate DESC in SOQL.
	SortOrderLastModified SortOrder = "last_modified"

	// SortOrderDefault is the default sort order when none is specified.
	// Salesforce returns results in an unspecified but stable-ish order when no
	// ORDER BY clause is present; we default to newest-first so that the most
	// recently added members appear on the first page.
	SortOrderDefault = SortOrderNewest
)

// MembershipFilters holds the SOQL-pushable filter predicates for
// ListMembershipsForProject. Only non-empty fields are applied; an empty
// MembershipFilters value means "no additional filtering" (the base query
// always restricts to Status = 'Active').
//
// Note: Status is not exposed as a filter — all list queries are hardcoded to
// active members only. Asset.Year__c is null on many projects and is not
// exposed. Asset.Tier__c is inaccessible at the field-level read permission
// level and is always null in results.
// Use the ListProjectTiers endpoint to discover tier UIDs for filtering.
type MembershipFilters struct {
	// TierUID is a v2 tier UUID. It is decoded to a Salesforce Product2Id SFID
	// and used as an exact-match filter on Asset.Product2Id in SOQL.
	// Use the ListProjectTiers endpoint to discover available tier UIDs.
	TierUID string

	// CompanyNameSearch is a free-text substring to match against
	// Account.Name via a SOQL LIKE predicate. This field MUST always be
	// lowercase — callers are responsible for normalising with
	// strings.ToLower before setting it. Lowercasing here rather than at
	// the query or cache-key level ensures a single canonical value is
	// interpolated into both the SOQL query and the NATS KV cache key.
	CompanyNameSearch string

	// SortOrder controls the ORDER BY clause in the SOQL query. Defaults to
	// SortOrderNewest when not set.
	SortOrder SortOrder

	// PageToken is an opaque cursor returned in a previous MembershipPage
	// response. It encodes a batch index and iterator into the NATS KV batch
	// cache maintained by the Salesforce reader (see soql_pager.go), along
	// with a FingerprintCursor digest of the other filter fields in effect
	// when the cursor was minted. When non-empty, TierUID, SortOrder, and
	// CompanyNameSearch must be resent identically to the request that
	// produced the token — a mismatch against the embedded fingerprint is
	// rejected with a 400 rather than silently ignored, since resuming a
	// cached page sequence under different filters would return results that
	// don't match the caller's stated query.
	PageToken string
}

// IsEmpty reports whether f has no filter predicates set.
func (f MembershipFilters) IsEmpty() bool {
	return f.TierUID == "" && f.CompanyNameSearch == "" && f.SortOrder == "" && f.PageToken == ""
}

// EffectiveSortOrder returns the sort order to apply, substituting the default
// when none is explicitly set.
func (f MembershipFilters) EffectiveSortOrder() SortOrder {
	if f.SortOrder == "" {
		return SortOrderDefault
	}
	return f.SortOrder
}

// MembershipPage is the result of a paginated ListMembershipsForProject call.
// It carries both the current page of records and an opaque cursor token for
// the next page (empty when this is the last page).
type MembershipPage struct {
	// Memberships is the current page of ProjectMembership records.
	Memberships []*ProjectMembership

	// NextPageToken is an opaque cursor that can be passed as
	// MembershipFilters.PageToken to retrieve the next page. Empty string
	// means this is the last page.
	NextPageToken string

	// TotalSize is the total number of records matching the query, as reported
	// by Salesforce. This is set on the first page; subsequent pages (using a
	// page token) may return 0 if the locator response does not repeat it.
	TotalSize int
}
