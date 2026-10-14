// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// seqQueryTransport returns a fixed /limits response for sf.Init, then returns
// SOQL query responses from a queue (one per /query call, in order). When the
// queue is exhausted it returns an empty-records response so tests don't have
// to account for trailing calls.
type seqQueryTransport struct {
	mu            sync.Mutex
	responses     []string // JSON response bodies, consumed in order
	queryStatuses []int    // optional HTTP status codes aligned with responses
	pos           int
	queryCalls    int
	queries       []string // decoded q parameter of each initial /query call
}

func (t *seqQueryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.Contains(req.URL.Path, "/limits") {
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}
	// All other calls are SOQL /query requests.
	t.queryCalls++
	if q := req.URL.Query().Get("q"); q != "" {
		t.queries = append(t.queries, q)
	}
	body := `{"totalSize":0,"done":true,"records":[]}`
	status := http.StatusOK
	if t.pos < len(t.responses) {
		body = t.responses[t.pos]
		if t.pos < len(t.queryStatuses) && t.queryStatuses[t.pos] != 0 {
			status = t.queryStatuses[t.pos]
		}
		t.pos++
	}
	return fakeResponse(status, body, nil), nil
}

// companyAccountFields is the eligibility projection of a Company Account
// without a membership, the shape Salesforce returns for the Assets subquery
// when it matches no row.
const companyAccountFields = `"RecordType":{"DeveloperName":"Company_Account"},"Assets":null`

// soqlParentChildJSON builds a single-record SOQL response with one ParentId/Id
// pair whose child is an eligible Company Account.
func soqlParentChildJSON(parentID, childID string) string {
	return fmt.Sprintf(
		`{"totalSize":1,"done":true,"records":[{"ParentId":%q,"Id":%q,`+companyAccountFields+`}]}`,
		parentID, childID,
	)
}

// batchParentSFID generates a valid 18-char Salesforce ID for test index i.
// Uses the pattern "001" + 12-digit zero-padded index, then appends the
// Salesforce checksum suffix via sfuuid.Salesforce15To18.
func batchParentSFID(i int) string {
	id15 := fmt.Sprintf("001%012d", i)
	sfid, err := sfuuid.Salesforce15To18(id15)
	if err != nil {
		panic(fmt.Sprintf("batchParentSFID(%d): %v", i, err))
	}
	return sfid
}

// TestAccountRepo_FetchChildUIDsByParentUIDs_EmptyInput verifies the empty-input
// short-circuit: no SF call and an empty map returned immediately.
func TestAccountRepo_FetchChildUIDsByParentUIDs_EmptyInput(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{}
	sfClient := fakeSalesforce(t, tr)
	repo := NewAccountRepo(sfClient)

	got, err := repo.FetchChildUIDsByParentUIDs(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, got, "nil input must return empty map")
	assert.Equal(t, 0, tr.queryCalls, "no SF call expected for nil input")

	got2, err := repo.FetchChildUIDsByParentUIDs(context.Background(), []string{})
	require.NoError(t, err)
	assert.Empty(t, got2, "empty slice input must return empty map")
	assert.Equal(t, 0, tr.queryCalls, "no SF call expected for empty input")
}

// soqlMultiChildJSON builds a two-record SOQL response where both children share
// the same ParentId. Used to verify append-not-overwrite semantics.
func soqlMultiChildJSON(parentID, childID1, childID2 string) string {
	return fmt.Sprintf(
		`{"totalSize":2,"done":true,"records":[{"ParentId":%q,"Id":%q,`+companyAccountFields+`},`+
			`{"ParentId":%q,"Id":%q,`+companyAccountFields+`}]}`,
		parentID, childID1, parentID, childID2,
	)
}

// TestAccountRepo_FetchChildUIDsByParentUIDs_InvalidParentSFIDSkipped verifies
// that a parent UID that fails normalization is silently skipped while valid
// parents in the same call are still fetched and returned.
func TestAccountRepo_FetchChildUIDsByParentUIDs_InvalidParentSFIDSkipped(t *testing.T) {
	t.Parallel()

	validParent := batchParentSFID(3000)
	child := batchParentSFID(3001)
	const invalidParent = "not-a-valid-sfid"

	tr := &seqQueryTransport{
		responses: []string{soqlParentChildJSON(validParent, child)},
	}
	sfClient := fakeSalesforce(t, tr)
	repo := NewAccountRepo(sfClient)

	got, err := repo.FetchChildUIDsByParentUIDs(context.Background(), []string{validParent, invalidParent})
	require.NoError(t, err)

	assert.Equal(t, 1, tr.queryCalls, "one SF call — invalid parent excluded from IN clause")
	childNorm, normErr := sfuuid.Normalize18(child)
	require.NoError(t, normErr)
	assert.Equal(t, []string{childNorm}, got[validParent], "valid parent must still have its child")
	_, hasInvalid := got[invalidParent]
	assert.False(t, hasInvalid, "invalid parent must not appear in result")
}

// TestAccountRepo_FetchChildUIDsByParentUIDs_InvalidChildSFIDSkipped verifies
// that a child record whose Id cannot be normalized is silently dropped while
// sibling children of the same parent are still returned.
func TestAccountRepo_FetchChildUIDsByParentUIDs_InvalidChildSFIDSkipped(t *testing.T) {
	t.Parallel()

	parent := batchParentSFID(4000)
	validChild := batchParentSFID(4001)

	// Two-record response: one valid child Id and one malformed Id.
	response := fmt.Sprintf(
		`{"totalSize":2,"done":true,"records":[{"ParentId":%q,"Id":%q,`+companyAccountFields+`},`+
			`{"ParentId":%q,"Id":"not-valid-sfid",`+companyAccountFields+`}]}`,
		parent, validChild, parent,
	)

	tr := &seqQueryTransport{responses: []string{response}}
	sfClient := fakeSalesforce(t, tr)
	repo := NewAccountRepo(sfClient)

	got, err := repo.FetchChildUIDsByParentUIDs(context.Background(), []string{parent})
	require.NoError(t, err)

	validChildNorm, normErr := sfuuid.Normalize18(validChild)
	require.NoError(t, normErr)
	require.Len(t, got[parent], 1, "only the valid child must survive; invalid child dropped")
	assert.Equal(t, validChildNorm, got[parent][0])
}

// TestAccountRepo_FetchChildUIDsByParentUIDs_OrphanedParentIDSkipped verifies
// that a SOQL result record whose ParentId was not in the requested set is
// silently dropped and does not appear in the output map.
func TestAccountRepo_FetchChildUIDsByParentUIDs_OrphanedParentIDSkipped(t *testing.T) {
	t.Parallel()

	requestedParent := batchParentSFID(5000)
	orphanParent := batchParentSFID(5001) // not in the input slice
	child := batchParentSFID(5002)

	// SOQL response contains a child whose ParentId is the orphan — not requested.
	tr := &seqQueryTransport{
		responses: []string{soqlParentChildJSON(orphanParent, child)},
	}
	sfClient := fakeSalesforce(t, tr)
	repo := NewAccountRepo(sfClient)

	got, err := repo.FetchChildUIDsByParentUIDs(context.Background(), []string{requestedParent})
	require.NoError(t, err)

	assert.Equal(t, 1, tr.queryCalls)
	assert.Empty(t, got, "orphaned child must be dropped; result map must be empty")
}

// TestAccountRepo_FetchChildUIDsByParentUIDs_MultipleChildrenPerParent verifies
// that when Salesforce returns two children for the same parent in one response,
// both are appended (not overwritten) in the result map.
func TestAccountRepo_FetchChildUIDsByParentUIDs_MultipleChildrenPerParent(t *testing.T) {
	t.Parallel()

	parent := batchParentSFID(6000)
	child1 := batchParentSFID(6001)
	child2 := batchParentSFID(6002)

	tr := &seqQueryTransport{
		responses: []string{soqlMultiChildJSON(parent, child1, child2)},
	}
	sfClient := fakeSalesforce(t, tr)
	repo := NewAccountRepo(sfClient)

	got, err := repo.FetchChildUIDsByParentUIDs(context.Background(), []string{parent})
	require.NoError(t, err)

	child1Norm, err1 := sfuuid.Normalize18(child1)
	child2Norm, err2 := sfuuid.Normalize18(child2)
	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.Len(t, got[parent], 2, "both children must be appended, not overwritten")
	assert.ElementsMatch(t, []string{child1Norm, child2Norm}, got[parent])
}

// TestAccountRepo_FetchChildUIDsByParentUIDs_ChunkBoundary feeds 201 parent UIDs
// (chunk size is 200) and verifies:
//   - QueryAllPages is invoked twice (one per chunk).
//   - Results from both chunks are merged into the returned map.
//   - A parent in the second chunk (index 200) is present alongside one from
//     the first chunk (index 0).
func TestAccountRepo_FetchChildUIDsByParentUIDs_ChunkBoundary(t *testing.T) {
	t.Parallel()

	const total = 201

	// Build 201 parent SFIDs.
	parents := make([]string, total)
	for i := range parents {
		parents[i] = batchParentSFID(i)
	}

	// One child SFID for parent[0] (in chunk 1) and one for parent[200] (chunk 2).
	childOfFirst := batchParentSFID(1000) // distinct SFID for child
	childOfLast := batchParentSFID(1001)

	// Chunk 1 response: parent[0] has one child.
	chunk1Response := soqlParentChildJSON(parents[0], childOfFirst)
	// Chunk 2 response: parent[200] has one child.
	chunk2Response := soqlParentChildJSON(parents[200], childOfLast)

	tr := &seqQueryTransport{
		responses: []string{chunk1Response, chunk2Response},
	}
	sfClient := fakeSalesforce(t, tr)
	repo := NewAccountRepo(sfClient)

	got, err := repo.FetchChildUIDsByParentUIDs(context.Background(), parents)
	require.NoError(t, err)

	// Two SF calls — one per chunk.
	assert.Equal(t, 2, tr.queryCalls, "expected exactly two SOQL calls for 201 parents (chunks: 200+1)")

	// Results from both chunks are merged.
	assert.Len(t, got, 2, "merged map must contain one entry per parent that has children")

	// parent[0] (from chunk 1) maps to its child.
	childOfFirstNorm, err := sfuuid.Normalize18(childOfFirst)
	require.NoError(t, err)
	assert.Equal(t, []string{childOfFirstNorm}, got[parents[0]],
		"parent[0] from chunk 1 must be present in merged result")

	// parent[200] (from chunk 2) maps to its child.
	childOfLastNorm, err := sfuuid.Normalize18(childOfLast)
	require.NoError(t, err)
	assert.Equal(t, []string{childOfLastNorm}, got[parents[200]],
		"parent[200] from chunk 2 must be present in merged result")
}

// eligibilityFields renders the RecordType and Assets projection in the shape
// Salesforce returns: null record type for an empty name, "Assets": null when
// the membership subquery matches no row.
func eligibilityFields(recordType string, member bool) string {
	rt := `null`
	if recordType != "" {
		rt = fmt.Sprintf(`{"DeveloperName":%q,"attributes":{"type":"RecordType"}}`, recordType)
	}
	assets := `null`
	if member {
		assets = `{"totalSize":1,"done":true,"records":[{"Id":"02i000000000001AAA","attributes":{"type":"Asset"}}]}`
	}
	return `"RecordType":` + rt + `,"Assets":` + assets
}

// accountRecordJSON builds one soqlAccount record.
func accountRecordJSON(id, recordType string, member bool) string {
	return fmt.Sprintf(`{"Id":%q,"Name":"Org %s",`, id, id) + eligibilityFields(recordType, member) + `}`
}

// soqlPageJSON wraps records in a SOQL response. A non-empty next makes the
// page not done, with next as its nextRecordsUrl.
func soqlPageJSON(next string, records ...string) string {
	if next == "" {
		return fmt.Sprintf(`{"totalSize":%d,"done":true,"records":[%s]}`, len(records), strings.Join(records, ","))
	}
	return fmt.Sprintf(`{"totalSize":%d,"done":false,"nextRecordsUrl":%q,"records":[%s]}`,
		len(records), next, strings.Join(records, ","))
}

func orgUIDs(orgs []*model.B2BOrg) []string {
	uids := make([]string, 0, len(orgs))
	for _, o := range orgs {
		uids = append(uids, o.UID)
	}
	return uids
}

func TestSOQLEligibility_Decode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		record   string
		eligible bool
	}{
		{"no record type, no membership", accountRecordJSON(batchParentSFID(7000), "", false), false},
		{"no record type, membership", accountRecordJSON(batchParentSFID(7001), "", true), true},
		{"company, Assets null", accountRecordJSON(batchParentSFID(7002), "Company_Account", false), true},
		{"partner, Assets null", accountRecordJSON(batchParentSFID(7003), "Partner_Restricted", false), false},
		{"partner, one Assets record", accountRecordJSON(batchParentSFID(7004), "Partner_Restricted", true), true},
	}
	for _, tt := range tests {
		var acc soqlAccount
		require.NoError(t, json.Unmarshal([]byte(tt.record), &acc), tt.name)
		assert.Equal(t, tt.eligible, acc.eligible(), tt.name)
	}
}

func TestAccountRepo_FetchAccountsBySFIDs_Eligibility(t *testing.T) {
	t.Parallel()

	company := batchParentSFID(7100)
	partnerNoMembership := batchParentSFID(7101)
	partnerMember := batchParentSFID(7102)
	projectMember := batchParentSFID(7103)
	personMember := batchParentSFID(7104)

	tr := &seqQueryTransport{responses: []string{soqlPageJSON("",
		accountRecordJSON(company, "Company_Account", false),
		accountRecordJSON(partnerNoMembership, "Partner_Restricted", false),
		accountRecordJSON(partnerMember, "Partner_Restricted", true),
		accountRecordJSON(projectMember, "Project_Account", true),
		accountRecordJSON(personMember, "PersonAccount", true),
	)}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	orgs, convErrs, err := repo.FetchAccountsBySFIDs(context.Background(),
		[]string{company, partnerNoMembership, partnerMember, projectMember, personMember})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{company, partnerMember, projectMember, personMember}, orgUIDs(orgs))
	assert.Empty(t, convErrs, "an ineligible Account is absent, not a conversion error")
	require.Len(t, tr.queries, 1)
	assert.NotContains(t, tr.queries[0], "Id IN (SELECT AccountId FROM Asset")
	assert.Contains(t, tr.queries[0], "RecordType.DeveloperName")
}

func TestAccountRepo_FetchAccountsBySFIDs_ReadsEveryPage(t *testing.T) {
	t.Parallel()

	first := batchParentSFID(7200)
	second := batchParentSFID(7201)

	tr := &seqQueryTransport{responses: []string{
		soqlPageJSON("/services/data/v63.0/query/01gNEXT-2000", accountRecordJSON(first, "Company_Account", false)),
		soqlPageJSON("", accountRecordJSON(second, "Company_Account", false)),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	orgs, _, err := repo.FetchAccountsBySFIDs(context.Background(), []string{first, second})
	require.NoError(t, err)

	assert.Equal(t, 2, tr.queryCalls, "the second page must be fetched")
	assert.ElementsMatch(t, []string{first, second}, orgUIDs(orgs))
}

// TestAccountRepo_FetchAccountsBySFIDs_ChunkBoundary: 201 SFIDs span two
// 200-id chunks; both are queried and merged, and a failing second chunk
// returns an error with no partial result, so callers never read the
// missing half as absent.
func TestAccountRepo_FetchAccountsBySFIDs_ChunkBoundary(t *testing.T) {
	t.Parallel()

	sfids := make([]string, 201)
	for i := range sfids {
		sfids[i] = batchParentSFID(8000 + i)
	}
	first := soqlPageJSON("", accountRecordJSON(sfids[0], model.RecordTypeCompanyAccount, false))
	last := soqlPageJSON("", accountRecordJSON(sfids[200], model.RecordTypeCompanyAccount, false))

	t.Run("both chunks are merged", func(t *testing.T) {
		t.Parallel()
		tr := &seqQueryTransport{responses: []string{first, last}}
		orgs, convErrs, err := NewAccountRepo(fakeSalesforce(t, tr)).FetchAccountsBySFIDs(context.Background(), sfids)
		require.NoError(t, err)
		assert.Empty(t, convErrs)
		assert.Equal(t, 2, tr.queryCalls)
		assert.ElementsMatch(t, []string{sfids[0], sfids[200]}, orgUIDs(orgs))
	})

	t.Run("a failing second chunk returns no partial result", func(t *testing.T) {
		t.Parallel()
		tr := &seqQueryTransport{
			responses:     []string{first, `[{"message":"temporarily unavailable","errorCode":"SERVER_UNAVAILABLE"}]`},
			queryStatuses: []int{200, 500},
		}
		orgs, convErrs, err := NewAccountRepo(fakeSalesforce(t, tr)).FetchAccountsBySFIDs(context.Background(), sfids)
		require.Error(t, err)
		assert.Nil(t, orgs)
		assert.Nil(t, convErrs)
		assert.Equal(t, 2, tr.queryCalls)
	})
}

// TestAccountRepo_IterB2BOrgs_Eligibility swaps the default logger, so it must
// not run in parallel.
func TestAccountRepo_IterB2BOrgs_Eligibility(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	company := batchParentSFID(7300)
	partnerNoMembership := batchParentSFID(7301)
	personNoMembership := batchParentSFID(7302)
	partnerMember := batchParentSFID(7303)
	projectMember := batchParentSFID(7304)
	personMember := batchParentSFID(7305)

	tr := &seqQueryTransport{responses: []string{
		soqlPageJSON("/services/data/v63.0/query/01gNEXT-2000",
			accountRecordJSON(company, "Company_Account", false),
			accountRecordJSON(partnerNoMembership, "Partner_Restricted", false),
			accountRecordJSON(personNoMembership, "PersonAccount", false),
		),
		soqlPageJSON("",
			accountRecordJSON(partnerMember, "Partner_Restricted", true),
			accountRecordJSON(projectMember, "Project_Account", true),
			accountRecordJSON(personMember, "PersonAccount", true),
		),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	var pages [][]string
	err := repo.IterB2BOrgs(context.Background(), nil, nil, func(orgs []*model.B2BOrg) error {
		for _, o := range orgs {
			require.NotNil(t, o, "fn must never receive a nil org")
		}
		pages = append(pages, orgUIDs(orgs))
		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, [][]string{{company}, {partnerMember, projectMember, personMember}}, pages)
	assert.NotContains(t, logs.String(), "skipping record due to conversion error")
}

func TestAccountRepo_FetchAccountEligibility(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(7500)
	tr := &seqQueryTransport{responses: []string{
		soqlPageJSON("", `{"Id":"`+sfid+`",`+eligibilityFields("Partner_Restricted", false)+`}`),
		soqlPageJSON(""),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	got, err := repo.fetchAccountEligibility(context.Background(), sfid)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Partner_Restricted", got.RecordType.DeveloperName)
	assert.False(t, got.eligible())
	assert.Contains(t, tr.queries[0], "IsDeleted = false")

	missing, err := repo.fetchAccountEligibility(context.Background(), sfid)
	require.NoError(t, err)
	assert.Nil(t, missing, "no row means no non-deleted Account")
}

func TestAccountRepo_IsB2BOrgEligible(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(7600)
	tr := &seqQueryTransport{responses: []string{
		soqlPageJSON("", `{"Id":"`+sfid+`",`+eligibilityFields("Company_Account", false)+`}`),
		soqlPageJSON("", `{"Id":"`+sfid+`",`+eligibilityFields("Project_Account", false)+`}`),
		soqlPageJSON(""),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	for _, want := range []bool{true, false, false} {
		got, err := repo.IsB2BOrgEligible(context.Background(), sfid)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}

	_, err := repo.IsB2BOrgEligible(context.Background(), "not-a-valid-sfid")
	assert.Error(t, err)
}

func TestAccountRepo_ChildLists_Eligibility(t *testing.T) {
	t.Parallel()

	parent := batchParentSFID(7400)
	companyChild := batchParentSFID(7401)
	partnerChild := batchParentSFID(7402)
	partnerMemberChild := batchParentSFID(7403)

	singleResp := soqlPageJSON("",
		`{"Id":"`+companyChild+`",`+eligibilityFields("Company_Account", false)+`}`,
		`{"Id":"`+partnerChild+`",`+eligibilityFields("Partner_Restricted", false)+`}`,
		`{"Id":"`+partnerMemberChild+`",`+eligibilityFields("Partner_Restricted", true)+`}`,
	)
	batchResp := soqlPageJSON("",
		`{"ParentId":"`+parent+`","Id":"`+companyChild+`",`+eligibilityFields("Company_Account", false)+`}`,
		`{"ParentId":"`+parent+`","Id":"`+partnerChild+`",`+eligibilityFields("Partner_Restricted", false)+`}`,
		`{"ParentId":"`+parent+`","Id":"`+partnerMemberChild+`",`+eligibilityFields("Partner_Restricted", true)+`}`,
	)
	tr := &seqQueryTransport{responses: []string{singleResp, batchResp}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	// The child list replaces the parent's whole FGA child set, so an
	// ineligible child left in it would be granted a parent tuple.
	single, err := repo.FetchChildUIDsByParentUID(context.Background(), parent)
	require.NoError(t, err)
	assert.Equal(t, []string{companyChild, partnerMemberChild}, single)

	batch, err := repo.FetchChildUIDsByParentUIDs(context.Background(), []string{parent})
	require.NoError(t, err)
	assert.Equal(t, []string{companyChild, partnerMemberChild}, batch[parent])

	require.Len(t, tr.queries, 2)
	for _, q := range tr.queries {
		assert.NotContains(t, q, "Id IN (SELECT AccountId FROM Asset")
		assert.Contains(t, q, "RecordType.DeveloperName")
	}
}
