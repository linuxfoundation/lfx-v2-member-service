// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// B2BOrgReader provides read access to B2BOrg (Salesforce Account) records.
// Implementations are responsible for resolving the UUID from the Salesforce
// Account.Id and populating all fields of the returned model.B2BOrg.
type B2BOrgReader interface {
	// GetB2BOrg returns the B2BOrg identified by its v2 UUID. Returns an error
	// wrapping ErrNotFound if no record exists for the given uid.
	GetB2BOrg(ctx context.Context, uid string) (*model.B2BOrg, error)

	// FetchChildUIDsByParentUID returns the v2 UUIDs of all direct child orgs
	// whose Salesforce Account.ParentId matches the given parent UID. Returns an
	// empty slice when the parent has no children. Used to build the FGA child-list
	// tuples that enable the b2b_org hierarchy view cascade.
	FetchChildUIDsByParentUID(ctx context.Context, parentUID string) ([]string, error)

	// FetchChildUIDsByParentUIDs returns a map of parentUID → []childUID for all
	// given parent UIDs that have at least one direct member-eligible child Account.
	// Parents with no qualifying children are absent from the result map.
	// Used to bulk-compute is_parent and FGA parent tuples for a batch of orgs in
	// place of N individual FetchChildUIDsByParentUID calls.
	FetchChildUIDsByParentUIDs(ctx context.Context, parentUIDs []string) (map[string][]string, error)

	// FindByNameOrWebsite resolves a single member-eligible Account by primary
	// domain, domain alias, website, then name, in that priority order. Consulted by
	// committee-service when an organization.id does not resolve directly via
	// GetB2BOrg (e.g. a legacy, non-SFID id for an org that exists under a
	// different SFID). Returns found=false (not an error) when no tier
	// produces exactly one match, including when a tier's result is
	// ambiguous — a less specific tier could otherwise resolve to a
	// different, incorrect organization than the one a more specific tier
	// couldn't safely identify.
	FindByNameOrWebsite(ctx context.Context, name, website string) (*model.B2BOrg, bool, error)
}
