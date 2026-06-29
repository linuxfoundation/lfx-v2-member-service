// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// soqlAccountID is a minimal projection used when only the Salesforce Id is needed.
type soqlAccountID struct {
	ID string `salesforce:"Id" json:"Id"`
}

// accountsSOQLBase is the SELECT and fixed WHERE base for Account search/list
// queries. The caller appends optional LIKE predicates and an ORDER BY clause
// before executing. Only Accounts that are not deleted and have at least one
// membership Asset are returned — the SOQL semi-join on Asset ensures we do
// not expose arbitrary Account records to the API.
const accountsSOQLBase = `
SELECT
    Id, Name, Logo_URL__c, Website,
    Account_Domain__c, Domain_Alias__c,
    Description, Phone, ParentId,
    Parent.Id, Parent.Name, Parent.Logo_URL__c,
    Industry, Sector__c, CrunchBase_URL__c,
    NumberOfEmployees, LF_Membership_Status__c, IsMember__c,
    CreatedDate, LastModifiedDate
FROM Account
WHERE IsDeleted = false
    AND Id IN (
        SELECT AccountId FROM Asset
        WHERE Product2.Family = 'Membership'
            AND IsDeleted = false
    )`

// AccountRepo handles Salesforce SOQL queries for Account (B2BOrg) records.
type AccountRepo struct {
	client *sf.Salesforce
}

// NewAccountRepo creates a new AccountRepo backed by the given Salesforce client.
func NewAccountRepo(client *sf.Salesforce) *AccountRepo {
	return &AccountRepo{client: client}
}

// FetchChildUIDsByParentUID returns the SFIDs of member-eligible Salesforce
// Accounts whose ParentId matches the SFID derived from parentUID (uid == SFID).
// Only accounts with at least one membership Asset are returned — matching the
// same gate used by accountsSOQLBase so the result contains only UIDs that exist
// in the index. Returns an empty slice (no error) when the parent has no
// member-eligible children.
func (r *AccountRepo) FetchChildUIDsByParentUID(ctx context.Context, parentUID string) ([]string, error) {
	parentSFID, err := sfuuid.Normalize18(parentUID)
	if err != nil {
		return nil, fmt.Errorf("normalizing parent uid %q to sfid: %w", parentUID, err)
	}
	slog.DebugContext(ctx, "fetching child account SFIDs from Salesforce", "parent_sfid", parentSFID)

	query := "SELECT Id FROM Account WHERE ParentId = " + quoteSOQL(parentSFID) +
		" AND IsDeleted = false" +
		" AND Id IN (SELECT AccountId FROM Asset WHERE Product2.Family = 'Membership' AND IsDeleted = false)"
	records, _, err := QueryAllPages[soqlAccountID](ctx, r.client, query, "")
	if err != nil {
		return nil, fmt.Errorf("fetching children of parent %s: %w", parentSFID, err)
	}

	uids := make([]string, 0, len(records))
	for _, rec := range records {
		uid, convErr := sfuuid.Normalize18(rec.ID)
		if convErr != nil {
			slog.WarnContext(ctx, "child account SFID could not be normalized, skipping",
				"parent_sfid", parentSFID, "child_sfid", rec.ID, "error", convErr)
			continue
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

// FetchAccountBySFID fetches a single Account record from Salesforce by its
// Salesforce Id. Returns nil, nil when no matching record is found.
func (r *AccountRepo) FetchAccountBySFID(ctx context.Context, sfid string) (*model.B2BOrg, error) {
	slog.DebugContext(ctx, "fetching account by SFID from Salesforce", "sfid", sfid)
	query := accountsSOQLBase + "\n    AND Id = " + quoteSOQL(sfid) + "\nLIMIT 1"
	sfResult, err := QueryPage[soqlAccount](ctx, r.client, query, "")
	if err != nil {
		return nil, fmt.Errorf("fetching account by SFID %s: %w", sfid, err)
	}
	if len(sfResult.Records) == 0 {
		return nil, nil
	}
	return convertSOQLToB2BOrg(ctx, sfResult.Records[0])
}

// FetchAccountsBySFIDs fetches multiple Account records from Salesforce by
// their Salesforce Ids in a single batched SOQL query. Records absent from
// the result set have been soft-deleted or no longer hold a membership Asset
// (the membership semi-join in accountsSOQLBase excludes them). Returns an
// empty slice when none are found.
func (r *AccountRepo) FetchAccountsBySFIDs(ctx context.Context, sfids []string) ([]*model.B2BOrg, []string, error) {
	if len(sfids) == 0 {
		return []*model.B2BOrg{}, nil, nil
	}
	slog.DebugContext(ctx, "fetching accounts by SFIDs from Salesforce (batch)", "count", len(sfids))

	const batchSize = 200
	all := make([]*model.B2BOrg, 0, len(sfids))
	var conversionErrorSFIDs []string
	for start := 0; start < len(sfids); start += batchSize {
		end := start + batchSize
		if end > len(sfids) {
			end = len(sfids)
		}
		chunk := sfids[start:end]

		query := accountsSOQLBase + "\n    AND Id IN (" + buildSOQLInClause(chunk) + ")"
		sfResult, err := QueryPage[soqlAccount](ctx, r.client, query, "")
		if err != nil {
			return nil, nil, fmt.Errorf("batch fetching accounts (chunk %d-%d): %w", start, end, err)
		}
		for _, acc := range sfResult.Records {
			org, convErr := convertSOQLToB2BOrg(ctx, acc)
			if convErr != nil {
				slog.WarnContext(ctx, "skipping account with invalid SFID in batch",
					"sfid", acc.ID, "error", convErr)
				conversionErrorSFIDs = append(conversionErrorSFIDs, acc.ID)
				continue
			}
			all = append(all, org)
		}
	}
	return all, conversionErrorSFIDs, nil
}

// soqlParentChild is a minimal projection for the batched child-list query.
// No aggregate alias — uses the same plain json.Unmarshal path as soqlAccountID.
type soqlParentChild struct {
	ParentID string `json:"ParentId"`
	ID       string `json:"Id"`
}

// normalizeParentUIDs converts caller UIDs to 18-char SFIDs and builds a
// reverse sfid→callerUID map. Invalid UIDs are logged and skipped.
func normalizeParentUIDs(ctx context.Context, parentUIDs []string) (sfids []string, sfidToUID map[string]string) {
	sfids = make([]string, 0, len(parentUIDs))
	sfidToUID = make(map[string]string, len(parentUIDs))
	for _, uid := range parentUIDs {
		sfid, err := sfuuid.Normalize18(uid)
		if err != nil {
			slog.WarnContext(ctx, "parent UID could not be normalized, skipping",
				"uid", uid, "error", err)
			continue
		}
		sfids = append(sfids, sfid)
		sfidToUID[sfid] = uid
	}
	return sfids, sfidToUID
}

// fetchChildUIDsByParents issues one chunked SOQL query per <=200 parents and
// returns child UIDs grouped by caller-supplied parent UID. Chunking mirrors
// FetchAccountsBySFIDs (account_repo.go:117). All normalization and the
// membership Asset semi-join live here; callers see only domain UIDs.
func (r *AccountRepo) fetchChildUIDsByParents(
	ctx context.Context, parentUIDs []string,
) (map[string][]string, error) {
	if len(parentUIDs) == 0 {
		return map[string][]string{}, nil
	}

	slog.DebugContext(ctx, "batch-fetching child UIDs from Salesforce", "parent_count", len(parentUIDs))

	sfids, sfidToUID := normalizeParentUIDs(ctx, parentUIDs)
	if len(sfids) == 0 {
		return map[string][]string{}, nil
	}

	out := make(map[string][]string)

	const chunkSize = 200
	for start := 0; start < len(sfids); start += chunkSize {
		chunk := sfids[start:min(start+chunkSize, len(sfids))]

		query := "SELECT ParentId, Id FROM Account" +
			" WHERE ParentId IN (" + buildSOQLInClause(chunk) + ")" +
			" AND IsDeleted = false" +
			" AND Id IN (SELECT AccountId FROM Asset" +
			" WHERE Product2.Family = 'Membership' AND IsDeleted = false)"

		records, _, err := QueryAllPages[soqlParentChild](ctx, r.client, query, "")
		if err != nil {
			return out, fmt.Errorf("fetching child UIDs for %d parents (chunk starting at %d): %w",
				len(chunk), start, err)
		}

		for _, rec := range records {
			pid, pidErr := sfuuid.Normalize18(rec.ParentID)
			if pidErr != nil {
				continue
			}
			callerUID, ok := sfidToUID[pid]
			if !ok {
				continue
			}
			childUID, convErr := sfuuid.Normalize18(rec.ID)
			if convErr != nil {
				slog.WarnContext(ctx, "child SFID could not be normalized, skipping",
					"parent_uid", callerUID, "child_sfid", rec.ID, "error", convErr)
				continue
			}
			out[callerUID] = append(out[callerUID], childUID)
		}
	}
	return out, nil
}

// FetchChildUIDsByParentUIDs returns child UIDs grouped by parent UID for a
// batch of parents. Used by the backfill runner to compute is_parent and FGA
// parent tuples in one query instead of N per-org calls.
func (r *AccountRepo) FetchChildUIDsByParentUIDs(
	ctx context.Context, parentUIDs []string,
) (map[string][]string, error) {
	return r.fetchChildUIDsByParents(ctx, parentUIDs)
}

// Ensure AccountRepo satisfies the port at compile time.
var _ port.AccountBatchReader = (*AccountRepo)(nil)

// convertSOQLToB2BOrg converts a Salesforce Account SOQL result to the domain
// B2BOrg model.
func convertSOQLToB2BOrg(ctx context.Context, acc soqlAccount) (*model.B2BOrg, error) {
	uid, err := sfuuid.Normalize18(acc.ID)
	if err != nil {
		return nil, fmt.Errorf("normalizing account SFID %q: %w", acc.ID, err)
	}

	org := &model.B2BOrg{
		UID:  uid,
		SFID: acc.ID,
		Name: acc.Name,
	}

	// Normalize Website: ensure it has a scheme so that url.Parse produces a
	// host. If the value cannot be parsed at all, omit it with a warning.
	if raw := derefString(acc.Website); raw != "" {
		if u, parseErr := url.Parse(raw); parseErr == nil {
			if u.Scheme == "" {
				u.Scheme = "http"
			}
			org.Website = u.String()
		} else {
			slog.WarnContext(ctx, "account website could not be parsed, omitting",
				"sfid", acc.ID,
				"raw_value", raw,
			)
		}
	}

	// Normalize Account_Domain__c into PrimaryDomain.
	if raw := derefString(acc.PrimaryDomain); raw != "" {
		if normalized, ok := normalizeDomain(raw); ok {
			org.PrimaryDomain = normalized
		} else {
			slog.WarnContext(ctx, "account primary domain does not look like a valid domain, omitting",
				"sfid", acc.ID,
				"raw_value", raw,
			)
		}
	}

	// Normalize Domain_Alias__c into DomainAliases using the shared parser
	// that also handles account-merge CRLF artifacts.
	org.DomainAliases = parseDomainAliases(ctx, acc.ID, derefString(acc.DomainAlias))

	org.LogoURL = derefString(acc.LogoURL)
	org.Description = derefString(acc.Description)
	org.Phone = derefString(acc.Phone)
	org.Industry = derefString(acc.Industry)
	org.Sector = derefString(acc.Sector)
	org.CrunchBaseURL = acc.CrunchBaseURL
	org.NumberOfEmployees = acc.NumberOfEmployees
	org.Status = derefString(acc.Status)
	if acc.IsMember != nil {
		org.IsMember = *acc.IsMember
	}

	if parentSFID := derefString(acc.ParentID); parentSFID != "" {
		parentUID, convErr := sfuuid.Normalize18(parentSFID)
		if convErr != nil {
			slog.WarnContext(ctx, "account parent SFID could not be normalized, omitting",
				"sfid", acc.ID, "parent_sfid", parentSFID)
		} else {
			org.ParentUID = parentUID
			if acc.Parent != nil {
				org.ParentDetail = &model.B2BOrgParentDetail{
					UID:     parentUID,
					Name:    acc.Parent.Name,
					LogoURL: acc.Parent.LogoURL,
				}
			}
		}
	}

	org.CreatedAt = parseSOQLTime(acc.CreatedDate)
	org.UpdatedAt = parseSOQLTime(acc.LastModifiedDate)

	if org.UpdatedAt.IsZero() {
		org.UpdatedAt = org.CreatedAt
	}
	if org.CreatedAt.IsZero() {
		org.CreatedAt = time.Now()
	}

	return org, nil
}

// IterB2BOrgs iterates over B2B orgs (Accounts) from Salesforce, applying an
// optional LastModifiedDate filter when since is provided. Calls fn for each
// page of converted records. Conversion errors are logged and skipped.
func (r *AccountRepo) IterB2BOrgs(ctx context.Context, since *time.Time, fn func([]*model.B2BOrg) error) error {
	query := accountsSOQLBase
	if since != nil {
		query += "\n    AND LastModifiedDate >= " + soqlDateTime(*since)
	}
	return IterPages[soqlAccount, *model.B2BOrg](ctx, r.client, query, func(acc soqlAccount) (*model.B2BOrg, error) {
		return convertSOQLToB2BOrg(ctx, acc)
	}, fn)
}

// normalizeDomain validates and returns the host portion of a bare domain
// string. Values containing "/" or " " are rejected immediately. Bare domains
// (no scheme) are handled by reading u.Path, which is where url.Parse places
// them when no scheme is present.
func normalizeDomain(s string) (string, bool) {
	if strings.ContainsAny(s, "/ ") {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	host := u.Host
	if host == "" {
		// Bare domain: url.Parse puts it in Path when there is no scheme.
		host = u.Path
	}
	if host == "" {
		return "", false
	}
	// Reject single-label tokens (no dot in host). Real domain aliases always
	// carry a TLD; dot-less values are typically merge-artifact remnants.
	if !strings.Contains(host, ".") {
		return "", false
	}
	return host, true
}
