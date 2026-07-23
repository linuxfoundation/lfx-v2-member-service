// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// primaryEmailSOQL fetches primary alternate email addresses for a set of
// contact IDs. The caller must substitute a buildSOQLInClause result for the
// %s placeholder.
const primaryEmailSOQL = `
SELECT Contact_Name__c, Alternate_Email_Address__c
FROM Alternate_Email__c
WHERE Contact_Name__c IN (%s)
    AND Primary_Email__c = true
`

// keyContactsByAssetSOQL fetches Project_Role__c records for a specific Asset
// (membership). Used for single-membership key contact lookups. The caller must
// substitute a quoteSOQL-escaped ID for the %s placeholder.
//
// Account fields are pulled via the Asset relationship (Asset__r.Account) so
// that company data matches the membership record rather than the contact's
// own account.
const keyContactsByAssetSOQL = `
SELECT
    Id, Asset__c, Contact__c, Role__c, Status__c,
    BoardMember__c, PrimaryContact__c,
    CreatedDate, SystemModstamp,
    Contact__r.Id, Contact__r.FirstName, Contact__r.LastName, Contact__r.Title, Contact__r.Email,
    Asset__r.Id, Asset__r.AccountId, Asset__r.Product2Id, Asset__r.Projects__c,
    Asset__r.Account.Id, Asset__r.Account.Name,
    Asset__r.Account.Logo_URL__c, Asset__r.Account.Website,
    Asset__r.Projects__r.Id, Asset__r.Projects__r.Name,
    Asset__r.Projects__r.Slug__c, Asset__r.Projects__r.Project_Logo__c
FROM Project_Role__c
WHERE Asset__c = %s
    AND IsDeleted = false
`

// keyContactByIDSOQL fetches a single Project_Role__c record by its Salesforce
// ID. The caller must substitute a quoteSOQL-escaped ID for the %s placeholder.
const keyContactByIDSOQL = `
SELECT
    Id, Asset__c, Contact__c, Role__c, Status__c,
    BoardMember__c, PrimaryContact__c,
    CreatedDate, SystemModstamp,
    Contact__r.Id, Contact__r.FirstName, Contact__r.LastName, Contact__r.Title, Contact__r.Email,
    Asset__r.Id, Asset__r.AccountId, Asset__r.Product2Id, Asset__r.Projects__c,
    Asset__r.Account.Id, Asset__r.Account.Name,
    Asset__r.Account.Logo_URL__c, Asset__r.Account.Website,
    Asset__r.Projects__r.Id, Asset__r.Projects__r.Name,
    Asset__r.Projects__r.Slug__c, Asset__r.Projects__r.Project_Logo__c
FROM Project_Role__c
WHERE Id = %s
    AND IsDeleted = false
`

// KeyContactRepo handles Salesforce SOQL queries for key contacts
// (Project_Role__c records).
type KeyContactRepo struct {
	client *sf.Salesforce
}

// NewKeyContactRepo creates a new KeyContactRepo backed by the given Salesforce
// client.
func NewKeyContactRepo(client *sf.Salesforce) *KeyContactRepo {
	return &KeyContactRepo{client: client}
}

// FetchKeyContactsByAssetSFID fetches all key contacts for a specific Asset
// (membership) by its Salesforce ID, returning KeyContact domain objects
// with inline Contact and Account fields.
func (r *KeyContactRepo) FetchKeyContactsByAssetSFID(ctx context.Context, assetSFID string) ([]*model.KeyContact, error) {
	slog.DebugContext(ctx, "fetching key contacts for asset from Salesforce",
		"asset_sfid", assetSFID,
	)

	var roles []soqlProjectRole
	if err := r.client.Query(fmt.Sprintf(keyContactsByAssetSOQL, quoteSOQL(assetSFID)), &roles); err != nil {
		return nil, fmt.Errorf("fetching key contacts for asset %s: %w", assetSFID, err)
	}

	contactIDs := collectProjectRoleContactIDs(roles)

	emailMap, err := r.fetchPrimaryEmails(ctx, contactIDs)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch primary emails for asset key contacts",
			"error", err,
		)
		emailMap = make(map[string]string)
	}

	contacts := make([]*model.KeyContact, 0, len(roles))
	for _, role := range roles {
		c, convertErr := convertSOQLToKeyContact(role, emailMap)
		if convertErr != nil {
			slog.WarnContext(ctx, "skipping key contact with invalid SFID",
				"sfid", role.ID,
				"error", convertErr,
			)
			continue
		}
		contacts = append(contacts, c)
	}

	return contacts, nil
}

// FetchKeyContactBySFID fetches a single key contact by its Salesforce
// Project_Role__c ID. Returns nil if the record is not found.
func (r *KeyContactRepo) FetchKeyContactBySFID(ctx context.Context, sfid string) (*model.KeyContact, error) {
	slog.DebugContext(ctx, "fetching key contact from Salesforce by SFID", "sfid", sfid)

	var roles []soqlProjectRole
	if err := r.client.Query(fmt.Sprintf(keyContactByIDSOQL, quoteSOQL(sfid)), &roles); err != nil {
		return nil, fmt.Errorf("fetching key contact by SFID %s: %w", sfid, err)
	}

	if len(roles) == 0 {
		return nil, nil
	}

	// Fetch primary email for the single contact on this role record.
	emailMap := make(map[string]string)
	if roles[0].Contact != nil && roles[0].Contact.ID != "" {
		em, emailErr := r.fetchPrimaryEmails(ctx, []string{roles[0].Contact.ID})
		if emailErr != nil {
			slog.WarnContext(ctx, "failed to fetch primary email for key contact",
				"error", emailErr,
			)
		} else {
			emailMap = em
		}
	}

	return convertSOQLToKeyContact(roles[0], emailMap)
}

// keyContactsByIDsSOQL fetches multiple Project_Role__c records by a set of
// Salesforce IDs. The caller must substitute a buildSOQLInClause result for
// the %s placeholder. Applies the same IsDeleted = false filter as the
// single-record path.
const keyContactsByIDsSOQL = `
SELECT
    Id, Asset__c, Contact__c, Role__c, Status__c,
    BoardMember__c, PrimaryContact__c,
    CreatedDate, SystemModstamp,
    Contact__r.Id, Contact__r.FirstName, Contact__r.LastName, Contact__r.Title, Contact__r.Email,
    Asset__r.Id, Asset__r.AccountId, Asset__r.Product2Id, Asset__r.Projects__c,
    Asset__r.Account.Id, Asset__r.Account.Name,
    Asset__r.Account.Logo_URL__c, Asset__r.Account.Website,
    Asset__r.Projects__r.Id, Asset__r.Projects__r.Name,
    Asset__r.Projects__r.Slug__c, Asset__r.Projects__r.Project_Logo__c
FROM Project_Role__c
WHERE Id IN (%s)
    AND IsDeleted = false
`

// FetchKeyContactsBySFIDs fetches multiple Project_Role__c records by their
// Salesforce IDs in a single batched SOQL query. Primary email addresses are
// fetched via the same batched fetchPrimaryEmails path used by the
// single-record fetch, so the email field matches the read path. IDs absent
// from the result set have been soft-deleted. Returns an empty slice when none
// are found.
func (r *KeyContactRepo) FetchKeyContactsBySFIDs(ctx context.Context, sfids []string) ([]*model.KeyContact, []string, error) {
	if len(sfids) == 0 {
		return []*model.KeyContact{}, nil, nil
	}
	slog.DebugContext(ctx, "fetching key contacts by SFIDs from Salesforce (batch)", "count", len(sfids))

	const batchSize = 200
	var allRoles []soqlProjectRole
	for start := 0; start < len(sfids); start += batchSize {
		end := start + batchSize
		if end > len(sfids) {
			end = len(sfids)
		}
		chunk := sfids[start:end]

		var roles []soqlProjectRole
		if err := r.client.Query(fmt.Sprintf(keyContactsByIDsSOQL, buildSOQLInClause(chunk)), &roles); err != nil {
			return nil, nil, fmt.Errorf("batch fetching key contacts (chunk %d-%d): %w", start, end, err)
		}
		allRoles = append(allRoles, roles...)
	}

	// Collect all contact IDs for a single primary-email fetch.
	contactIDs := collectProjectRoleContactIDs(allRoles)
	emailMap, err := r.fetchPrimaryEmails(ctx, contactIDs)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch primary emails for batch key contacts",
			"error", err,
		)
		emailMap = make(map[string]string)
	}

	contacts := make([]*model.KeyContact, 0, len(allRoles))
	var conversionErrorSFIDs []string
	for _, role := range allRoles {
		kc, convErr := convertSOQLToKeyContact(role, emailMap)
		if convErr != nil {
			slog.WarnContext(ctx, "skipping key contact with invalid SFID in batch",
				"sfid", role.ID, "error", convErr)
			conversionErrorSFIDs = append(conversionErrorSFIDs, role.ID)
			continue
		}
		contacts = append(contacts, kc)
	}
	return contacts, conversionErrorSFIDs, nil
}

// Ensure KeyContactRepo satisfies the port at compile time.
var _ port.KeyContactBatchReader = (*KeyContactRepo)(nil)

// fetchPrimaryEmails fetches the primary alternate email address for each of the
// given contact IDs. Returns a map of contactID → email address. Requests are
// automatically batched to stay within SOQL IN-clause limits.
//
// ctx is checked for cancellation before each batch so a timed-out or cancelled
// caller aborts promptly even though the underlying go-salesforce Query call is
// not context-aware.
func (r *KeyContactRepo) fetchPrimaryEmails(ctx context.Context, contactIDs []string) (map[string]string, error) {
	if len(contactIDs) == 0 {
		return make(map[string]string), nil
	}

	const batchSize = 200
	emailMap := make(map[string]string, len(contactIDs))

	for i := 0; i < len(contactIDs); i += batchSize {
		if err := ctx.Err(); err != nil {
			slog.WarnContext(ctx, "fetchPrimaryEmails aborted by context",
				"batch_start", i, "total", len(contactIDs), "reason", err)
			return nil, fmt.Errorf("fetchPrimaryEmails: context cancelled at batch %d: %w", i, err)
		}

		end := min(i+batchSize, len(contactIDs))
		batch := contactIDs[i:end]

		inClause := buildSOQLInClause(batch)
		soql := fmt.Sprintf(primaryEmailSOQL, inClause)

		var emails []soqlAlternateEmail
		if err := r.client.Query(soql, &emails); err != nil {
			return nil, fmt.Errorf("fetching primary emails (batch %d-%d): %w", i, end, err)
		}

		slog.DebugContext(ctx, "fetched primary email batch",
			"batch_start", i, "batch_end", end, "results", len(emails))

		for _, e := range emails {
			emailMap[e.ContactID] = e.Email
		}
	}

	return emailMap, nil
}

// convertSOQLToKeyContact converts a Salesforce Project_Role__c SOQL
// result to the domain KeyContact model. Contact and company (Account)
// attributes are inlined directly onto the struct — no sub-objects are used.
// Company data is sourced from the Asset's Account (Asset__r.Account) so that
// it is consistent with the associated ProjectMembership record.
func convertSOQLToKeyContact(role soqlProjectRole, emailMap map[string]string) (*model.KeyContact, error) {
	contactUID, err := sfuuid.Normalize18(role.ID)
	if err != nil {
		return nil, fmt.Errorf("normalizing project role SFID %q: %w", role.ID, err)
	}

	membershipUID, err := sfuuid.Normalize18(role.AssetID)
	if err != nil {
		return nil, fmt.Errorf("normalizing asset SFID %q: %w", role.AssetID, err)
	}

	c := &model.KeyContact{
		UID:            contactUID,
		MembershipUID:  membershipUID,
		Role:           derefString(role.Role),
		Status:         derefString(role.Status),
		BoardMember:    role.BoardMember,
		PrimaryContact: role.PrimaryContact,
	}

	// Derive TierUID from the Product2 SFID stored on the Asset relationship.
	// The Asset carries Product2Id but the SOQL projection here only returns the
	// Asset FK fields; TierUID is left empty when the relationship is absent and
	// can be populated by a caller that cross-references the membership record.
	if role.Asset != nil {
		if role.Asset.Product2ID != "" {
			tierUID, tierErr := sfuuid.Normalize18(role.Asset.Product2ID)
			if tierErr == nil {
				c.TierUID = tierUID
			}
		}

		// Company (Account) fields — sourced from the Asset's Account so the
		// data is consistent with the associated ProjectMembership.
		if role.Asset.Account != nil {
			c.CompanyName = role.Asset.Account.Name
			c.CompanyLogoURL = derefString(role.Asset.Account.LogoURL)
			c.CompanyDomain = derefString(role.Asset.Account.Website)
		}

		// Derive B2BOrgUID from the Asset's AccountId so callers can link this
		// contact to the B2BOrg entity without an extra resolver round-trip.
		if role.Asset.AccountID != "" {
			if orgUID, orgErr := sfuuid.Normalize18(role.Asset.AccountID); orgErr == nil {
				c.B2BOrgUID = orgUID
			}
		}

		// Populate ProjectSlug (and ProjectSFID) from the Projects__r relationship.
		// ProjectUID is resolved from the slug via project-service (NATS); see
		// MemberReader and backfill_runner for the resolution step.
		if role.Asset.Project != nil {
			c.ProjectSlug = derefString(role.Asset.Project.Slug)
			c.ProjectName = role.Asset.Project.Name
			c.ProjectLogoURL = derefString(role.Asset.Project.LogoURL)
			if role.Asset.Project.ID != "" {
				if projectSFID, normErr := sfuuid.Normalize18(role.Asset.Project.ID); normErr == nil {
					c.ProjectSFID = projectSFID
				}
			}
		}
	}

	// Inline Contact fields (Contact__r).
	if role.Contact != nil {
		c.FirstName = derefString(role.Contact.FirstName)
		c.LastName = derefString(role.Contact.LastName)
		c.Title = derefString(role.Contact.Title)

		// Resolve primary email: prefer Alternate_Email__c (via emailMap keyed
		// by Contact_Name__c), fall back to Contact__r.Email for contacts that
		// have no Alternate_Email__c record.
		if role.Contact.ID != "" {
			if email, ok := emailMap[role.Contact.ID]; ok {
				c.Email = email
			} else {
				c.Email = derefString(role.Contact.Email)
			}
		}
	} else if role.ContactID != nil && *role.ContactID != "" {
		// Defensive: Contact__r is nil but Contact__c (bare FK) is set.
		// This can occur if the referenced Contact is deleted or inaccessible.
		// Still attempt an email map lookup using the bare ID so that any
		// Alternate_Email__c record fetched via fetchPrimaryEmails is not lost.
		if email, ok := emailMap[*role.ContactID]; ok {
			c.Email = email
		}
	}

	// Timestamps.
	c.CreatedAt = parseSOQLTime(role.CreatedDate)
	c.UpdatedAt = parseSOQLTime(role.SystemModstamp)

	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}

	return c, nil
}

// IterKeyContacts iterates over all key contacts from Salesforce, applying an
// optional LastModifiedDate filter when since is provided. Calls fn for each
// page of converted records, performing per-page email batch lookups.
// Conversion errors are logged and skipped.
func (r *KeyContactRepo) IterKeyContacts(ctx context.Context, since, until *time.Time, fn func([]*model.KeyContact) error) error {
	const baseSOQL = `
SELECT
    Id, Asset__c, Contact__c, Role__c, Status__c,
    BoardMember__c, PrimaryContact__c,
    CreatedDate, SystemModstamp,
    Contact__r.Id, Contact__r.FirstName, Contact__r.LastName, Contact__r.Title, Contact__r.Email,
    Asset__r.Id, Asset__r.AccountId, Asset__r.Product2Id, Asset__r.Projects__c,
    Asset__r.Account.Id, Asset__r.Account.Name,
    Asset__r.Account.Logo_URL__c, Asset__r.Account.Website,
    Asset__r.Projects__r.Id, Asset__r.Projects__r.Name,
    Asset__r.Projects__r.Slug__c, Asset__r.Projects__r.Project_Logo__c
FROM Project_Role__c
WHERE IsDeleted = false`

	query := baseSOQL + lastModifiedWindowClause(since, until)

	// Drive the paging loop directly so each page's email batch lookup and
	// conversion happen before fn is called — no intermediate copy needed.
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("IterKeyContacts aborted by context: %w", err)
		}
		result, err := QueryPage[soqlProjectRole](ctx, r.client, query, token)
		if err != nil {
			return fmt.Errorf("fetching key contacts page (token=%q): %w", token, err)
		}

		contactIDs := collectProjectRoleContactIDs(result.Records)
		emailMap := make(map[string]string)
		if len(contactIDs) > 0 {
			if em, emailErr := r.fetchPrimaryEmails(ctx, contactIDs); emailErr == nil {
				emailMap = em
			} else {
				slog.WarnContext(ctx, "fetchPrimaryEmails failed on page; contacts will be indexed without primary email",
					"batch_size", len(contactIDs), "error", emailErr)
			}
		}

		contacts := make([]*model.KeyContact, 0, len(result.Records))
		for _, role := range result.Records {
			kc, convErr := convertSOQLToKeyContact(role, emailMap)
			if convErr != nil {
				slog.WarnContext(ctx, "skipping key contact due to conversion error", "error", convErr)
				continue
			}
			contacts = append(contacts, kc)
		}

		if err := fn(contacts); err != nil {
			return err
		}

		if result.NextPageToken == "" {
			break
		}
		token = result.NextPageToken
	}
	return nil
}

// collectProjectRoleContactIDs extracts unique non-empty contact IDs from a
// slice of SOQL Project_Role__c records.
func collectProjectRoleContactIDs(roles []soqlProjectRole) []string {
	seen := make(map[string]struct{}, len(roles))
	var ids []string

	for _, r := range roles {
		cid := ""
		if r.Contact != nil {
			cid = r.Contact.ID
		} else if r.ContactID != nil {
			cid = *r.ContactID
		}
		if cid == "" {
			continue
		}
		if _, ok := seen[cid]; ok {
			continue
		}
		seen[cid] = struct{}{}
		ids = append(ids, cid)
	}

	return ids
}
