// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// seqQueryTransport returns a fixed /limits response for sf.Init, then returns
// SOQL query responses from a queue (one per /query call, in order). When the
// queue is exhausted it returns an empty-records response so tests don't have
// to account for trailing calls.
type seqQueryTransport struct {
	mu         sync.Mutex
	responses  []string // JSON response bodies, consumed in order
	pos        int
	queryCalls int
}

func (t *seqQueryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.Contains(req.URL.Path, "/limits") {
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}
	// All other calls are SOQL /query requests.
	t.queryCalls++
	body := `{"totalSize":0,"done":true,"records":[]}`
	if t.pos < len(t.responses) {
		body = t.responses[t.pos]
		t.pos++
	}
	return fakeResponse(http.StatusOK, body, nil), nil
}

// soqlParentChildJSON builds a single-record SOQL response with one ParentId/Id pair.
func soqlParentChildJSON(parentID, childID string) string {
	return fmt.Sprintf(
		`{"totalSize":1,"done":true,"records":[{"ParentId":%q,"Id":%q}]}`,
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
		`{"totalSize":2,"done":true,"records":[{"ParentId":%q,"Id":%q},{"ParentId":%q,"Id":%q}]}`,
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
		`{"totalSize":2,"done":true,"records":[{"ParentId":%q,"Id":%q},{"ParentId":%q,"Id":"not-valid-sfid"}]}`,
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
