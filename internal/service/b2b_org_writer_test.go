// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testB2BOrgUID = "00000000-0000-0000-0000-000000000011"

// ── Helpers ────────────────────────────────────────────────────────────────

// seededOrgWriter returns pre-seeded orgs for CreateB2BOrg and UpdateB2BOrg.
type seededOrgWriter struct {
	createOrg *model.B2BOrg
	updateOrg *model.B2BOrg
}

func (w *seededOrgWriter) CreateB2BOrg(_ context.Context, _ string, _ model.B2BOrgInput) (*model.B2BOrg, error) {
	return w.createOrg, nil
}

func (w *seededOrgWriter) UpdateB2BOrg(_ context.Context, _ string, _ model.B2BOrgInput) (*model.B2BOrg, error) {
	return w.updateOrg, nil
}

// seededOrgReader returns a pre-seeded org and optional per-parent child UID lists.
// Used only for B2BOrgWriter tests; org_settings_writer_test.go has seedB2BOrgReader.
type seededOrgReader struct {
	org      *model.B2BOrg
	children map[string][]string // parentUID → childUIDs
}

func (r *seededOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	if r.org != nil {
		return r.org, nil
	}
	return nil, pkgerrors.NewNotFound("b2b org not found")
}

func (r *seededOrgReader) FetchChildUIDsByParentUID(_ context.Context, parentUID string) ([]string, error) {
	if r.children != nil {
		if uids, ok := r.children[parentUID]; ok {
			return uids, nil
		}
	}
	return nil, nil
}

func (r *seededOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// capturingPublisher captures published indexer messages to inspect payload contents.
type capturingPublisher struct {
	mu               sync.Mutex
	indexerMessages  []any // captured message payloads
	indexerCallCount int
	accessCallCount  int
}

func (p *capturingPublisher) Indexer(_ context.Context, _ string, msg any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.indexerMessages = append(p.indexerMessages, msg)
	p.indexerCallCount++
	return nil
}

func (p *capturingPublisher) Access(_ context.Context, _ string, _ any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accessCallCount++
	return nil
}

func (p *capturingPublisher) getIndexerMessages() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]any, len(p.indexerMessages))
	copy(out, p.indexerMessages)
	return out
}

func newB2BOrgWriter(orgReader port.B2BOrgReader, orgWriter port.B2BOrgWriter, pub port.MemberPublisher, globalOrgAdminTeamUID string) svc.B2BOrgWriter {
	return svc.NewB2BOrgWriter(
		svc.WithB2BOrgReader(orgReader),
		svc.WithB2BOrgWriter(orgWriter),
		svc.WithB2BOrgPublisher(pub),
		svc.WithGlobalOrgAdminTeamUID(globalOrgAdminTeamUID),
	)
}

func mustEtag(t *testing.T, v any) string {
	t.Helper()
	val, err := etag.LFXEtag(v)
	require.NoError(t, err)
	return val
}

// ── Create tests ──────────────────────────────────────────────────────────

func TestB2BOrgWriter_Create_IndexerBeforeAccess(t *testing.T) {
	org := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	pub := &trackingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{org: org},
		&seededOrgWriter{createOrg: org},
		pub,
		"admin-team-uid",
	)

	_, err := w.Create(context.Background(), "sf-account-001")

	require.NoError(t, err)
	calls := pub.calls()
	require.NotEmpty(t, calls, "at least one publish call expected on create")
	assert.True(t, strings.HasPrefix(calls[0], "indexer:"),
		"first call must be indexer (sequential before FGA errgroup); got %v", calls)
}

func TestB2BOrgWriter_Create_PublishesAtLeastOneAccess(t *testing.T) {
	org := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	pub := &trackingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{org: org},
		&seededOrgWriter{createOrg: org},
		pub,
		"admin-team-uid",
	)

	_, err := w.Create(context.Background(), "sf-account-001")

	require.NoError(t, err)
	assert.Greater(t, countCalls(pub.calls(), "access:"), 0,
		"create must emit at least one FGA access call")
}

// ── Update tests ──────────────────────────────────────────────────────────

func TestB2BOrgWriter_Update_NoOp_SkipsWriteAndPublish(t *testing.T) {
	current := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	pub := &trackingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{org: current},
		&seededOrgWriter{},
		pub,
		"",
	)

	// Empty input → HasChanges() == false → no write, no publish.
	_, err := w.Update(context.Background(), testB2BOrgUID, model.B2BOrgInput{}, "")

	require.NoError(t, err)
	assert.Empty(t, pub.calls(), "no-op update must not publish")
}

func TestB2BOrgWriter_Update_HasChanges_IndexerBeforeAccess(t *testing.T) {
	current := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	updated := &model.B2BOrg{UID: testB2BOrgUID, Name: "Updated Name", UpdatedAt: time.Now()}
	pub := &trackingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{org: current},
		&seededOrgWriter{updateOrg: updated},
		pub,
		"",
	)

	input := model.B2BOrgInput{Name: "Updated Name"}
	_, err := w.Update(context.Background(), testB2BOrgUID, input, "")

	require.NoError(t, err)
	calls := pub.calls()
	require.NotEmpty(t, calls)
	assert.True(t, strings.HasPrefix(calls[0], "indexer:"),
		"first call must be indexer (sequential before errgroup FGA); got %v", calls)
}

func TestB2BOrgWriter_Update_IfMatch_Mismatch_PreconditionFailed(t *testing.T) {
	current := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	pub := &trackingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{org: current},
		&seededOrgWriter{},
		pub,
		"",
	)

	// IfMatch mismatch must fail before any write.
	input := model.B2BOrgInput{Name: "Name"}
	_, err := w.Update(context.Background(), testB2BOrgUID, input, "\"stale-etag\"")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err))
	assert.Empty(t, pub.calls(), "must not publish on precondition failure")
}

func TestB2BOrgWriter_Update_IfMatch_Matches_Succeeds(t *testing.T) {
	current := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	updated := &model.B2BOrg{UID: testB2BOrgUID, Name: "Name", UpdatedAt: time.Now()}
	pub := &trackingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{org: current},
		&seededOrgWriter{updateOrg: updated},
		pub,
		"",
	)

	input := model.B2BOrgInput{Name: "Name"}
	_, err := w.Update(context.Background(), testB2BOrgUID, input, mustEtag(t, current))

	assert.NoError(t, err)
}

// TestB2BOrgWriter_Update_Reparenting_EmitsMoreAccessCalls verifies that when a b2b_org's
// ParentUID changes (as returned by the writer), the fan-out emits extra FGA access calls
// for the old-parent and new-parent child-list messages on top of the base update_access.
func TestB2BOrgWriter_Update_Reparenting_EmitsMoreAccessCalls(t *testing.T) {
	current := &model.B2BOrg{UID: testB2BOrgUID, ParentUID: "old-parent", UpdatedAt: time.Now()}

	// Writer returns org with a new parent — this is what triggers reparenting.
	reparentedOrg := &model.B2BOrg{UID: testB2BOrgUID, ParentUID: "new-parent", UpdatedAt: time.Now()}
	// Writer returns org with same parent — no reparenting.
	sameParentOrg := &model.B2BOrg{UID: testB2BOrgUID, ParentUID: "old-parent", UpdatedAt: time.Now()}

	reparentPub := &trackingPublisher{}
	wReparent := newB2BOrgWriter(
		&seededOrgReader{
			org: current,
			children: map[string][]string{
				"old-parent": {"sibling-org"},
				"new-parent": {},
			},
		},
		&seededOrgWriter{updateOrg: reparentedOrg},
		reparentPub,
		"",
	)

	sameParentPub := &trackingPublisher{}
	wSame := newB2BOrgWriter(
		&seededOrgReader{org: current},
		&seededOrgWriter{updateOrg: sameParentOrg},
		sameParentPub,
		"",
	)

	input := model.B2BOrgInput{Name: "Updated Name"}
	_, err := wReparent.Update(context.Background(), testB2BOrgUID, input, "")
	require.NoError(t, err)

	_, err = wSame.Update(context.Background(), testB2BOrgUID, input, "")
	require.NoError(t, err)

	assert.Greater(t,
		countCalls(reparentPub.calls(), "access:"),
		countCalls(sameParentPub.calls(), "access:"),
		"reparenting must emit more FGA access calls than a non-reparenting update")
}

// ── Children field tests ───────────────────────────────────────────────────

func TestB2BOrgWriter_Create_PopulatesChildrenInIndexer(t *testing.T) {
	orgWithChildren := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	pub := &capturingPublisher{}
	childUIDs := []string{"child-1", "child-2"}
	w := newB2BOrgWriter(
		&seededOrgReader{
			org:      orgWithChildren,
			children: map[string][]string{testB2BOrgUID: childUIDs},
		},
		&seededOrgWriter{createOrg: orgWithChildren},
		pub,
		"admin-team-uid",
	)

	result, err := w.Create(context.Background(), "sf-account-001")

	require.NoError(t, err)
	assert.True(t, result.IsParent, "created org with children should have IsParent = true")

	// Verify the indexer message received the is_parent field
	msgs := pub.getIndexerMessages()
	require.Len(t, msgs, 1, "should publish exactly one indexer message on create")
	msgBytes, err := json.Marshal(msgs[0])
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(msgBytes, &decoded))
	if data, ok := decoded["data"]; ok {
		dataBytes, _ := json.Marshal(data)
		var dataObj map[string]interface{}
		_ = json.Unmarshal(dataBytes, &dataObj)
		isParentVal, _ := dataObj["is_parent"].(bool)
		assert.True(t, isParentVal, "indexer message data should include is_parent = true")
	}
}

func TestB2BOrgWriter_Create_LeafOrgNoChildren(t *testing.T) {
	leafOrg := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	pub := &capturingPublisher{}
	w := newB2BOrgWriter(
		&seededOrgReader{
			org:      leafOrg,
			children: map[string][]string{testB2BOrgUID: {}}, // empty children list
		},
		&seededOrgWriter{createOrg: leafOrg},
		pub,
		"admin-team-uid",
	)

	result, err := w.Create(context.Background(), "sf-account-001")

	require.NoError(t, err)
	assert.False(t, result.IsParent, "leaf org should have IsParent = false")
}

func TestB2BOrgWriter_Update_PopulatesChildrenInIndexer(t *testing.T) {
	current := &model.B2BOrg{UID: testB2BOrgUID, UpdatedAt: time.Now()}
	updated := &model.B2BOrg{UID: testB2BOrgUID, Name: "Updated Name", UpdatedAt: time.Now()}
	pub := &capturingPublisher{}
	childUIDs := []string{"child-1", "child-2", "child-3"}
	w := newB2BOrgWriter(
		&seededOrgReader{
			org:      current,
			children: map[string][]string{testB2BOrgUID: childUIDs},
		},
		&seededOrgWriter{updateOrg: updated},
		pub,
		"",
	)

	input := model.B2BOrgInput{Name: "Updated Name"}
	result, err := w.Update(context.Background(), testB2BOrgUID, input, "")

	require.NoError(t, err)
	assert.True(t, result.IsParent, "updated org with children should have IsParent = true")
}

// ── Helper ────────────────────────────────────────────────────────────────

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
