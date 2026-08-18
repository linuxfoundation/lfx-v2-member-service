// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// sfid returns a deterministic canonical 18-char Salesforce test ID from a
// human-readable label. Non-alnum chars are stripped and lowercased, the body
// is right-padded with '0' to 15 chars, and the suffix is computed by
// sfuuid.Salesforce15To18 — the same production function used at runtime.
func sfid(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 15 {
		s = s[:15]
	}
	s += strings.Repeat("0", 15-len(s))
	id, _ := sfuuid.Salesforce15To18(s)
	return id
}

// ── In-process stubs ──────────────────────────────────────────────────────────

// fakeCDCSubscriber feeds a fixed slice of events then closes the channel.
type fakeCDCSubscriber struct {
	events []model.CDCEvent
}

func (f *fakeCDCSubscriber) Subscribe(_ context.Context, _ string, _ []byte, _ port.ReplayStore) (<-chan model.CDCEvent, error) {
	ch := make(chan model.CDCEvent, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// errCDCSubscriber always returns an error from Subscribe.
type errCDCSubscriber struct{ err error }

func (e *errCDCSubscriber) Subscribe(_ context.Context, _ string, _ []byte, _ port.ReplayStore) (<-chan model.CDCEvent, error) {
	return nil, e.err
}

// fakeReplayStore records the last saved replay ID (commit-after-process check).
type fakeReplayStore struct {
	saved    []byte
	savedAll [][]byte // every Save call, in order
	loadErr  error
	saveErr  error
}

func (r *fakeReplayStore) Load(_ context.Context, _ string) ([]byte, error) {
	return nil, r.loadErr
}
func (r *fakeReplayStore) Save(_ context.Context, _ string, id []byte) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	r.saved = id
	r.savedAll = append(r.savedAll, id)
	return nil
}

func requireAuthorizationRetry(
	t *testing.T,
	consumer *svc.CDCConsumer,
	channel string,
	replay *fakeReplayStore,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, consumer.Run(ctx, channel, replay), context.DeadlineExceeded)
	assert.Empty(t, replay.savedAll)
}

// reparentingB2BOrgReader returns different results for GetB2BOrg on successive
// calls: first call returns the pre-change record (old parent), subsequent
// calls return the post-change record (new parent). This simulates the consumer
// reading the cached old state before eviction and then re-fetching from
// Salesforce after eviction.
type reparentingB2BOrgReader struct {
	calls    int
	preOrg   *model.B2BOrg // returned on call 0 (before eviction)
	postOrg  *model.B2BOrg // returned on call 1+ (after eviction)
	children map[string][]string
}

func (r *reparentingB2BOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	defer func() { r.calls++ }()
	if r.calls == 0 {
		return r.preOrg, nil
	}
	return r.postOrg, nil
}
func (r *reparentingB2BOrgReader) FetchChildUIDsByParentUID(_ context.Context, parentUID string) ([]string, error) {
	if r.children != nil {
		return r.children[parentUID], nil
	}
	return nil, nil
}
func (r *reparentingB2BOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// fakeB2BOrgReader returns a pre-seeded org.
type fakeB2BOrgReader struct {
	org            *model.B2BOrg
	children       []string
	orgErr         error
	childMap       map[string][]string
	batchErr       error
	batchCallCount atomic.Int32
}

func (r *fakeB2BOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	return r.org, r.orgErr
}
func (r *fakeB2BOrgReader) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return r.children, nil
}
func (r *fakeB2BOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	r.batchCallCount.Add(1)
	if r.childMap != nil {
		return r.childMap, r.batchErr
	}
	return map[string][]string{}, r.batchErr
}

// subjectCapturingPublisher captures subjects and message payloads for
// both indexer and access publish calls.
type subjectCapturingPublisher struct {
	mu              sync.Mutex
	indexer         []string // subjects
	indexerMessages []any    // payloads, parallel to indexer
	access          []string // subjects
	accessMessages  []any    // payloads, parallel to access
	flushCount      int      // upserts never flush; key_contact delete/supersede and genuine-delete purges do (see flushErr)
	flushErr        error    // returned by every Flush call when set
	accessErr       error    // returned by every Access call when set; the call is still recorded

	// beforeAccess, when set, runs at the top of Access and its error is
	// returned instead of accessErr. Use it to fail a specific message rather
	// than every access publish.
	beforeAccess func(subject string, msg any) error
}

func (p *subjectCapturingPublisher) Indexer(_ context.Context, subject string, msg any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.indexer = append(p.indexer, subject)
	p.indexerMessages = append(p.indexerMessages, msg)
	return nil
}
func (p *subjectCapturingPublisher) Access(_ context.Context, subject string, msg any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.access = append(p.access, subject)
	p.accessMessages = append(p.accessMessages, msg)
	if p.beforeAccess != nil {
		return p.beforeAccess(subject, msg)
	}
	return p.accessErr
}

func (p *subjectCapturingPublisher) Flush(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushCount++
	return p.flushErr
}

// hasAccess returns true if any access call subject contains the given substring.
func (p *subjectCapturingPublisher) hasAccess(sub string) bool {
	for _, s := range p.access {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// indexerAction extracts the "action" field from the i-th indexer message by
// round-tripping through JSON. Returns "" if the message is nil or the field
// is absent.
func (p *subjectCapturingPublisher) indexerAction(i int) string {
	if i >= len(p.indexerMessages) || p.indexerMessages[i] == nil {
		return ""
	}
	b, err := json.Marshal(p.indexerMessages[i])
	if err != nil {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	raw, ok := m["action"]
	if !ok {
		return ""
	}
	var action string
	_ = json.Unmarshal(raw, &action)
	return action
}

// indexerDataIsString reports whether the i-th indexer message's wire "data"
// field is a JSON string (the indexer delete contract) and returns its value.
func (p *subjectCapturingPublisher) indexerDataIsString(i int) (string, bool) {
	if i >= len(p.indexerMessages) || p.indexerMessages[i] == nil {
		return "", false
	}
	b, err := json.Marshal(p.indexerMessages[i])
	if err != nil {
		return "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	raw, ok := m["data"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false // data is not a JSON string (e.g. an object)
	}
	return s, true
}

// ── Constructor helper ────────────────────────────────────────────────────────

func newTestCDCConsumer(
	subscriber port.CDCSubscriber,
	orgReader *fakeB2BOrgReader,
	invalidator *mock.MockCacheInvalidator,
	pub *subjectCapturingPublisher,
	globalOrgAdminTeamUID string,
	extraOpts ...svc.CDCConsumerOption,
) *svc.CDCConsumer {
	opts := []svc.CDCConsumerOption{
		svc.WithCDCSubscriber(subscriber),
		svc.WithCDCB2BOrgReader(orgReader),
		svc.WithCDCCacheInvalidator(invalidator),
		svc.WithCDCPublisher(pub),
		svc.WithCDCGlobalOrgAdminTeamUID(globalOrgAdminTeamUID),
	}
	return svc.NewCDCConsumer(append(opts, extraOpts...)...)
}

// ── Account (b2b_org) tests ───────────────────────────────────────────────────

// indexerIsParent extracts data.is_parent from an indexer message captured by
// subjectCapturingPublisher. Returns false if the field is absent (omitempty).
func indexerIsParent(msg any) bool {
	b, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return false
	}
	raw, ok := envelope.Data["is_parent"]
	if !ok {
		return false
	}
	var v bool
	_ = json.Unmarshal(raw, &v)
	return v
}

// TestCDCConsumer_Account_BatchSetsIsParentFromChildUIDsBatch verifies that the
// CDC batch path calls FetchChildUIDsByParentUIDs exactly once per batch via
// b2bOrgReader and uses the result to set is_parent on each org before publishing.
func TestCDCConsumer_Account_BatchSetsIsParentFromChildUIDsBatch(t *testing.T) {
	t.Parallel()

	parentOrg := &model.B2BOrg{UID: sfid("parent-org")}
	leafOrg := &model.B2BOrg{UID: sfid("leaf-org")}

	// parentOrg has a child; leafOrg does not appear in the map → is_parent=false.
	orgReader := &fakeB2BOrgReader{
		childMap: map[string][]string{
			parentOrg.UID: {"some-child-uid"},
		},
	}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate,
				RecordIDs: []string{sfid("parent-org"), sfid("leaf-org")}, ReplayID: []byte("bp1")},
		}},
		orgReader,
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{parentOrg, leafOrg}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	// FetchChildUIDsByParentUIDs called exactly once for the whole batch — not once per org.
	assert.Equal(t, int32(1), orgReader.batchCallCount.Load(),
		"FetchChildUIDsByParentUIDs must be called once per batch, not per org")

	// Two indexer messages published (one per org).
	require.Len(t, pub.indexerMessages, 2, "both orgs must publish an indexer message")

	// Identify messages by UID rather than position (order is not guaranteed).
	var gotParentIsParent, gotLeafIsParent bool
	for _, msg := range pub.indexerMessages {
		b, _ := json.Marshal(msg)
		var env struct {
			Data struct {
				UID      string `json:"uid"`
				IsParent bool   `json:"is_parent"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			continue
		}
		if env.Data.UID == parentOrg.UID {
			gotParentIsParent = env.Data.IsParent
		}
		if env.Data.UID == leafOrg.UID {
			gotLeafIsParent = env.Data.IsParent
		}
	}
	assert.True(t, gotParentIsParent, "parent org must have is_parent=true in indexer message")
	assert.False(t, gotLeafIsParent, "leaf org must have is_parent=false in indexer message")
}

// TestCDCConsumer_Account_BatchChildFetchError_ContinuesBatchWithFalse verifies
// that a FetchChildUIDsByParentUIDs failure is non-fatal: all orgs are still
// published with is_parent=false.
func TestCDCConsumer_Account_BatchChildFetchError_ContinuesBatchWithFalse(t *testing.T) {
	t.Parallel()

	org := &model.B2BOrg{UID: sfid("parent-would-be")}

	orgReader := &fakeB2BOrgReader{
		batchErr: errors.New("salesforce timeout"),
	}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate,
				RecordIDs: []string{sfid("parent-would-be")}, ReplayID: []byte("bp2")},
		}},
		orgReader,
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	// Batch must continue despite the error — org still published.
	require.Len(t, pub.indexerMessages, 1, "org must still be published even when FetchChildUIDsByParentUIDs fails")

	// is_parent degrades to false when the fetch fails.
	assert.False(t, indexerIsParent(pub.indexerMessages[0]),
		"is_parent must be false when FetchChildUIDsByParentUIDs errors")
}

func TestCDCConsumer_Account_Upsert_PublishesIndexerAndFGA(t *testing.T) {
	org := &model.B2BOrg{UID: sfid("org-uid-1")}
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("org-uid-1")}, ReplayID: []byte("r1")},
		}},
		&fakeB2BOrgReader{org: org},
		invalidator,
		pub,
		"admin-team-uid",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	)

	replay := &fakeReplayStore{}
	err := consumer.Run(context.Background(), "/data/AccountChangeEvent", replay)
	require.NoError(t, err)

	assert.NotEmpty(t, pub.indexer, "indexer must be published on account upsert")
	assert.NotEmpty(t, pub.access, "FGA access must be published on account upsert")
	assert.Equal(t, 1, invalidator.B2BOrgCalls, "cache must be invalidated once")
	assert.Equal(t, []byte("r1"), replay.saved, "replay cursor must be committed")
}

func TestCDCConsumer_Account_Upsert_PassesGlobalOrgAdminTeamUID(t *testing.T) {
	// globalOrgAdminTeamUID must reach BuildB2BOrgFGAMessage — verified indirectly:
	// if it were "" the FGA subject is still emitted; this test ensures the field
	// is wired at all (non-empty UID → message contains non-empty team reference).
	org := &model.B2BOrg{UID: sfid("org-uid-1")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("org-uid-1")}, ReplayID: []byte("r2")},
		}},
		&fakeB2BOrgReader{org: org},
		&mock.MockCacheInvalidator{},
		pub,
		"global-admin-team",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
	assert.NotEmpty(t, pub.access, "FGA access must be published")
}

// TestCDCConsumer_Account_Delete_AssertsNoTeamReferences pins the delete-path
// behaviour change. fga-sync structurally never deletes a tuple whose subject
// begins with "team:", so any team reference asserted on an org that no longer
// exists is a permanent orphan on a dead object that nothing can ever reap.
// Before this change the delete path re-asserted global_org_admin; adding the
// two auditor teams would have tripled the rate of those orphans.
// TestCDCConsumer_Account_Delete_AssertsNoTeamReferences guards the original
// concern — a delete must not write team references that nothing can ever reap
// — now that the delete path sends delete_access instead of update_access.
// Asserting the absence of any update_access is the stronger form of the old
// per-field checks: a message that is never sent can carry no orphan reference.
func TestCDCConsumer_Account_Delete_AssertsNoTeamReferences(t *testing.T) {
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete, RecordIDs: []string{sfid("org-uid-teams")}, ReplayID: []byte("r3t")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"global-admin-team",
		svc.WithCDCB2BOrgAuditorTeams([]string{"staff-team", "second-team"}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	assert.Zero(t, pub.updateAccessCount(),
		"a delete must not write team references onto an object that no longer exists")
	assert.Equal(t, []string{sfid("org-uid-teams")}, pub.deleteAccessUIDs(t, "b2b_org"),
		"the delete must instead withdraw the org's tuples")
}

func TestCDCConsumer_Account_Delete_PublishesIndexerAndFGA(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete, RecordIDs: []string{sfid("org-uid-del")}, ReplayID: []byte("r3")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))

	assert.NotEmpty(t, pub.indexer, "indexer delete must be published")
	assert.NotEmpty(t, pub.access, "FGA access must be published on delete")
	assert.Equal(t, 1, invalidator.B2BOrgCalls, "cache must be invalidated on delete")
	assert.Equal(t, []byte("r3"), replay.saved)
	data, isStr := pub.indexerDataIsString(0)
	assert.True(t, isStr, "b2b_org delete data must be a JSON string (object ID), not an object")
	assert.Equal(t, sfid("org-uid-del"), data)
}

// ── Asset (project_membership) tests ─────────────────────────────────────────

func TestCDCConsumer_Asset_Upsert_PublishesIndexerAndFGA(t *testing.T) {
	pm := &model.ProjectMembership{UID: sfid("pm-uid-1"), B2BOrgUID: "org-uid-1"}
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-uid-1")}, ReplayID: []byte("r4")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	assert.NotEmpty(t, pub.indexer, "indexer must be published")
	assert.NotEmpty(t, pub.access, "FGA access (project_membership) must be published")
	assert.Equal(t, 1, invalidator.MembershipCalls)
	assert.Equal(t, []byte("r4"), replay.saved)
}

func TestCDCConsumer_Asset_Delete_PublishesIndexerOnly(t *testing.T) {
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeDelete, RecordIDs: []string{sfid("pm-uid-del")}, ReplayID: []byte("r5")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "indexer delete must be published")
	assert.Zero(t, pub.updateAccessCount(),
		"a membership delete reconciles nothing — it withdraws, via delete_access")
	data, isStr := pub.indexerDataIsString(0)
	assert.True(t, isStr, "project_membership delete data must be a JSON string (object ID), not an object")
	assert.Equal(t, sfid("pm-uid-del"), data)
}

// ── Project_Role__c (key_contact) tests ──────────────────────────────────────

func TestCDCConsumer_ProjectRole_Upsert_WithUsername_PublishesIndexerAndFGAMemberPut(t *testing.T) {
	kc := &model.KeyContact{UID: sfid("kc-uid-1"), MembershipUID: "pm-uid-1", Username: "alice"}
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("kc-uid-1")}, ReplayID: []byte("r6")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", replay))

	assert.NotEmpty(t, pub.indexer, "indexer must be published")
	assert.True(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"FGA member_put must be published for accepted key contact; access calls: %v", pub.access)
	assert.Equal(t, 1, invalidator.KeyContactCalls)
	assert.Equal(t, []byte("r6"), replay.saved)
}

func TestCDCConsumer_ProjectRole_Upsert_WithoutUsername_NoFGAMemberPut(t *testing.T) {
	// Pending/unaccepted contact — no username — must not emit FGA member_put.
	kc := &model.KeyContact{UID: sfid("kc-uid-2"), MembershipUID: "pm-uid-1", Username: ""}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("kc-uid-2")}, ReplayID: []byte("r7")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "indexer must still be published")
	assert.False(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"FGA member_put must NOT be published for pending contact without username")
}

func TestCDCConsumer_ProjectRole_Delete_PublishesIndexerAndFGAMemberRemove(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeDelete, RecordIDs: []string{sfid("kc-uid-del")}, ReplayID: []byte("r8")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", replay))

	assert.NotEmpty(t, pub.indexer, "indexer delete must be published")
	assert.True(t, pub.hasAccess(fgaconstants.GenericMemberRemoveSubject),
		"FGA member_remove must be published on key_contact delete; access calls: %v", pub.access)
	assert.Equal(t, 1, invalidator.KeyContactCalls)
	assert.Equal(t, []byte("r8"), replay.saved)
	data, isStr := pub.indexerDataIsString(0)
	assert.True(t, isStr, "key_contact delete data must be a JSON string (object ID), not an object")
	assert.Equal(t, sfid("kc-uid-del"), data)

	// With no recorded grant there is nothing to address the revoke with, so the
	// remove falls back to the key contact's own UID and an empty username. This
	// message is knowingly rejected by fga-sync; it is retained only for parity
	// with the behaviour that predates the grant index.
	require.NotEmpty(t, pub.accessMessages)
	removeMsg, ok := pub.accessMessages[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	assert.Equal(t, "member_remove", removeMsg.Operation)
	removeData, ok := removeMsg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, sfid("kc-uid-del"), removeData.UID,
		"index miss must fall back to the key contact's own UID")
	assert.Empty(t, removeData.Username)

	assert.Zero(t, pub.flushCount,
		"no grant index is wired, so there is no index entry to flush before clearing")
}

// TestCDCConsumer_ProjectRole_Delete_UsesGrantIndex covers the bug this index
// exists for: the revoke must target the project_membership the grant was made
// on, not the key contact's own SFID, which OpenFGA has no tuple for.
func TestCDCConsumer_ProjectRole_Delete_UsesGrantIndex(t *testing.T) {
	kcUID := sfid("kc-uid-indexed")
	membershipUID := sfid("asset-parent")

	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			kcUID: {MembershipUID: membershipUID, Username: "jdoe", Revision: 7},
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeDelete, RecordIDs: []string{kcUID}, ReplayID: []byte("r8b")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactGrantIndex(grants),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.accessMessages)
	removeMsg, ok := pub.accessMessages[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	assert.Equal(t, "member_remove", removeMsg.Operation)
	removeData, ok := removeMsg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, membershipUID, removeData.UID,
		"revoke must target the membership the grant was made on, not the key contact SFID")
	assert.Equal(t, "jdoe", removeData.Username,
		"fga-sync rejects a member_remove with an empty username")

	assert.Equal(t, []string{kcUID}, grants.Deletes,
		"the grant entry must be cleared once the revoke is published")
	assert.Equal(t, 1, pub.flushCount,
		"delivery must be confirmed before the only recorded address is cleared")
}

// TestCDCConsumer_ProjectRole_Delete_FlushFailure_PreservesIndexEntry verifies
// that Access only hands the revoke to the local NATS
// connection, it does not confirm the broker received it. Deleting the index
// entry immediately after a nil Access error — without confirming delivery —
// would create a crash/disconnect window where the member_remove is lost and
// a replayed CDC delete can no longer address the tuple because its only
// address is already gone. Flush must be confirmed first, and a failed flush
// must leave the entry in place for the next delivery attempt to use.
func TestCDCConsumer_ProjectRole_Delete_FlushFailure_PreservesIndexEntry(t *testing.T) {
	kcUID := sfid("kc-uid-flushfail")
	membershipUID := sfid("asset-flushfail-parent")

	pub := &subjectCapturingPublisher{flushErr: assert.AnError}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			kcUID: {MembershipUID: membershipUID, Username: "jdoe", Revision: 7},
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeDelete, RecordIDs: []string{kcUID}, ReplayID: []byte("r8flush")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactGrantIndex(grants),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.accessMessages, "the revoke was handed to NATS even though delivery was never confirmed")
	assert.Empty(t, grants.Deletes,
		"an unconfirmed flush must not clear the only recorded address for this grant")
	_, found, err := grants.Get(context.Background(), kcUID)
	require.NoError(t, err)
	assert.True(t, found, "the entry must survive so a retry can still address the revoke")
}

// TestCDCConsumer_ProjectRole_Delete_TransientIndexReadFailure_Retries verifies
// that a grant-index read failure must not be collapsed into an
// ordinary miss on the first try — the index may still hold the exact address
// needed to revoke this grant, and once this handler returns, the replay
// cursor advances with no other chance to retry a deleted contact. A bounded
// retry that recovers within this call must still use the recorded grant.
func TestCDCConsumer_ProjectRole_Delete_TransientIndexReadFailure_Retries(t *testing.T) {
	kcUID := sfid("kc-uid-flaky")
	membershipUID := sfid("asset-flaky-parent")

	pub := &subjectCapturingPublisher{}
	calls := 0
	grants := &mock.MockKeyContactGrantIndex{
		GetFn: func(_ context.Context, _ string) (port.KeyContactGrant, bool, error) {
			calls++
			if calls == 1 {
				// One transient failure, then the read succeeds — well within
				// maxGrantIndexReadAttempts.
				return port.KeyContactGrant{}, false, assert.AnError
			}
			return port.KeyContactGrant{MembershipUID: membershipUID, Username: "jdoe", Revision: 3}, true, nil
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeDelete, RecordIDs: []string{kcUID}, ReplayID: []byte("r8flaky")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactGrantIndex(grants),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.accessMessages)
	removeMsg, ok := pub.accessMessages[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	removeData, ok := removeMsg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, membershipUID, removeData.UID,
		"a read that recovers within the retry budget must still use the recorded grant, not the unaddressed fallback")
	assert.Equal(t, "jdoe", removeData.Username)
}

// TestCDCConsumer_ProjectRole_Delete_IndexReadFailsAllAttempts_FallsBackAndExhaustsRetries
// covers the other side of the same finding: once every retry attempt fails,
// the handler must still fall back to the (known-useless) unaddressed revoke
// rather than blocking the batch — but only after exhausting the retry
// budget, not on the first error.
func TestCDCConsumer_ProjectRole_Delete_IndexReadFailsAllAttempts_FallsBackAndExhaustsRetries(t *testing.T) {
	kcUID := sfid("kc-uid-downtime")

	pub := &subjectCapturingPublisher{}
	calls := 0
	grants := &mock.MockKeyContactGrantIndex{
		GetFn: func(_ context.Context, _ string) (port.KeyContactGrant, bool, error) {
			calls++
			return port.KeyContactGrant{}, false, assert.AnError
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeDelete, RecordIDs: []string{kcUID}, ReplayID: []byte("r8down")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactGrantIndex(grants),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", replay))

	assert.Equal(t, 3, calls, "must exhaust the retry budget, not give up on the first error")
	assert.Equal(t, []byte("r8down"), replay.saved, "the batch must not be blocked by an exhausted retry")

	require.NotEmpty(t, pub.accessMessages)
	removeMsg, ok := pub.accessMessages[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	removeData, ok := removeMsg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, kcUID, removeData.UID,
		"once retries are exhausted, the handler still falls back to the unaddressed revoke rather than blocking")
}

// TestCDCConsumer_ProjectRole_AbsentFromSOQL_UsesGrantIndex covers the second
// deletion trigger: a record that vanished from the upsert query rather than
// arriving as an explicit delete event. It routes through the same handler, so
// it must resolve the revoke the same way.
func TestCDCConsumer_ProjectRole_AbsentFromSOQL_UsesGrantIndex(t *testing.T) {
	kcUID := sfid("kc-uid-absent")
	membershipUID := sfid("asset-absent-parent")

	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			kcUID: {MembershipUID: membershipUID, Username: "asmith", Revision: 2},
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{kcUID}, ReplayID: []byte("r8c")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		// The batch reader returns no contact for the requested SFID, which the
		// consumer treats as a soft delete.
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{}),
		svc.WithCDCKeyContactGrantIndex(grants),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.accessMessages)
	removeMsg, ok := pub.accessMessages[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	removeData, ok := removeMsg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, membershipUID, removeData.UID)
	assert.Equal(t, "asmith", removeData.Username)
	assert.Equal(t, []string{kcUID}, grants.Deletes)
}

// ── Error resilience ──────────────────────────────────────────────────────────

func TestCDCConsumer_UnhandledEntity_SkipsAndAdvancesReplay(t *testing.T) {
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Opportunity", ChangeType: model.CDCChangeCreate, RecordIDs: []string{"opp-1"}, ReplayID: []byte("r9")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/ChangeEvents", replay))

	assert.Empty(t, pub.indexer, "unknown entity must produce no indexer publish")
	assert.Empty(t, pub.access, "unknown entity must produce no FGA publish")
	assert.Equal(t, []byte("r9"), replay.saved, "replay cursor must still advance on skip")
}

func TestCDCConsumer_HandlerError_ReplayStillAdvances(t *testing.T) {
	// ProjectMembershipReader returns an error (simulates Salesforce sObject fetch
	// failure after cache invalidation). The handler fails but replay must still
	// advance — at-least-once semantics; /admin/reindex recovers missed events.
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-bad")}, ReplayID: []byte("r10")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Err: pkgerrors.NewNotFound("not found")}),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	assert.Empty(t, pub.indexer, "failed handler must not publish")
	assert.Equal(t, []byte("r10"), replay.saved, "replay cursor must advance even after handler error")
}

func TestCDCConsumer_MultipleRecordIDs_ProcessedAll(t *testing.T) {
	// A batch event with two record IDs must result in two cache invalidations.
	invalidator := &mock.MockCacheInvalidator{}
	pm1 := &model.ProjectMembership{UID: sfid("pm-1")}
	pm2 := &model.ProjectMembership{UID: sfid("pm-2")}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-1"), sfid("pm-2")}, ReplayID: []byte("r11")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		&subjectCapturingPublisher{},
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm1, pm2}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 2, invalidator.MembershipCalls, "both record IDs in the batch must be processed")
}

// ── Create action tests ───────────────────────────────────────────────────────

func TestCDCConsumer_Asset_Create_SetsActionCreated(t *testing.T) {
	// CDCChangeCreate must result in ActionCreated in the indexer message payload,
	// not ActionUpdated. The action is encoded in the message body, not the subject.
	pm := &model.ProjectMembership{UID: sfid("pm-create-1")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeCreate, RecordIDs: []string{sfid("pm-create-1")}, ReplayID: []byte("rc1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.Len(t, pub.indexer, 1, "exactly one indexer call on create")
	assert.Equal(t, "created", pub.indexerAction(0),
		"indexer message action must be 'created' for CDCChangeCreate")
}

func TestCDCConsumer_ProjectRole_Create_SetsActionCreated(t *testing.T) {
	kc := &model.KeyContact{UID: sfid("kc-create-1"), MembershipUID: "pm-uid-1", Username: "bob"}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeCreate, RecordIDs: []string{sfid("kc-create-1")}, ReplayID: []byte("rc2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.Len(t, pub.indexer, 1, "exactly one indexer call on create")
	assert.Equal(t, "created", pub.indexerAction(0),
		"indexer message action must be 'created' for CDCChangeCreate")
}

// ── Reparenting test ──────────────────────────────────────────────────────────

func TestCDCConsumer_Account_Reparenting_EmitsMoreFGAAccessCalls(t *testing.T) {
	// Pre-change: org has old-parent. Post-change: org has new-parent.
	// The consumer reads pre-change before eviction, then post-change after.
	// BuildB2BOrgReparentingMessages should fire extra FGA access calls.
	preOrg := &model.B2BOrg{UID: sfid("org-uid-r"), ParentUID: "old-parent"}
	postOrg := &model.B2BOrg{UID: sfid("org-uid-r"), ParentUID: "new-parent"}

	reparentReader := &reparentingB2BOrgReader{
		preOrg:  preOrg,
		postOrg: postOrg,
		children: map[string][]string{
			"old-parent":      {"sibling-org"},
			"new-parent":      {},
			sfid("org-uid-r"): {},
		},
	}

	// Baseline: same parent (no reparenting) — should emit fewer FGA calls.
	sameOrg := &model.B2BOrg{UID: sfid("org-uid-s"), ParentUID: "same-parent"}
	sameReader := &reparentingB2BOrgReader{
		preOrg:  sameOrg,
		postOrg: sameOrg,
		children: map[string][]string{
			"same-parent":     {},
			sfid("org-uid-s"): {},
		},
	}

	reparentPub := &subjectCapturingPublisher{}
	reparentConsumer := svc.NewCDCConsumer(
		svc.WithCDCSubscriber(&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("org-uid-r")}, ReplayID: []byte("rr1")},
		}}),
		svc.WithCDCB2BOrgReader(reparentReader),
		svc.WithCDCCacheInvalidator(&mock.MockCacheInvalidator{}),
		svc.WithCDCPublisher(reparentPub),
		svc.WithCDCGlobalOrgAdminTeamUID(""),
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{postOrg}}),
	)

	samePub := &subjectCapturingPublisher{}
	sameConsumer := svc.NewCDCConsumer(
		svc.WithCDCSubscriber(&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("org-uid-s")}, ReplayID: []byte("rr2")},
		}}),
		svc.WithCDCB2BOrgReader(sameReader),
		svc.WithCDCCacheInvalidator(&mock.MockCacheInvalidator{}),
		svc.WithCDCPublisher(samePub),
		svc.WithCDCGlobalOrgAdminTeamUID(""),
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{sameOrg}}),
	)

	require.NoError(t, reparentConsumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
	require.NoError(t, sameConsumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	assert.Greater(t, len(reparentPub.access), len(samePub.access),
		"reparenting must emit more FGA access calls (%d) than a non-reparenting update (%d)",
		len(reparentPub.access), len(samePub.access))
}

// ── Guard condition tests ─────────────────────────────────────────────────────

func TestCDCConsumer_ProjectRole_Upsert_WithUsername_EmptyMembershipUID_NoFGAMemberPut(t *testing.T) {
	// Guard: kc.Username != "" && kc.MembershipUID != ""
	// A malformed record with a username but no MembershipUID must NOT emit FGA member_put.
	kc := &model.KeyContact{UID: sfid("kc-bad"), MembershipUID: "", Username: "charlie"}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("kc-bad")}, ReplayID: []byte("rg1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "indexer must still be published even for malformed record")
	assert.False(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"FGA member_put must NOT be published when MembershipUID is empty; access calls: %v", pub.access)
}

// ── Startup error tests ───────────────────────────────────────────────────────

func TestCDCConsumer_ReplayStore_LoadError_RunReturnsError(t *testing.T) {
	loadErr := errors.New("nats: kv unavailable")

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		&subjectCapturingPublisher{},
		"",
	)

	replay := &fakeReplayStore{loadErr: loadErr}
	err := consumer.Run(context.Background(), "/data/AccountChangeEvent", replay)

	require.Error(t, err, "Run must return the Load error")
	assert.ErrorIs(t, err, loadErr)
}

func TestCDCConsumer_Subscriber_SubscribeError_RunReturnsError(t *testing.T) {
	subscribeErr := errors.New("grpc: connection refused")

	consumer := newTestCDCConsumer(
		&errCDCSubscriber{err: subscribeErr},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		&subjectCapturingPublisher{},
		"",
	)

	err := consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{})

	require.Error(t, err, "Run must propagate Subscribe error")
	assert.ErrorIs(t, err, subscribeErr)
}

// ── Replay cursor durability tests ────────────────────────────────────────────

func TestCDCConsumer_ReplayStore_SaveError_NotFatal(t *testing.T) {
	// Save failures are logged and swallowed — Run must not return an error.
	pm := &model.ProjectMembership{UID: sfid("pm-save-err")}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-save-err")}, ReplayID: []byte("rs1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		&subjectCapturingPublisher{},
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
	)

	replay := &fakeReplayStore{saveErr: errors.New("nats: kv write failed")}
	err := consumer.Run(context.Background(), "/data/AssetChangeEvent", replay)

	require.NoError(t, err, "Save error must not be returned from Run")
}

func TestCDCConsumer_MultipleEvents_ReplayAdvancesPerEvent(t *testing.T) {
	// Three events in sequence — replay cursor must be committed after EACH one,
	// not just at the end of the batch.
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-1")}, ReplayID: []byte("seq-1")},
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-2")}, ReplayID: []byte("seq-2")},
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-3")}, ReplayID: []byte("seq-3")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		&subjectCapturingPublisher{},
		"",
		// Return empty — absent IDs route to delete, which still advances replay correctly.
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{}),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	require.Len(t, replay.savedAll, 3, "replay cursor must be committed once per event")
	assert.Equal(t, []byte("seq-1"), replay.savedAll[0])
	assert.Equal(t, []byte("seq-2"), replay.savedAll[1])
	assert.Equal(t, []byte("seq-3"), replay.savedAll[2])
}

// ── CDCChangeType fallthrough tests ──────────────────────────────────────────

func TestCDCConsumer_Asset_Undelete_TreatedAsUpsert(t *testing.T) {
	pm := &model.ProjectMembership{
		UID:        sfid("pm-undelete"),
		B2BOrgUID:  sfid("org-undelete"),
		ProjectUID: "project-undelete",
	}
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUndelete, RecordIDs: []string{sfid("pm-undelete")}, ReplayID: []byte("ru1")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "UNDELETE must re-publish indexer (treated as upsert)")
	assert.NotEmpty(t, pub.access, "UNDELETE must publish FGA access (upsert path, not delete path)")
	assert.Equal(t, 1, invalidator.MembershipCalls, "cache must be invalidated on UNDELETE")
}

func TestCDCConsumer_Asset_GapOverflow_TreatedAsUpsert(t *testing.T) {
	// GAP_OVERFLOW also falls into the non-delete upsert path.
	pm := &model.ProjectMembership{UID: sfid("pm-gap")}
	contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{{
		UID: sfid("kc-gap"), MembershipUID: pm.UID, Username: "alice",
	}}}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeGapOverflow, RecordIDs: []string{sfid("pm-gap")}, ReplayID: []byte("rg2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCKeyContactGrantIndex(&mock.MockKeyContactGrantIndex{}),
		svc.WithCDCKeyContactsByMembershipReader(contacts),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "GAP_OVERFLOW must trigger re-fetch and re-publish (treated as upsert)")
	assert.False(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"GAP_OVERFLOW is not a restore and must not rebuild key_contact grants")
}

func TestCDCConsumer_Asset_GapDelete_TreatedAsDelete(t *testing.T) {
	// GAP_DELETE must route to the delete path, not the upsert path.
	// Salesforce emits GAP_DELETE when a record is deleted during a CDC
	// overflow gap; treating it as an upsert would leave a ghost document.
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: "GAP_DELETE", RecordIDs: []string{sfid("pm-gapdel")}, ReplayID: []byte("rgd1")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "GAP_DELETE must publish a delete indexer event")
	assert.Equal(t, 1, invalidator.MembershipCalls, "cache must be invalidated on GAP_DELETE")
	// No re-fetch happens on the delete path, so nothing can be reconciled;
	// the only FGA traffic is the withdrawal itself.
	assert.Zero(t, pub.updateAccessCount(), "GAP_DELETE must not emit update_access (no re-fetch)")
	assert.Equal(t, []string{sfid("pm-gapdel")}, pub.deleteAccessUIDs(t, "project_membership"),
		"GAP_DELETE is a real deletion and must withdraw tuples")
}

func TestCDCConsumer_ProjectRole_GapDelete_TreatedAsDelete(t *testing.T) {
	// Same invariant for key_contact: GAP_DELETE must call the delete handler.
	pub := &subjectCapturingPublisher{}
	invalidator := &mock.MockCacheInvalidator{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: "GAP_DELETE", RecordIDs: []string{sfid("kc-gapdel")}, ReplayID: []byte("rgd2")},
		}},
		&fakeB2BOrgReader{},
		invalidator,
		pub,
		"",
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "GAP_DELETE must publish a delete indexer event for key_contact")
	assert.Equal(t, 1, invalidator.KeyContactCalls, "cache must be invalidated on key_contact GAP_DELETE")
}

// ── pkgerrors test helper ─────────────────────────────────────────────────────

// Verify that a batch-fetch error propagates correctly: replay advances, nothing is published.
func TestCDCConsumer_Account_OrgNotFound_AdvancesReplay(t *testing.T) {
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("org-missing")}, ReplayID: []byte("r12")},
		}},
		&fakeB2BOrgReader{orgErr: errors.New("not found")},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		// Batch fetch errors → no publish; error is logged, replay still advances.
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Err: errors.New("not found")}),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))

	assert.Empty(t, pub.indexer, "no indexer on org not found")
	assert.Equal(t, []byte("r12"), replay.saved, "replay must advance even when org not found")
}

// ── LFID resolution + silent provisioning ────────────────────────────────────

// fakeUserReader implements port.UserReader for CDC consumer tests.
type fakeUserReader struct {
	sub string
	err error
}

func (r *fakeUserReader) UsernameByEmail(_ context.Context, _ string) (string, error) {
	return r.sub, r.err
}

func (r *fakeUserReader) UserMetadataByPrincipal(_ context.Context, _ string) (port.UserMetadata, error) {
	return port.UserMetadata{}, nil
}

// newProjectRoleCDCConsumer builds a CDCConsumer wired for a single
// Project_Role__c upsert event keyed by kc.UID. Boring mocks (PM reader,
// org reader, cache invalidator) are pre-filled so each test only passes
// the options it actually cares about via extraOpts.
func newProjectRoleCDCConsumer(
	kc *model.KeyContact,
	pub *subjectCapturingPublisher,
	extraOpts ...svc.CDCConsumerOption,
) *svc.CDCConsumer {
	base := []svc.CDCConsumerOption{
		svc.WithCDCSubscriber(&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{kc.UID}},
		}}),
		svc.WithCDCB2BOrgReader(&fakeB2BOrgReader{}),
		svc.WithCDCCacheInvalidator(&mock.MockCacheInvalidator{}),
		svc.WithCDCPublisher(pub),
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
	}
	return svc.NewCDCConsumer(append(base, extraOpts...)...)
}

func TestCDCConsumer_ProjectRole_Upsert_EmailResolves_GrantsFGAAndProvisions(t *testing.T) {
	// Email resolves to an LFID → FGA member_put published AND AddPrincipal called
	// with SuppressNotification=true (CDC must never email).
	kc := &model.KeyContact{
		UID: sfid("kc-res-1"), MembershipUID: "pm-1",
		B2BOrgUID: "001000000000001AAA", Email: "carol@example.com",
		Role: "Billing Contact",
	}
	pub := &subjectCapturingPublisher{}
	spy := &spyOrgSettings{}

	consumer := newProjectRoleCDCConsumer(kc, pub,
		svc.WithCDCUserReader(&fakeUserReader{sub: "auth0|carol"}),
		svc.WithCDCOrgSettings(spy),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.True(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"FGA member_put must be published when LFID is resolved")
	require.Len(t, spy.adds, 1, "AddPrincipal must be called once")
	assert.True(t, spy.adds[0].SuppressNotification, "CDC provisioning must suppress notification")
	assert.Equal(t, "001000000000001AAA", spy.adds[0].OrgUID)
	assert.Equal(t, "carol@example.com", spy.adds[0].Email)
}

func TestCDCConsumer_ProjectRole_Upsert_EmailNotFound_NoGrantNoProvision(t *testing.T) {
	// UsernameByEmail returns NotFound → Username stays empty → FGA grant skipped;
	// no AddPrincipal call (unregistered contacts stay pending via the invite flow).
	kc := &model.KeyContact{
		UID: sfid("kc-res-2"), MembershipUID: "pm-2",
		B2BOrgUID: "001000000000002AAA", Email: "unknown@example.com",
	}
	pub := &subjectCapturingPublisher{}
	spy := &spyOrgSettings{}

	consumer := newProjectRoleCDCConsumer(kc, pub,
		svc.WithCDCUserReader(&fakeUserReader{err: pkgerrors.NewNotFound("not found")}),
		svc.WithCDCOrgSettings(spy),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.False(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"FGA member_put must NOT be published for unresolved email")
	assert.Empty(t, spy.adds, "AddPrincipal must not be called for unregistered contact")
}

func TestCDCConsumer_ProjectRole_Upsert_NilUserReader_PreservesExistingBehavior(t *testing.T) {
	// nil userReader must not regress existing behavior: a contact with a stored
	// Username still gets FGA member_put; no provisioning attempt is made.
	kc := &model.KeyContact{
		UID: sfid("kc-res-3"), MembershipUID: "pm-3", Username: "auth0|existing",
		B2BOrgUID: "001000000000003AAA", Email: "existing@example.com",
	}
	pub := &subjectCapturingPublisher{}

	consumer := newProjectRoleCDCConsumer(kc, pub) // no extraOpts — nil userReader path

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.True(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"pre-existing Username must still produce FGA member_put even without userReader")
}

// ── Quota guard tests ─────────────────────────────────────────────────────────

func TestCDCConsumer_QuotaGuard_AboveThreshold_SkipsUpsert(t *testing.T) {
	pm := &model.ProjectMembership{UID: sfid("pm-quota-1")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-quota-1")}, ReplayID: []byte("qg1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100}), // 0.96 ≥ 0.95
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, pub.indexer, "quota exceeded must suppress indexer publish")
	assert.Empty(t, pub.access, "quota exceeded must suppress FGA publish")
}

func TestCDCConsumer_QuotaGuard_AtThreshold_SkipsUpsert(t *testing.T) {
	pm := &model.ProjectMembership{UID: sfid("pm-quota-2")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-quota-2")}, ReplayID: []byte("qg2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 95, Limit: 100}), // 0.95 == threshold
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, pub.indexer, "quota at exact threshold must suppress indexer publish")
	assert.Empty(t, pub.access, "quota at exact threshold must suppress FGA publish")
}

func TestCDCConsumer_QuotaGuard_BelowThreshold_Proceeds(t *testing.T) {
	pm := &model.ProjectMembership{UID: sfid("pm-quota-3")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-quota-3")}, ReplayID: []byte("qg3")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 94, Limit: 100}), // 0.94 < 0.95
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "below threshold must proceed and publish indexer")
}

func TestCDCConsumer_QuotaGuard_LimitZero_FailsOpen(t *testing.T) {
	// limit ≤ 0 means the gauge has not yet observed a response — must proceed.
	pm := &model.ProjectMembership{UID: sfid("pm-quota-4")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-quota-4")}, ReplayID: []byte("qg4")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 100, Limit: 0}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "limit=0 (unobserved) must fail open and publish")
}

func TestCDCConsumer_QuotaGuard_NilGauge_FailsOpen(t *testing.T) {
	// No WithCDCQuotaGauge injected — nil gauge must fail open.
	pm := &model.ProjectMembership{UID: sfid("pm-quota-5")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-quota-5")}, ReplayID: []byte("qg5")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		// intentionally no WithCDCQuotaGauge
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "nil gauge must fail open and publish")
}

func TestCDCConsumer_QuotaGuard_DeleteBypassesQuota(t *testing.T) {
	// DELETE events must publish even when quota is 100% — the delete path never
	// calls quotaExceeded and must always fire for index/FGA convergence.
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeDelete, RecordIDs: []string{sfid("pm-quota-del")}, ReplayID: []byte("qg6")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 100, Limit: 100}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.NotEmpty(t, pub.indexer, "DELETE must always publish regardless of quota state")
}

// ── Quota stale-refresh matrix tests ──────────────────────────────────────────

func TestCDCConsumer_QuotaRefresh_Fresh_NoActiveRefresh(t *testing.T) {
	// Reading is within the staleness window — the guard must decide from the
	// existing snapshot without issuing an active refresh.
	t.Setenv("CDC_QUOTA_REFRESH_STALE_AFTER", "1h")
	pm := &model.ProjectMembership{UID: sfid("pm-refresh-fresh")}
	pub := &subjectCapturingPublisher{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()} // fresh, ≥ threshold

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-refresh-fresh")}, ReplayID: []byte("qr1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(gauge),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 0, gauge.RefreshCalls, "a fresh reading must not trigger an active refresh")
	assert.Empty(t, pub.indexer, "fresh reading above threshold must still skip")
}

func TestCDCConsumer_QuotaRefresh_StaleAndRecovered_Proceeds(t *testing.T) {
	// Reading is stale; the active refresh reports usage back below threshold —
	// the guard must refresh-then-proceed (no skip, no repair-queue write).
	t.Setenv("CDC_QUOTA_REFRESH_STALE_AFTER", "200ms")
	pm := &model.ProjectMembership{UID: sfid("pm-refresh-recovered")}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{
		Current: 96, Limit: 100, ObservedAt: time.Now().Add(-time.Second), // stale, currently ≥ threshold
		RefreshFn: func(_ context.Context) (port.QuotaSnapshot, error) {
			return port.QuotaSnapshot{Current: 10, Limit: 100, ObservedAt: time.Now(), Generation: 2}, nil
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-refresh-recovered")}, ReplayID: []byte("qr2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(gauge),
		svc.WithCDCRepairStore(repairStore),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 1, gauge.RefreshCalls, "a stale reading must trigger exactly one active refresh")
	assert.NotEmpty(t, pub.indexer, "recovered quota after refresh must proceed and publish")
	assert.Empty(t, repairStore.Puts, "a proceeded event must not write a repair marker")
}

func TestCDCConsumer_QuotaRefresh_StaleAndExhausted_Skips(t *testing.T) {
	// Reading is stale; the active refresh confirms quota is still exhausted —
	// the guard must refresh-then-skip and queue a repair marker.
	t.Setenv("CDC_QUOTA_REFRESH_STALE_AFTER", "200ms")
	pm := &model.ProjectMembership{UID: sfid("pm-refresh-exhausted")}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{
		Current: 96, Limit: 100, ObservedAt: time.Now().Add(-time.Second), // stale
		RefreshFn: func(_ context.Context) (port.QuotaSnapshot, error) {
			return port.QuotaSnapshot{Current: 98, Limit: 100, ObservedAt: time.Now(), Generation: 2}, nil // still exhausted
		},
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-refresh-exhausted")}, ReplayID: []byte("qr3")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(gauge),
		svc.WithCDCRepairStore(repairStore),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 1, gauge.RefreshCalls, "a stale reading must trigger exactly one active refresh")
	assert.Empty(t, pub.indexer, "confirmed-exhausted quota after refresh must skip")
	require.Len(t, repairStore.Puts, 1, "a skipped upsert must queue exactly one repair marker")
	assert.Equal(t, "project_membership", repairStore.Puts[0].Type)
}

func TestCDCConsumer_QuotaRefresh_RequestError_FallsBackToLastReading(t *testing.T) {
	// Reading is stale; the active refresh request itself fails — the guard must
	// evaluate the last known (stale but valid) reading rather than fail open.
	t.Setenv("CDC_QUOTA_REFRESH_STALE_AFTER", "200ms")
	pm := &model.ProjectMembership{UID: sfid("pm-refresh-err")}
	pub := &subjectCapturingPublisher{}
	gauge := &mock.MockSalesforceQuotaGauge{
		Current: 97, Limit: 100, ObservedAt: time.Now().Add(-time.Second), // stale, ≥ threshold
		RefreshErr: errors.New("salesforce: quota refresh request failed"),
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-refresh-err")}, ReplayID: []byte("qr4")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(gauge),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 1, gauge.RefreshCalls, "a stale reading must still attempt exactly one active refresh")
	assert.Empty(t, pub.indexer, "a failed refresh must fall back to the last (exhausted) reading and skip")
}

func TestCDCConsumer_QuotaRefresh_NeverObserved_FailsOpenAfterFailedRefresh(t *testing.T) {
	// No valid reading has ever been observed, and the active refresh attempt
	// also fails — the guard must fail open (never block on an unreadable gauge).
	t.Setenv("CDC_QUOTA_REFRESH_STALE_AFTER", "200ms")
	pm := &model.ProjectMembership{UID: sfid("pm-refresh-never")}
	pub := &subjectCapturingPublisher{}
	gauge := &mock.MockSalesforceQuotaGauge{
		Current: 0, Limit: 0, // never observed (Limit ≤ 0 ⇒ Observed() == false)
		RefreshErr: errors.New("salesforce: no route to host"),
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-refresh-never")}, ReplayID: []byte("qr5")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(gauge),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 1, gauge.RefreshCalls)
	assert.NotEmpty(t, pub.indexer, "never-observed + failed refresh must fail open and publish")
}

func TestCDCConsumer_QuotaRefresh_FailedAttempt_ThrottledToOncePerWindow(t *testing.T) {
	// Two events in the same window, both against a stale+failing gauge: the
	// second call must not re-attempt the refresh (throttled to once/window).
	t.Setenv("CDC_QUOTA_REFRESH_STALE_AFTER", "1h") // window long enough to span both events
	pm1 := &model.ProjectMembership{UID: sfid("pm-throttle-1")}
	pm2 := &model.ProjectMembership{UID: sfid("pm-throttle-2")}
	pub := &subjectCapturingPublisher{}
	gauge := &mock.MockSalesforceQuotaGauge{
		Current: 96, Limit: 100, ObservedAt: time.Now().Add(-time.Hour), // stale, ≥ threshold
		RefreshErr: errors.New("salesforce: rate limited"),
	}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-throttle-1")}, ReplayID: []byte("qr6a")},
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-throttle-2")}, ReplayID: []byte("qr6b")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm1, pm2}}),
		svc.WithCDCQuotaGauge(gauge),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, 1, gauge.RefreshCalls, "the second event within the same window must not re-attempt a failed refresh")
	assert.Empty(t, pub.indexer, "both events must skip: first via failed-refresh fallback, second via throttled stale reading")
}

// ── Skip mapping / repair-queue write tests ───────────────────────────────────

func TestCDCConsumer_RepairMapping_AllThreeEntitiesMapCorrectly(t *testing.T) {
	tests := []struct {
		entity   string
		wantType string
		wireOpt  func(uid string) svc.CDCConsumerOption
		channel  string
	}{
		{
			entity:   "Account",
			wantType: "b2b_org",
			channel:  "/data/AccountChangeEvent",
			wireOpt: func(uid string) svc.CDCConsumerOption {
				return svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{{UID: uid}}})
			},
		},
		{
			entity:   "Asset",
			wantType: "project_membership",
			channel:  "/data/AssetChangeEvent",
			wireOpt: func(uid string) svc.CDCConsumerOption {
				return svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{{UID: uid}}})
			},
		},
		{
			entity:   "Project_Role__c",
			wantType: "key_contact",
			channel:  "/data/ProjectRoleChangeEvent",
			wireOpt: func(uid string) svc.CDCConsumerOption {
				return svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{{UID: uid}}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.entity, func(t *testing.T) {
			id := sfid("repair-map-" + tt.entity)
			pub := &subjectCapturingPublisher{}
			repairStore := &mock.MockCDCRepairStore{}

			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{
					{Entity: tt.entity, ChangeType: model.CDCChangeUpdate, RecordIDs: []string{id}, ReplayID: []byte("map-" + tt.entity)},
				}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				pub,
				"",
				tt.wireOpt(id),
				svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()}),
				svc.WithCDCRepairStore(repairStore),
			)

			require.NoError(t, consumer.Run(context.Background(), tt.channel, &fakeReplayStore{}))

			require.Len(t, repairStore.Puts, 1)
			assert.Equal(t, tt.wantType, repairStore.Puts[0].Type)
			assert.Equal(t, id, repairStore.Puts[0].SFID)
		})
	}
}

func TestCDCConsumer_RepairMapping_PartialKVFailure_LogsOnlyFailedIDs(t *testing.T) {
	// Two IDs skipped in the same batch; the repair store fails to write the
	// first and succeeds on the second. The successful marker must remain (no
	// rollback) and the cursor must still advance.
	id1 := sfid("repair-fail-1")
	id2 := sfid("repair-fail-2")
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()}

	// Wrap repairStore with a decorator that fails only the first PutPending
	// call, so the batch of two IDs exercises a partial (not total) failure.
	failingStore := &failFirstPutRepairStore{inner: repairStore}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{id1, id2}, ReplayID: []byte("pf1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{}),
		svc.WithCDCQuotaGauge(gauge),
		svc.WithCDCRepairStore(failingStore),
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	require.Len(t, repairStore.Puts, 1, "only the successful marker write must be recorded by the inner store")
	assert.Equal(t, id2, repairStore.Puts[0].SFID, "the second ID's marker must be written despite the first failing")
	assert.NotEmpty(t, replay.saved, "the replay cursor must still advance despite a partial repair-queue write failure")
}

// failFirstPutRepairStore wraps a port.CDCRepairStore and fails only the first
// PutPending call, delegating all others (including the first ID) to inner.
type failFirstPutRepairStore struct {
	inner     port.CDCRepairStore
	putCalled bool
}

func (s *failFirstPutRepairStore) PutPending(ctx context.Context, reindexType, sfid string) error {
	if !s.putCalled {
		s.putCalled = true
		return errors.New("kv write failed")
	}
	return s.inner.PutPending(ctx, reindexType, sfid)
}

func (s *failFirstPutRepairStore) ListPending(ctx context.Context, reindexType string, limit int) ([]port.RepairMarker, error) {
	return s.inner.ListPending(ctx, reindexType, limit)
}

func (s *failFirstPutRepairStore) DeletePending(ctx context.Context, reindexType, sfid string, revision uint64) error {
	return s.inner.DeletePending(ctx, reindexType, sfid, revision)
}

func TestCDCConsumer_RepairMapping_NoRepairStore_StillAdvancesCursor(t *testing.T) {
	// When repairStore is nil (mock mode / not configured), a skip must still
	// advance the cursor — the repair queue is a best-effort backstop, not a
	// correctness dependency.
	pm := &model.ProjectMembership{UID: sfid("pm-no-repair-store")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-no-repair-store")}, ReplayID: []byte("nr1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()}),
		// no WithCDCRepairStore
	)

	replay := &fakeReplayStore{}
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	assert.Empty(t, pub.indexer, "quota exceeded must still skip even without a repair store")
	assert.NotEmpty(t, replay.saved, "cursor must advance even when the repair store is not configured")
}

func TestCDCConsumer_RepairMapping_UnmappableEntity_NotQueued(t *testing.T) {
	// An entity with no reindexTypeForCDCEntity mapping must not attempt a
	// repair-queue write (there would be no valid reindex_type to key it by).
	// handle() dispatches only the three known entities, so this is exercised
	// indirectly by confirming an unmapped entity produces no repair Puts even
	// when everything else (quota, repair store) is wired.
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "SomeUnhandledEntity__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("unmapped-1")}, ReplayID: []byte("um1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()}),
		svc.WithCDCRepairStore(repairStore),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/UnhandledChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, repairStore.Puts, "an entity outside the handled dispatch set must never reach the repair queue")
}

func TestCDCConsumer_RepairMapping_MalformedID_NotQueued(t *testing.T) {
	// A record ID that cannot be normalized to an 18-char SFID is dropped by
	// partitionRecordIDs before the quota check ever runs, so it must never
	// reach the repair queue.
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{"not-a-valid-sfid"}, ReplayID: []byte("mid1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()}),
		svc.WithCDCRepairStore(repairStore),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, repairStore.Puts, "a malformed record ID must never be queued for repair")
}

func TestCDCConsumer_RepairMapping_15CharID_NormalizesTo18BeforeQueuing(t *testing.T) {
	// A raw 15-char case-sensitive SFID must be normalized to its canonical
	// 18-char form before it is written to the repair queue — the drain path
	// (and every other KV key in this bucket) keys exclusively on 18-char IDs.
	id18 := sfid("norm-15-to-18")
	raw15 := id18[:15]
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{raw15}, ReplayID: []byte("norm1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{}),
		svc.WithCDCQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 96, Limit: 100, ObservedAt: time.Now()}),
		svc.WithCDCRepairStore(repairStore),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.Len(t, repairStore.Puts, 1)
	assert.Equal(t, id18, repairStore.Puts[0].SFID, "the queued marker must use the canonical 18-char SFID, not the raw 15-char input")
	assert.Len(t, repairStore.Puts[0].SFID, 18)
}

// ── Absent-from-SOQL → delete convergence tests ───────────────────────────────

func TestCDCConsumer_Asset_AbsentFromSOQL_RoutesToDelete(t *testing.T) {
	// Batch event with two IDs: SOQL only returns pm-present. pm-absent is missing
	// (soft-deleted or no longer holds a membership Asset) and must be routed to
	// the delete path for index/FGA convergence, not silently skipped.
	pmPresent := &model.ProjectMembership{UID: sfid("pm-present")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-present"), sfid("pm-absent")}, ReplayID: []byte("ab1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pmPresent}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.Len(t, pub.indexerMessages, 2, "both IDs must produce an indexer event")
	// absent ID fires delete first (absent loop runs before present loop)
	assert.Equal(t, "deleted", pub.indexerAction(0), "absent ID must produce ActionDeleted")
	assert.Equal(t, "updated", pub.indexerAction(1), "present ID must produce ActionUpdated")
}

func TestCDCConsumer_Account_AbsentFromSOQL_RoutesToDelete(t *testing.T) {
	// Same convergence guarantee for Account / b2b_org.
	orgPresent := &model.B2BOrg{UID: sfid("org-present")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("org-present"), sfid("org-absent")}, ReplayID: []byte("ab2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{orgPresent}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	require.Len(t, pub.indexerMessages, 2, "both account IDs must produce an indexer event")
	assert.Equal(t, "deleted", pub.indexerAction(0), "absent account must produce ActionDeleted")
	assert.Equal(t, "updated", pub.indexerAction(1), "present account must produce ActionUpdated")
}

func TestCDCConsumer_ProjectRole_AbsentFromSOQL_RoutesToDelete(t *testing.T) {
	// Same convergence guarantee for Project_Role__c / key_contact.
	kcPresent := &model.KeyContact{UID: sfid("kc-present")}
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("kc-present"), sfid("kc-absent")}, ReplayID: []byte("ab3")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kcPresent}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.Len(t, pub.indexerMessages, 2, "both key_contact IDs must produce an indexer event")
	assert.Equal(t, "deleted", pub.indexerAction(0), "absent key_contact must produce ActionDeleted")
	assert.Equal(t, "updated", pub.indexerAction(1), "present key_contact must produce ActionUpdated")
}

// ── Conv-error SFID → not routed to delete (Finding 3) ───────────────────────

func TestCDCConsumer_Asset_ConvErrSFID_NotRoutedToDelete(t *testing.T) {
	// A SFID that SOQL returned but the batch reader could not convert (e.g. the
	// sObject row had an unexpected shape) is reported in ConvErrSFIDs. The
	// consumer must NOT route it to the delete handler — the record exists in
	// Salesforce; it just couldn't be materialised in this batch pass.
	// /admin/reindex can repair it later.
	badID := sfid("pm-bad")
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{badID}, ReplayID: []byte("cv1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{
			Memberships:  nil,
			ConvErrSFIDs: []string{badID},
		}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	// The SFID is in seenButFailed → returned set contains it → absent check skips it.
	assert.Empty(t, pub.indexerMessages,
		"a conv-error SFID must NOT produce ActionDeleted; got indexer calls: %v", pub.indexer)
}

// ── project_uid resolution parity (CDC vs backfill/HTTP) ─────────────────────

// indexerTags extracts the top-level "tags" of the i-th indexer message.
func (p *subjectCapturingPublisher) indexerTags(i int) []string {
	if i >= len(p.indexerMessages) {
		return nil
	}
	msg, ok := p.indexerMessages[i].(*model.MemberIndexerMessage)
	if !ok || msg == nil {
		return nil
	}
	return msg.Tags
}

// indexerParentRefs extracts indexing_config.parent_refs of the i-th indexer message.
func (p *subjectCapturingPublisher) indexerParentRefs(i int) []string {
	if i >= len(p.indexerMessages) {
		return nil
	}
	msg, ok := p.indexerMessages[i].(*model.MemberIndexerMessage)
	if !ok || msg == nil || msg.IndexingConfig == nil {
		return nil
	}
	return msg.IndexingConfig.ParentRefs
}

// fgaMessages type-asserts all captured access payloads to fgatypes.GenericFGAMessage.
func (p *subjectCapturingPublisher) fgaMessages(t *testing.T) []fgatypes.GenericFGAMessage {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]fgatypes.GenericFGAMessage, 0, len(p.accessMessages))
	for _, m := range p.accessMessages {
		msg, ok := m.(fgatypes.GenericFGAMessage)
		require.True(t, ok, "expected fgatypes.GenericFGAMessage, got %T", m)
		out = append(out, msg)
	}
	return out
}

func TestCDCConsumer_Asset_Upsert_StampsResolvedProjectUID(t *testing.T) {
	pm := &model.ProjectMembership{
		UID: sfid("pm-res"), B2BOrgUID: "org-res",
		ProjectSlug: "jupiter", ProjectSFID: "a0p-jupiter",
	}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "proj-uuid-123", Slug: "jupiter"})

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-res")}, ReplayID: []byte("pu1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.indexerMessages)
	assert.True(t, slices.Contains(pub.indexerTags(0), "project_uid:proj-uuid-123"),
		"indexer tags must carry the resolved project_uid; got %v", pub.indexerTags(0))
	assert.Contains(t, pub.indexerParentRefs(0), "project:proj-uuid-123",
		"indexer parent_refs must carry the resolved project ref")

	var projectRef bool
	for _, m := range pub.fgaMessages(t) {
		if m.ObjectType != "project_membership" {
			continue
		}
		data, ok := m.Data.(fgatypes.GenericAccessData)
		if !ok {
			continue
		}
		if refs, ok := data.References["project"]; ok {
			assert.Equal(t, []string{"project:proj-uuid-123"}, refs)
			projectRef = true
		}
	}
	assert.True(t, projectRef, "project_membership FGA must carry the resolved project reference")
}

func TestCDCConsumer_Asset_Upsert_MixedCaseProjectSlug_StampsResolvedProjectUID(t *testing.T) {
	pm := &model.ProjectMembership{
		UID: sfid("pm-toip"), B2BOrgUID: "org-toip",
		ProjectSlug: "ToIP", ProjectSFID: "a0p-toip",
	}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "proj-uuid-toip", Slug: "toip"})

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-toip")}, ReplayID: []byte("toip1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.indexerMessages)
	assert.True(t, slices.Contains(pub.indexerTags(0), "project_uid:proj-uuid-toip"),
		"mixed-case slug must resolve; got tags %v", pub.indexerTags(0))
}

func TestCDCConsumer_Asset_Upsert_ResolverFailure_SkipsIndexerPublishesFGAOnly(t *testing.T) {
	pm := &model.ProjectMembership{
		UID: sfid("pm-nores"), B2BOrgUID: "org-nores", ProjectSlug: "unknown-slug",
	}
	// Empty resolver → UIDFromSlug returns an error for the unseeded slug.
	resolver := mock.NewMockProjectResolver()

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-nores")}, ReplayID: []byte("pu2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, pub.indexerMessages, "indexer publish must be skipped when project_uid resolution fails")
	assert.NotEmpty(t, pub.access, "OpenFGA publish must still run when project_uid resolution fails")
	var sawPM bool
	for _, m := range pub.fgaMessages(t) {
		if m.ObjectType != "project_membership" {
			continue
		}
		sawPM = true
		data, ok := m.Data.(fgatypes.GenericAccessData)
		require.True(t, ok)
		_, hasProjectRef := data.References["project"]
		assert.False(t, hasProjectRef, "FGA must not carry a project ref when project_uid is unresolved")
		assert.Contains(t, data.ExcludeRelations, "project")
	}
	assert.True(t, sawPM, "project_membership OpenFGA must publish on resolver failure")
}

func TestCDCConsumer_ProjectRole_Upsert_ResolverFailure_SkipsIndexerPublishesFGAOnly(t *testing.T) {
	kc := &model.KeyContact{
		UID: sfid("kc-nores"), MembershipUID: "pm-1", B2BOrgUID: "001000000000001AAA",
		ProjectSlug: "unknown-slug", Username: "jdoe",
	}
	resolver := mock.NewMockProjectResolver()

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("kc-nores")}, ReplayID: []byte("pu5")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, pub.indexerMessages, "indexer publish must be skipped when project_uid resolution fails")
	assert.NotEmpty(t, pub.access, "key_contact OpenFGA member_put must still publish when project_uid resolution fails")
}

func TestCDCConsumer_ProjectRole_ResolverFailure_OpenFGAOnlyNoProvisioning(t *testing.T) {
	kc := &model.KeyContact{
		UID: sfid("kc-prov"), MembershipUID: "pm-1", B2BOrgUID: "001000000000001AAA",
		Email: "carol@example.com", Role: "Billing Contact",
		ProjectSlug: "unknown-slug",
	}
	resolver := mock.NewMockProjectResolver()
	spy := &spyOrgSettings{}

	pub := &subjectCapturingPublisher{}
	consumer := newProjectRoleCDCConsumer(kc, pub,
		svc.WithCDCUserReader(&fakeUserReader{sub: "auth0|carol"}),
		svc.WithCDCOrgSettings(spy),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.Empty(t, pub.indexerMessages, "indexer publish must be skipped when project_uid resolution fails")
	assert.NotEmpty(t, pub.access, "OpenFGA member_put must publish when project_uid resolution fails")
	assert.Empty(t, spy.adds, "org-dashboard provisioning must not run when project_uid is unresolved")
}

func TestCDCConsumer_Asset_Upsert_PreSetProjectUID_NotReResolved(t *testing.T) {
	pm := &model.ProjectMembership{
		UID: sfid("pm-preset"), B2BOrgUID: "org-preset",
		ProjectSlug: "jupiter", ProjectUID: "preset-uid",
	}
	// Resolver maps the slug to a DIFFERENT uid; it must NOT be consulted.
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolver-uid", Slug: "jupiter"})

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("pm-preset")}, ReplayID: []byte("pu3")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.indexerMessages)
	assert.True(t, slices.Contains(pub.indexerTags(0), "project_uid:preset-uid"),
		"pre-set project_uid must be preserved (not re-resolved); got %v", pub.indexerTags(0))
}

func TestCDCConsumer_ProjectRole_Upsert_StampsResolvedProjectUID(t *testing.T) {
	kc := &model.KeyContact{
		UID: sfid("kc-res"), MembershipUID: "pm-1", B2BOrgUID: "001000000000001AAA",
		ProjectSlug: "jupiter",
	}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "proj-uuid-456", Slug: "jupiter"})

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{sfid("kc-res")}, ReplayID: []byte("pu4")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCKeyContactBatchReader(&mock.MockKeyContactBatchReader{Contacts: []*model.KeyContact{kc}}),
		svc.WithCDCProjectResolver(resolver),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	require.NotEmpty(t, pub.indexerMessages)
	assert.True(t, slices.Contains(pub.indexerTags(0), "project_uid:proj-uuid-456"),
		"key_contact indexer tags must carry the resolved project_uid; got %v", pub.indexerTags(0))
	assert.Contains(t, pub.indexerParentRefs(0), "project:proj-uuid-456",
		"key_contact indexer parent_refs must carry the resolved project ref")
}

// ── b2b_org parent hierarchy tuples (CDC) ────────────────────────────────────

func TestCDCConsumer_Account_ColdCreate_EmitsParentAndChildListTuples(t *testing.T) {
	childUID := sfid("child-org")
	parentUID := sfid("parent-org")
	childOrg := &model.B2BOrg{UID: childUID, ParentUID: parentUID}

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{childUID}, ReplayID: []byte("ph1")},
		}},
		// GetB2BOrg returns the post-change org (cold cache) → oldParent == newParent
		// → publishB2BOrgUpsertEvents emits no reparenting messages.
		&fakeB2BOrgReader{org: childOrg, childMap: map[string][]string{parentUID: {childUID}}},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{childOrg}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	msgs := pub.fgaMessages(t)
	var parentTuple, childList bool
	for _, m := range msgs {
		if m.ObjectType != "b2b_org" {
			continue
		}
		data, ok := m.Data.(fgatypes.GenericAccessData)
		if !ok {
			continue
		}
		if data.UID == childUID {
			if refs, ok := data.References["parent"]; ok && len(refs) == 1 && refs[0] == "b2b_org:"+parentUID {
				parentTuple = true
			}
		}
		if data.UID == parentUID {
			if refs, ok := data.References["child"]; ok && slices.Contains(refs, "b2b_org:"+childUID) {
				childList = true
			}
		}
	}
	assert.True(t, parentTuple, "cold-cache create must emit the child's parent tuple")
	assert.True(t, childList, "cold-cache create must emit the parent's child-list tuple")
}

func TestCDCConsumer_Account_RootOrg_NoParentTuple(t *testing.T) {
	rootUID := sfid("root-org")
	rootOrg := &model.B2BOrg{UID: rootUID} // no ParentUID

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{rootUID}, ReplayID: []byte("ph2")},
		}},
		&fakeB2BOrgReader{org: rootOrg},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{rootOrg}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	for _, m := range pub.fgaMessages(t) {
		if m.ObjectType != "b2b_org" {
			continue
		}
		data, ok := m.Data.(fgatypes.GenericAccessData)
		if !ok {
			continue
		}
		_, hasParent := data.References["parent"]
		assert.False(t, hasParent, "a root org must not emit a parent tuple; got %+v", data.References)
	}
}

func TestCDCConsumer_Account_Reparent_CleansUpOldParentChildList(t *testing.T) {
	orgUID := sfid("org-move")
	preOrg := &model.B2BOrg{UID: orgUID, ParentUID: "old-parent"}
	postOrg := &model.B2BOrg{UID: orgUID, ParentUID: "new-parent"}

	reader := &reparentingB2BOrgReader{
		preOrg:  preOrg,
		postOrg: postOrg,
		children: map[string][]string{
			"old-parent": {"sibling-org"},
			"new-parent": {},
		},
	}

	pub := &subjectCapturingPublisher{}
	consumer := svc.NewCDCConsumer(
		svc.WithCDCSubscriber(&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{orgUID}, ReplayID: []byte("ph3")},
		}}),
		svc.WithCDCB2BOrgReader(reader),
		svc.WithCDCCacheInvalidator(&mock.MockCacheInvalidator{}),
		svc.WithCDCPublisher(pub),
		svc.WithCDCGlobalOrgAdminTeamUID(""),
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{postOrg}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	var oldParentCleanup bool
	for _, m := range pub.fgaMessages(t) {
		if m.ObjectType != "b2b_org" {
			continue
		}
		data, ok := m.Data.(fgatypes.GenericAccessData)
		if !ok || data.UID != "old-parent" {
			continue
		}
		if refs, ok := data.References["child"]; ok && slices.Contains(refs, "b2b_org:sibling-org") {
			oldParentCleanup = true
		}
	}
	assert.True(t, oldParentCleanup, "reparent must re-publish the old parent's child list without the moved org")
}

// ── delete_access purge on genuine CDC deletes ───────────────────────────────

// deleteAccessUIDs returns the UIDs carried by every delete_access message
// published for the given object type. Filtering on the subject rather than the
// payload keeps the helper honest: a message sent to the wrong subject is
// invisible to fga-sync, so it must be invisible here too.
func (p *subjectCapturingPublisher) deleteAccessUIDs(t *testing.T, objectType string) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	var uids []string
	for i, subject := range p.access {
		if subject != constants.FGASyncDeleteAccessSubject {
			continue
		}
		msg, ok := p.accessMessages[i].(fgatypes.GenericFGAMessage)
		require.True(t, ok, "delete_access payload must be a GenericFGAMessage")
		require.Equal(t, "delete_access", msg.Operation)
		if msg.ObjectType != objectType {
			continue
		}
		data, ok := msg.Data.(fgatypes.GenericDeleteData)
		require.True(t, ok, "delete_access must carry GenericDeleteData")
		uids = append(uids, data.UID)
	}
	return uids
}

// allAccessUIDs returns the subject UID of every FGA message published,
// whatever its operation. Used to assert that a given object was left alone
// entirely, rather than merely spared one particular operation.
func (p *subjectCapturingPublisher) allAccessUIDs(t *testing.T) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	var uids []string
	for _, raw := range p.accessMessages {
		msg, ok := raw.(fgatypes.GenericFGAMessage)
		require.True(t, ok, "every FGA payload must be a GenericFGAMessage")
		switch data := msg.Data.(type) {
		case fgatypes.GenericDeleteData:
			uids = append(uids, data.UID)
		case fgatypes.GenericAccessData:
			uids = append(uids, data.UID)
		case fgatypes.GenericMemberData:
			uids = append(uids, data.UID)
		default:
			t.Fatalf("unhandled FGA data payload %T", msg.Data)
		}
	}
	return uids
}

// updateAccessCount reports how many update_access messages were published.
func (p *subjectCapturingPublisher) updateAccessCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for _, subject := range p.access {
		if subject == constants.FGASyncUpdateAccessSubject {
			n++
		}
	}
	return n
}

// runAccountChange drives a single Account CDC event through the consumer.
func runAccountChange(t *testing.T, pub *subjectCapturingPublisher, ct model.CDCChangeType, ids ...string) {
	t.Helper()
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: ct, RecordIDs: ids, ReplayID: []byte("dl-acct")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{}),
	)
	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
}

// runAssetChange drives a single Asset CDC event through the consumer.
func runAssetChange(t *testing.T, pub *subjectCapturingPublisher, ct model.CDCChangeType, ids ...string) {
	t.Helper()
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: ct, RecordIDs: ids, ReplayID: []byte("dl-asset")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{}),
	)
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))
}

// TestCDCConsumer_Account_Delete_PublishesDeleteAccess covers both change types
// isDelete accepts. GAP_DELETE is included deliberately: it reaches the same
// delete path, so omitting it would leave half the purge untested.
func TestCDCConsumer_Account_Delete_PublishesDeleteAccess(t *testing.T) {
	for _, ct := range []model.CDCChangeType{model.CDCChangeDelete, model.CDCChangeGapDelete} {
		t.Run(string(ct), func(t *testing.T) {
			pub := &subjectCapturingPublisher{}
			runAccountChange(t, pub, ct, sfid("org-purge"))

			assert.Equal(t, []string{sfid("org-purge")}, pub.deleteAccessUIDs(t, "b2b_org"),
				"a genuinely deleted org must have its tuples withdrawn")
		})
	}
}

// TestCDCConsumer_Account_Delete_UIDIsNormalized guards the boundary contract:
// OpenFGA object IDs are written from the 18-char SFID, so publishing the raw
// 15-char form would target an object that holds no tuples and purge nothing,
// while still reporting success.
func TestCDCConsumer_Account_Delete_UIDIsNormalized(t *testing.T) {
	full := sfid("org-normalize")
	pub := &subjectCapturingPublisher{}
	runAccountChange(t, pub, model.CDCChangeDelete, full[:15])

	uids := pub.deleteAccessUIDs(t, "b2b_org")
	require.Len(t, uids, 1)
	assert.Equal(t, full, uids[0], "delete_access must carry the normalized 18-char SFID")
	assert.Len(t, uids[0], 18)
}

// TestCDCConsumer_Account_Delete_StillPublishesIndexerDelete pins that the
// purge is added alongside the index tombstone, not in place of it.
func TestCDCConsumer_Account_Delete_StillPublishesIndexerDelete(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	runAccountChange(t, pub, model.CDCChangeDelete, sfid("org-idx"))

	require.Len(t, pub.indexerMessages, 1, "the index tombstone must still be published")
	assert.Equal(t, "deleted", pub.indexerAction(0))
	data, isStr := pub.indexerDataIsString(0)
	assert.True(t, isStr, "delete indexer data must remain the object ID string")
	assert.Equal(t, sfid("org-idx"), data)
}

// TestCDCConsumer_Asset_Delete_PublishesDeleteAccess covers the membership
// purge. Unlike the b2b_org path this branch published no FGA message at all
// before the change, so nothing is being replaced.
func TestCDCConsumer_Asset_Delete_PublishesDeleteAccess(t *testing.T) {
	for _, ct := range []model.CDCChangeType{model.CDCChangeDelete, model.CDCChangeGapDelete} {
		t.Run(string(ct), func(t *testing.T) {
			pub := &subjectCapturingPublisher{}
			runAssetChange(t, pub, ct, sfid("pm-purge"))

			assert.Equal(t, []string{sfid("pm-purge")}, pub.deleteAccessUIDs(t, "project_membership"),
				"a genuinely deleted membership must have its tuples withdrawn")
			assert.Zero(t, pub.updateAccessCount(),
				"the membership delete path writes no update_access")
		})
	}
}

// TestCDCConsumer_Asset_Delete_StillPublishesIndexerDelete pins the same
// alongside-not-instead-of property for the membership path.
func TestCDCConsumer_Asset_Delete_StillPublishesIndexerDelete(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	runAssetChange(t, pub, model.CDCChangeDelete, sfid("pm-idx"))

	require.Len(t, pub.indexerMessages, 1, "the index tombstone must still be published")
	assert.Equal(t, "deleted", pub.indexerAction(0))
	data, isStr := pub.indexerDataIsString(0)
	assert.True(t, isStr, "delete indexer data must remain the object ID string")
	assert.Equal(t, sfid("pm-idx"), data)
}

// ── Absence must never withdraw access ───────────────────────────────────────
//
// These tests make the delete/absence distinction non-vacuous. Before the purge
// was added they would all pass trivially. They assert the constraint that a
// record merely missing from the periodic query — an org whose membership
// lapsed, a membership whose Product2.Family flipped off — keeps every tuple.

// runAbsentBatch drives an upsert event in which one ID comes back from SOQL and
// one does not, exercising the convergence path for the absent ID.
func runAbsentAccountBatch(t *testing.T, pub *subjectCapturingPublisher) (present, absent string) {
	t.Helper()
	present, absent = sfid("org-live"), sfid("org-lapsed")
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{present, absent}, ReplayID: []byte("us3a")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{{UID: present}}}),
	)
	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
	return present, absent
}

func runAbsentAssetBatch(t *testing.T, pub *subjectCapturingPublisher) (present, absent string) {
	t.Helper()
	present, absent = sfid("pm-live"), sfid("pm-lapsed")
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{present, absent}, ReplayID: []byte("us3b")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{{UID: present}}}),
	)
	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))
	return present, absent
}

// TestCDCConsumer_Account_Absent_PublishesNoDeleteAccess is the guard against
// the worst failure mode this change could introduce: stripping a live
// customer's administrators because a membership renewal lapsed.
func TestCDCConsumer_Account_Absent_PublishesNoDeleteAccess(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	runAbsentAccountBatch(t, pub)

	assert.Empty(t, pub.deleteAccessUIDs(t, "b2b_org"),
		"an org absent from SOQL may still exist — purging it locks out live admins")
}

func TestCDCConsumer_Asset_Absent_PublishesNoDeleteAccess(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	runAbsentAssetBatch(t, pub)

	assert.Empty(t, pub.deleteAccessUIDs(t, "project_membership"),
		"a membership absent from SOQL may still exist")
}

// TestCDCConsumer_Absent_PublishesNoFGAMessageAtAll. The absent path
// previously sent an update_access stub in which every relation landed in
// ExcludeRelations, so it reconciled nothing while looking like it did. Removing
// it makes this stronger assertion available: the convergence path is inert with
// respect to authorization, so no future edit can widen it by accident.
func TestCDCConsumer_Absent_PublishesNoFGAMessageAtAll(t *testing.T) {
	// The record SOQL did return reconciles normally in the same batch, so the
	// assertion is keyed on the absent UID rather than on a message count.
	t.Run("b2b_org", func(t *testing.T) {
		pub := &subjectCapturingPublisher{}
		_, absent := runAbsentAccountBatch(t, pub)
		assert.NotContains(t, pub.allAccessUIDs(t), absent,
			"the absent path must emit no FGA message of any operation")
	})

	t.Run("project_membership", func(t *testing.T) {
		pub := &subjectCapturingPublisher{}
		_, absent := runAbsentAssetBatch(t, pub)
		assert.NotContains(t, pub.allAccessUIDs(t), absent,
			"the absent path must emit no FGA message of any operation")
	})
}

// TestCDCConsumer_Absent_StillPublishesIndexerDelete confirms the change is
// scoped to authorization. Index convergence on absence is unchanged, because a
// tombstoned document is cheaply rebuilt while a revoked grant is not.
func TestCDCConsumer_Absent_StillPublishesIndexerDelete(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	runAbsentAccountBatch(t, pub)

	require.Len(t, pub.indexerMessages, 2, "both IDs must produce an indexer event")
	assert.Equal(t, "deleted", pub.indexerAction(0), "the absent ID is still tombstoned")
	assert.Equal(t, "updated", pub.indexerAction(1))
}

// TestCDCConsumer_MixedBatch_PurgesOnlyTheDeletedRecord is the discriminating
// test: both records converge through the same handler, and only the genuinely
// deleted one may lose its tuples. A regression that routed absence to the
// delete entry point would pass every single-path test above and fail here.
func TestCDCConsumer_MixedBatch_PurgesOnlyTheDeletedRecord(t *testing.T) {
	deleted, present, absent := sfid("org-gone"), sfid("org-still-here"), sfid("org-no-membership")
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete, RecordIDs: []string{deleted}, ReplayID: []byte("mix1")},
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{present, absent}, ReplayID: []byte("mix2")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{{UID: present}}}),
	)
	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, []string{deleted}, pub.deleteAccessUIDs(t, "b2b_org"),
		"exactly one purge, carrying the deleted record's UID and no other")
}

// TestCDCConsumer_Undelete_PublishesNoDeleteAccess raises the stakes on an
// existing invariant. isDelete uses exact equality rather than HasSuffix so
// UNDELETE is not caught; before this change a mis-route wrote a spurious index
// tombstone, but now it would purge every tuple from a live restored object.
// The existing UNDELETE test asserts the upsert path fires — this asserts the
// delete path stays silent, which is the half that now carries the risk.
func TestCDCConsumer_Undelete_PublishesNoDeleteAccess(t *testing.T) {
	t.Run("Account", func(t *testing.T) {
		pub := &subjectCapturingPublisher{}
		runAccountChange(t, pub, model.CDCChangeUndelete, sfid("org-restored"))
		assert.Empty(t, pub.deleteAccessUIDs(t, "b2b_org"),
			"UNDELETE restores a record — purging it would strip a live object")
	})

	t.Run("Asset", func(t *testing.T) {
		pub := &subjectCapturingPublisher{}
		runAssetChange(t, pub, model.CDCChangeUndelete, sfid("pm-restored"))
		assert.Empty(t, pub.deleteAccessUIDs(t, "project_membership"),
			"UNDELETE restores a record — purging it would strip a live object")
	})
}

// TestCDCConsumer_Delete_PublishFailureDoesNotStrandBatch. The delete-path
// handlers now return the publish error rather than swallowing it — per
// MemberPublisher's own delete policy (port.MemberPublisher), and because
// /admin/reindex cannot repair a dropped purge (a genuinely deleted record
// reindexes as outcomeNotFound, which clears any repair marker without
// re-emitting delete_access). This test package is external (service_test)
// and cdc_consumer.go's handleAccountDelete/handleAssetDelete are
// unexported, so the returned error itself isn't assertable from here;
// what this test proves is the property that made swallowing look
// necessary in the first place — that dispatchEntity's per-ID loop already
// logs and continues rather than aborting the event on a handler error, so
// propagating the error carries no batch-stranding risk.
//
// It also asserts the durable side effect that replaces the old "log and
// hope" recovery story: on a publish failure the exact (type, uid) is written
// to the CDC repair queue under a delete-specific type so an operator can
// find and manually re-purge it. This is deliberately not an automated
// retry — reindexItem's targeted repair re-fetches and re-upserts the live
// Salesforce record, which cannot repair a purge for the reason above.
func TestCDCConsumer_Delete_PublishFailureDoesNotStrandBatch(t *testing.T) {
	first, second := sfid("org-fail-1"), sfid("org-fail-2")
	pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
	repair := &mock.MockCDCRepairStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete, RecordIDs: []string{first, second}, ReplayID: []byte("errpub")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCRepairStore(repair),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}),
		"a failed FGA publish must not fail the event")
	assert.Equal(t, []string{first, second}, pub.deleteAccessUIDs(t, "b2b_org"),
		"the second record must still be attempted after the first publish fails")
	assert.Len(t, pub.indexerMessages, 2, "index convergence must be unaffected")

	require.Len(t, repair.Puts, 2, "each failed delete_access publish must be durably recorded for manual recovery")
	for i, id := range []string{first, second} {
		assert.Equal(t, constants.ReindexTypeB2BOrgDeleteAccess, repair.Puts[i].Type,
			"the marker must use the delete-specific type, not the upsert reindex type")
		assert.Equal(t, id, repair.Puts[i].SFID)
	}
}

// TestCDCConsumer_AssetDelete_PublishFailureRecordsRepairMarker mirrors the
// b2b_org case above for project_membership.
func TestCDCConsumer_AssetDelete_PublishFailureRecordsRepairMarker(t *testing.T) {
	id := sfid("pm-fail")
	pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
	repair := &mock.MockCDCRepairStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeDelete, RecordIDs: []string{id}, ReplayID: []byte("pmerrpub")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCRepairStore(repair),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", &fakeReplayStore{}))

	require.Len(t, repair.Puts, 1)
	assert.Equal(t, constants.ReindexTypeProjectMembershipDeleteAccess, repair.Puts[0].Type)
	assert.Equal(t, id, repair.Puts[0].SFID)
}

// flakyRepairStore fails a fixed number of leading PutPending attempts, then
// delegates to the mock.
type flakyRepairStore struct {
	*mock.MockCDCRepairStore
	failFirst int
	attempts  int
}

func (s *flakyRepairStore) PutPending(ctx context.Context, reindexType, sfid string) error {
	s.attempts++
	if s.attempts <= s.failFirst {
		return errors.New("kv: transient write failure")
	}
	return s.MockCDCRepairStore.PutPending(ctx, reindexType, sfid)
}

// TestCDCConsumer_Delete_MarkerWriteRetry verifies that a transient KV failure
// cannot silently discard the only durable record of an unconfirmed purge.
func TestCDCConsumer_Delete_MarkerWriteRetry(t *testing.T) {
	t.Run("transient failure", func(t *testing.T) {
		id := sfid("org-transient")
		pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
		repair := &flakyRepairStore{MockCDCRepairStore: &mock.MockCDCRepairStore{}, failFirst: 2}
		consumer := newTestCDCConsumer(
			&fakeCDCSubscriber{events: []model.CDCEvent{{
				Entity: "Account", ChangeType: model.CDCChangeDelete,
				RecordIDs: []string{id}, ReplayID: []byte("transient"),
			}}},
			&fakeB2BOrgReader{}, &mock.MockCacheInvalidator{}, pub, "",
			svc.WithCDCRepairStore(repair),
		)

		require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
		assert.Equal(t, 3, repair.attempts)
		require.Len(t, repair.Puts, 1)
		assert.Equal(t, id, repair.Puts[0].SFID)
	})

	t.Run("persistent failure uses bounded attempts per event retry", func(t *testing.T) {
		pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
		repair := &flakyRepairStore{MockCDCRepairStore: &mock.MockCDCRepairStore{}, failFirst: 1000}
		consumer := newTestCDCConsumer(
			&fakeCDCSubscriber{events: []model.CDCEvent{{
				Entity: "Account", ChangeType: model.CDCChangeDelete,
				RecordIDs: []string{sfid("org-kvdown")}, ReplayID: []byte("kvdown"),
			}}},
			&fakeB2BOrgReader{}, &mock.MockCacheInvalidator{}, pub, "",
			svc.WithCDCRepairStore(repair),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, consumer.Run(ctx, "/data/AccountChangeEvent", &fakeReplayStore{}),
			context.DeadlineExceeded)
		assert.GreaterOrEqual(t, repair.attempts, 3)
		assert.Zero(t, repair.attempts%3)
		assert.Empty(t, repair.Puts)
	})
}

// TestCDCConsumer_Delete_DeliveryConfirmation verifies broker receipt because
// Access returning nil only proves that the purge reached the local connection.
func TestCDCConsumer_Delete_DeliveryConfirmation(t *testing.T) {
	t.Run("flush failure records repair marker", func(t *testing.T) {
		for _, tc := range []struct {
			entity, channel, reindexType string
		}{
			{"Account", "/data/AccountChangeEvent", constants.ReindexTypeB2BOrgDeleteAccess},
			{"Asset", "/data/AssetChangeEvent", constants.ReindexTypeProjectMembershipDeleteAccess},
		} {
			t.Run(tc.entity, func(t *testing.T) {
				id := sfid("flush-fail")
				pub := &subjectCapturingPublisher{flushErr: errors.New("nats: connection closed")}
				repair := &mock.MockCDCRepairStore{}
				consumer := newTestCDCConsumer(
					&fakeCDCSubscriber{events: []model.CDCEvent{{
						Entity: tc.entity, ChangeType: model.CDCChangeDelete,
						RecordIDs: []string{id}, ReplayID: []byte("flusherr"),
					}}},
					&fakeB2BOrgReader{}, &mock.MockCacheInvalidator{}, pub, "",
					svc.WithCDCRepairStore(repair),
				)

				require.NoError(t, consumer.Run(context.Background(), tc.channel, &fakeReplayStore{}))
				assert.Contains(t, pub.access, constants.FGASyncDeleteAccessSubject)
				require.Len(t, repair.Puts, 1)
				assert.Equal(t, tc.reindexType, repair.Puts[0].Type)
				assert.Equal(t, id, repair.Puts[0].SFID)
			})
		}
	})

	t.Run("success flushes and leaves no marker", func(t *testing.T) {
		pub := &subjectCapturingPublisher{}
		repair := &mock.MockCDCRepairStore{}
		consumer := newTestCDCConsumer(
			&fakeCDCSubscriber{events: []model.CDCEvent{{
				Entity: "Account", ChangeType: model.CDCChangeDelete,
				RecordIDs: []string{sfid("flush-ok")}, ReplayID: []byte("flushok"),
			}}},
			&fakeB2BOrgReader{}, &mock.MockCacheInvalidator{}, pub, "",
			svc.WithCDCRepairStore(repair),
		)

		require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
		assert.Equal(t, 1, pub.flushCount)
		assert.Empty(t, repair.Puts)
	})

	for _, tc := range []struct {
		name string
		pub  *subjectCapturingPublisher
	}{
		{"flush failure without repair store", &subjectCapturingPublisher{flushErr: errors.New("nats: connection closed")}},
		{"publish failure without repair store", &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{{
					Entity: "Account", ChangeType: model.CDCChangeDelete,
					RecordIDs: []string{sfid(tc.name)}, ReplayID: []byte(tc.name),
				}}},
				&fakeB2BOrgReader{}, &mock.MockCacheInvalidator{}, tc.pub, "",
			)

			assert.NotPanics(t, func() {
				require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
			})
		})
	}
}

// TestCDCConsumer_ProjectRole_Delete_PublishesNoDeleteAccess. Key
// contacts are deliberately excluded from the purge: a contact grants one
// person the key_contact relation on a membership that usually outlives them,
// so the correct revocation is the targeted member_remove this path already
// sends. A delete_access would carry the membership UID and strip every other
// principal from a live object.
func TestCDCConsumer_ProjectRole_Delete_PublishesNoDeleteAccess(t *testing.T) {
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Project_Role__c", ChangeType: model.CDCChangeDelete, RecordIDs: []string{sfid("kc-purge")}, ReplayID: []byte("kcdel")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
	)
	require.NoError(t, consumer.Run(context.Background(), "/data/ProjectRoleChangeEvent", &fakeReplayStore{}))

	assert.False(t, pub.hasAccess(constants.FGASyncDeleteAccessSubject),
		"key contacts are revoked by a targeted member_remove, never by a whole-object purge")
}

// ── Lost purges hold the cursor; restores rebuild grants ─────────────────────

// TestCDCConsumer_Delete_UnrecordedFirstPurgeRetriesInProcess verifies the
// first event does not depend on a previously persisted replay cursor.
func TestCDCConsumer_Delete_UnrecordedFirstPurgeRetriesInProcess(t *testing.T) {
	for _, tc := range []struct{ entity, channel string }{
		{"Account", "/data/AccountChangeEvent"},
		{"Asset", "/data/AssetChangeEvent"},
	} {
		t.Run(tc.entity, func(t *testing.T) {
			pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
			repair := &flakyRepairStore{MockCDCRepairStore: &mock.MockCDCRepairStore{}, failFirst: 3}
			replay := &fakeReplayStore{}

			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{
					{Entity: tc.entity, ChangeType: model.CDCChangeDelete,
						RecordIDs: []string{sfid("purge-lost")}, ReplayID: []byte("lost")},
				}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				pub,
				"",
				svc.WithCDCRepairStore(repair),
			)

			require.NoError(t, consumer.Run(context.Background(), tc.channel, replay))

			assert.Equal(t, 4, repair.attempts,
				"the same first event must run again after its initial marker attempts are exhausted")
			require.Len(t, repair.Puts, 1)
			assert.Equal(t, []byte("lost"), replay.saved,
				"the first event advances only after its failed purge is durably recorded")
		})
	}
}

// TestCDCConsumer_Delete_RecordedPurgeStillAdvancesReplayCursor guards the other
// side of the rule. Holding the cursor is a heavy remedy — it stalls every later
// commit in the run — so it must fire only when the purge is genuinely
// unrecoverable. A failed purge whose marker landed is already durably captured
// and an operator can replay it from the repair bucket, so the stream must not
// stall on it.
func TestCDCConsumer_Delete_RecordedPurgeStillAdvancesReplayCursor(t *testing.T) {
	pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
	repair := &mock.MockCDCRepairStore{}
	replay := &fakeReplayStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete,
				RecordIDs: []string{sfid("purge-recorded")}, ReplayID: []byte("recorded")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCRepairStore(repair),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))

	require.Len(t, repair.Puts, 1, "precondition: the recovery marker must have landed")
	assert.Len(t, replay.savedAll, 1,
		"a failed purge that IS durably recorded needs no redelivery, so it must not stall the stream")
}

func TestCDCConsumer_Delete_RetryCompletesBeforeLaterEvents(t *testing.T) {
	org := &model.B2BOrg{UID: sfid("org-after")}
	pub := &subjectCapturingPublisher{accessErr: errors.New("nats: connection closed")}
	replay := &fakeReplayStore{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete,
				RecordIDs: []string{sfid("purge-lost")}, ReplayID: []byte("lost")},
			// A later event that handles cleanly and would otherwise commit.
			{Entity: "Account", ChangeType: model.CDCChangeUpdate,
				RecordIDs: []string{sfid("org-after")}, ReplayID: []byte("after")},
		}},
		&fakeB2BOrgReader{org: org},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCRepairStore(&flakyRepairStore{
			MockCDCRepairStore: &mock.MockCDCRepairStore{},
			failFirst:          3,
		}),
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))

	assert.Equal(t, [][]byte{[]byte("lost"), []byte("after")}, replay.savedAll,
		"the incomplete event must finish and commit before the next event is consumed")
}

// TestCDCConsumer_Delete_RedeliveredPurgeIsHarmless is what makes holding the
// cursor safe at all. The hold exists to force redelivery, so if replaying a
// purge were not idempotent the remedy would be worse than the fault. This is
// asserted rather than assumed because the whole cursor-hold design rests on it.
func TestCDCConsumer_Delete_RedeliveredPurgeIsHarmless(t *testing.T) {
	id := sfid("org-replayed")
	pub := &subjectCapturingPublisher{}

	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeDelete, RecordIDs: []string{id}, ReplayID: []byte("p1")},
			{Entity: "Account", ChangeType: model.CDCChangeDelete, RecordIDs: []string{id}, ReplayID: []byte("p1")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	assert.Equal(t, []string{id, id}, pub.deleteAccessUIDs(t, "b2b_org"),
		"a redelivered purge re-publishes the same withdrawal for the same UID — repeating it changes nothing")
}

// acceptedUser builds an accepted settings principal — the only kind that
// carries an FGA tuple, and therefore the only kind a restore may rebuild.
func acceptedUser(username, invitedAs string) model.B2BOrgUser {
	return model.B2BOrgUser{
		Email:        username + "@example.com",
		Username:     username,
		InvitedAs:    invitedAs,
		InviteStatus: model.InviteStatusAccepted,
	}
}

// b2bOrgUpdateAccess returns the access data of every b2b_org update_access
// message captured, in order.
func (p *subjectCapturingPublisher) b2bOrgUpdateAccess(t *testing.T) []fgatypes.GenericAccessData {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	var out []fgatypes.GenericAccessData
	for _, raw := range p.accessMessages {
		msg, ok := raw.(fgatypes.GenericFGAMessage)
		if !ok || msg.ObjectType != "b2b_org" || msg.Operation != "update_access" {
			continue
		}
		data, ok := msg.Data.(fgatypes.GenericAccessData)
		require.True(t, ok, "update_access must carry GenericAccessData")
		out = append(out, data)
	}
	return out
}

// restoredOrgConsumer wires a consumer for an org restore with the given
// settings record, which may be nil to represent no record at all.
func restoredOrgConsumer(pub *subjectCapturingPublisher, org *model.B2BOrg,
	settings port.B2BOrgSettingsReader, changeType model.CDCChangeType) *svc.CDCConsumer {
	opts := []svc.CDCConsumerOption{
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	}
	if settings != nil {
		opts = append(opts, svc.WithCDCB2BOrgSettingsReader(settings))
	}
	return newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: changeType,
				RecordIDs: []string{org.UID}, ReplayID: []byte("undel")},
		}},
		&fakeB2BOrgReader{org: org},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		opts...,
	)
}

type failingB2BOrgSettingsReader struct {
	err error
}

func (r *failingB2BOrgSettingsReader) GetSettings(
	_ context.Context,
	_ string,
) (*model.B2BOrgSettings, uint64, error) {
	return nil, 0, r.err
}

func (r *failingB2BOrgSettingsReader) ListSettingsOrgUIDs(context.Context) ([]string, error) {
	return nil, r.err
}

// TestCDCConsumer_Account_Undelete_RebuildsWriterAndAuditorGrants verifies org
// restoration. The ordinary upsert passes nil for both relations, which
// preserves whatever tuples exist — correct for a normal update, but after a
// purge there is nothing left to preserve, so the administrators stay locked
// out. The settings record survives the purge (delete_access withdraws FGA
// tuples, not KV records) and is the authoritative source for rebuilding them.
func TestCDCConsumer_Account_Undelete_RebuildsWriterAndAuditorGrants(t *testing.T) {
	for _, changeType := range []model.CDCChangeType{
		model.CDCChangeUndelete,
		model.CDCChangeGapUndelete,
	} {
		t.Run(string(changeType), func(t *testing.T) {
			id := sfid("org-restored")
			org := &model.B2BOrg{UID: id}
			settings := mock.NewMockB2BOrgSettings()
			settings.Seed(id, &model.B2BOrgSettings{
				UID:      id,
				Writers:  []model.B2BOrgUser{acceptedUser("wuser", "writer")},
				Auditors: []model.B2BOrgUser{acceptedUser("auser", "auditor")},
			}, 1)

			pub := &subjectCapturingPublisher{}
			replay := &fakeReplayStore{}
			consumer := restoredOrgConsumer(pub, org, settings, changeType)
			require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))

			var restored *fgatypes.GenericAccessData
			for i, data := range pub.b2bOrgUpdateAccess(t) {
				if len(data.Relations["writer"]) > 0 {
					restored = &pub.b2bOrgUpdateAccess(t)[i]
					break
				}
			}
			require.NotNil(t, restored, "a restored org must republish its per-user grants")

			assert.Equal(t, []string{"wuser"}, restored.Relations["writer"])
			assert.Equal(t, []string{"auser"}, restored.Relations["auditor"])
			assert.NotContains(t, restored.ExcludeRelations, "writer",
				"writer must be full-synced, not excluded — an excluded relation preserves nothing after a purge")
			assert.NotContains(t, restored.ExcludeRelations, "auditor")
			assert.Equal(t, 1, pub.flushCount, "restored grants must be confirmed delivered before cursor advancement")
			assert.Equal(t, []byte("undel"), replay.saved)
		})
	}
}

func TestCDCConsumer_Account_Undelete_RebuildsCompleteManagedAccess(t *testing.T) {
	orgUID := sfid("org-restored-full")
	parentUID := sfid("org-parent")
	childUID := sfid("org-child")
	siblingUID := sfid("org-sibling")
	org := &model.B2BOrg{UID: orgUID, ParentUID: parentUID}
	settings := mock.NewMockB2BOrgSettings()
	settings.Seed(orgUID, &model.B2BOrgSettings{
		UID:      orgUID,
		Writers:  []model.B2BOrgUser{acceptedUser("wuser", "writer")},
		Auditors: []model.B2BOrgUser{acceptedUser("auser", "auditor")},
	}, 1)
	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Account", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{orgUID}, ReplayID: []byte("full-restore"),
		}}},
		&fakeB2BOrgReader{org: org, childMap: map[string][]string{
			orgUID:    {childUID},
			parentUID: {orgUID, siblingUID},
		}},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
		svc.WithCDCB2BOrgSettingsReader(settings),
		svc.WithCDCGlobalOrgAdminTeamUID("global-admin"),
		svc.WithCDCB2BOrgAuditorTeams([]string{"org-auditors"}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))

	byUID := make(map[string][]fgatypes.GenericAccessData)
	for _, data := range pub.b2bOrgUpdateAccess(t) {
		byUID[data.UID] = append(byUID[data.UID], data)
	}
	assert.Contains(t, byUID[orgUID], fgatypes.GenericAccessData{
		UID: orgUID,
		References: map[string][]string{
			"global_org_admin": {"team:global-admin#member"},
			"auditor":          {"team:org-auditors#member"},
		},
		Relations:        map[string][]string{"writer": {"wuser"}, "auditor": {"auser"}},
		ExcludeRelations: []string{"parent", "child", "membership"},
	})
	assert.Contains(t, byUID[orgUID], fgatypes.GenericAccessData{
		UID: orgUID, References: map[string][]string{"parent": {"b2b_org:" + parentUID}},
		ExcludeRelations: []string{"global_org_admin", "auditor", "writer", "owner", "membership", "child"},
	})
	assert.Contains(t, byUID[orgUID], fgatypes.GenericAccessData{
		UID: orgUID, References: map[string][]string{"child": {"b2b_org:" + childUID}},
		ExcludeRelations: []string{"global_org_admin", "auditor", "writer", "owner", "membership", "parent"},
	})
	assert.Equal(t, 1, pub.flushCount)
	assert.Equal(t, []byte("full-restore"), replay.saved)
}

func TestCDCConsumer_Account_Undelete_RetriesOwnChildPublishFailure(t *testing.T) {
	orgUID := sfid("org-child-retry")
	childUID := sfid("child-retry")
	org := &model.B2BOrg{UID: orgUID}
	pub := &subjectCapturingPublisher{}
	childAttempts := 0
	pub.beforeAccess = func(subject string, msg any) error {
		fgaMsg, ok := msg.(fgatypes.GenericFGAMessage)
		if subject != constants.FGASyncUpdateAccessSubject || !ok || fgaMsg.ObjectType != "b2b_org" {
			return nil
		}
		data, ok := fgaMsg.Data.(fgatypes.GenericAccessData)
		if !ok || len(data.References["child"]) == 0 {
			return nil
		}
		childAttempts++
		if childAttempts == 1 {
			return errors.New("nats: connection closed")
		}
		return nil
	}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Account", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{orgUID}, ReplayID: []byte("child-retry"),
		}}},
		&fakeB2BOrgReader{org: org, childMap: map[string][]string{orgUID: {childUID}}},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))
	assert.Equal(t, 2, childAttempts)
	assert.Equal(t, 2, pub.flushCount)
	assert.Equal(t, []byte("child-retry"), replay.saved)
}

func TestCDCConsumer_Account_Undelete_HierarchyReadFailurePublishesNoPartialHierarchy(t *testing.T) {
	orgUID := sfid("org-hierarchy-read")
	org := &model.B2BOrg{UID: orgUID, ParentUID: sfid("org-parent")}
	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Account", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{orgUID}, ReplayID: []byte("hierarchy-read"),
		}}},
		&fakeB2BOrgReader{
			org:      org,
			childMap: map[string][]string{orgUID: {sfid("partial-child")}},
			batchErr: errors.New("salesforce: hierarchy query failed"),
		},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
	)

	requireAuthorizationRetry(t, consumer, "/data/AccountChangeEvent", replay)
	for _, data := range pub.b2bOrgUpdateAccess(t) {
		assert.Empty(t, data.References["parent"])
		assert.Empty(t, data.References["child"],
			"partial hierarchy data must not full-sync and temporarily revoke valid edges")
	}
}

func TestCDCConsumer_Account_RestoreDeliveryFailureHoldsReplayCursor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*subjectCapturingPublisher)
	}{
		{"publish failure", func(pub *subjectCapturingPublisher) {
			pub.accessErr = errors.New("nats: connection closed")
		}},
		{"flush failure", func(pub *subjectCapturingPublisher) {
			pub.flushErr = errors.New("nats: flush timeout")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := sfid("org-restore-delivery")
			org := &model.B2BOrg{UID: id}
			settings := mock.NewMockB2BOrgSettings()
			settings.Seed(id, &model.B2BOrgSettings{
				UID:     id,
				Writers: []model.B2BOrgUser{acceptedUser("wuser", "writer")},
			}, 1)

			pub := &subjectCapturingPublisher{}
			tc.configure(pub)
			replay := &fakeReplayStore{}
			consumer := restoredOrgConsumer(pub, org, settings, model.CDCChangeUndelete)
			requireAuthorizationRetry(t, consumer, "/data/AccountChangeEvent", replay)
			if tc.name == "flush failure" {
				assert.GreaterOrEqual(t, pub.flushCount, 1)
			} else {
				assert.Zero(t, pub.flushCount, "nothing accepted for restore means there is nothing to flush")
			}
		})
	}
}

func TestCDCConsumer_Account_RestoreSettingsFailureHoldsReplayCursor(t *testing.T) {
	id := sfid("org-settings-fail")
	org := &model.B2BOrg{UID: id}
	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := restoredOrgConsumer(pub, org,
		&failingB2BOrgSettingsReader{err: errors.New("kv: unavailable")},
		model.CDCChangeUndelete,
	)

	requireAuthorizationRetry(t, consumer, "/data/AccountChangeEvent", replay)
}

func TestCDCConsumer_RestoreRecordFetchFailureHoldsReplayCursor(t *testing.T) {
	for _, tc := range []struct {
		entity       string
		readerOption svc.CDCConsumerOption
	}{
		{"Account", svc.WithCDCAccountBatchReader(
			&mock.MockAccountBatchReader{Err: errors.New("salesforce: unavailable")})},
		{"Asset", svc.WithCDCMembershipBatchReader(
			&mock.MockMembershipBatchReader{Err: errors.New("salesforce: unavailable")})},
	} {
		t.Run(tc.entity, func(t *testing.T) {
			id := sfid(tc.entity + "-restore-fetch")
			replay := &fakeReplayStore{}
			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{{
					Entity: tc.entity, ChangeType: model.CDCChangeUndelete,
					RecordIDs: []string{id}, ReplayID: []byte("fetch-fail"),
				}}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				&subjectCapturingPublisher{},
				"",
				tc.readerOption,
			)

			requireAuthorizationRetry(t, consumer, "/data/"+tc.entity+"ChangeEvent", replay)
		})
	}
}

func TestCDCConsumer_RestoreAbsentNonQualifyingRecordAdvancesReplayCursor(t *testing.T) {
	for _, tc := range []struct {
		entity       string
		readerOption svc.CDCConsumerOption
	}{
		{"Account", svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{})},
		{"Asset", svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{})},
	} {
		t.Run(tc.entity, func(t *testing.T) {
			id := sfid(tc.entity + "-restore-absent")
			pub := &subjectCapturingPublisher{}
			replay := &fakeReplayStore{}
			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{{
					Entity: tc.entity, ChangeType: model.CDCChangeUndelete,
					RecordIDs: []string{id}, ReplayID: []byte("absent"),
				}}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				pub,
				"",
				tc.readerOption,
			)

			require.NoError(t, consumer.Run(context.Background(), "/data/"+tc.entity+"ChangeEvent", replay))
			assert.Equal(t, []byte("absent"), replay.saved,
				"a record outside the managed membership projection must not stall the shared CDC channel")
			assert.False(t, pub.hasAccess(constants.FGASyncDeleteAccessSubject),
				"restore absence converges only the index and must never withdraw authorization")
		})
	}
}

func TestCDCConsumer_RestoreDeterministicConversionFailureAdvancesReplayCursor(t *testing.T) {
	for _, tc := range []struct {
		entity       string
		readerOption func(string) svc.CDCConsumerOption
	}{
		{"Account", func(id string) svc.CDCConsumerOption {
			return svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{ConvErrSFIDs: []string{id}})
		}},
		{"Asset", func(id string) svc.CDCConsumerOption {
			return svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{ConvErrSFIDs: []string{id}})
		}},
	} {
		t.Run(tc.entity, func(t *testing.T) {
			id := sfid(tc.entity + "-restore-convert")
			replay := &fakeReplayStore{}
			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{{
					Entity: tc.entity, ChangeType: model.CDCChangeUndelete,
					RecordIDs: []string{id}, ReplayID: []byte("convert"),
				}}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				&subjectCapturingPublisher{},
				"",
				tc.readerOption(id),
			)

			require.NoError(t, consumer.Run(context.Background(), "/data/"+tc.entity+"ChangeEvent", replay))
			assert.Equal(t, []byte("convert"), replay.saved,
				"a deterministic conversion failure is logged by the reader and must not stall every CDC entity")
		})
	}
}

func TestCDCConsumer_Account_FirstRestoreRetriesBeforeLaterEvents(t *testing.T) {
	id := sfid("org-restore-latch")
	org := &model.B2BOrg{UID: id}
	settings := mock.NewMockB2BOrgSettings()
	settings.Seed(id, &model.B2BOrgSettings{
		UID:     id,
		Writers: []model.B2BOrgUser{acceptedUser("wuser", "writer")},
	}, 1)

	pub := &subjectCapturingPublisher{}
	restoreAttempts := 0
	pub.beforeAccess = func(subject string, msg any) error {
		fgaMsg, ok := msg.(fgatypes.GenericFGAMessage)
		if !ok || subject != constants.FGASyncUpdateAccessSubject {
			return nil
		}
		data, ok := fgaMsg.Data.(fgatypes.GenericAccessData)
		if ok && len(data.Relations["writer"]) > 0 {
			restoreAttempts++
			if restoreAttempts == 1 {
				return errors.New("nats: connection closed")
			}
		}
		return nil
	}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUndelete, RecordIDs: []string{id}, ReplayID: []byte("restore")},
			{Entity: "Account", ChangeType: model.CDCChangeUpdate, RecordIDs: []string{id}, ReplayID: []byte("later")},
		}},
		&fakeB2BOrgReader{org: org},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
		svc.WithCDCB2BOrgSettingsReader(settings),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", replay))
	assert.Equal(t, 2, restoreAttempts, "the first restore must retry in process")
	assert.Equal(t, [][]byte{[]byte("restore"), []byte("later")}, replay.savedAll,
		"the failed restore must complete before a later event advances the cursor")
}

// TestCDCConsumer_Account_Undelete_NeverReapsGrants is the safety half of the
// same change, and the reason only non-empty lists are sent. A non-nil empty
// list means "replace with nothing", so an UNDELETE that followed no purge — a
// record deleted before this consumer was subscribed, then restored — would
// revoke grants that are still legitimate. Restoring never needs to reap.
func TestCDCConsumer_Account_Undelete_NeverReapsGrants(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings *model.B2BOrgSettings
	}{
		{"no settings record", nil},
		{"only pending and revoked principals", &model.B2BOrgSettings{
			Writers: []model.B2BOrgUser{
				{Email: "p@example.com", InvitedAs: "writer", InviteStatus: model.InviteStatusPending},
				{Email: "r@example.com", Username: "ruser", InvitedAs: "writer", InviteStatus: model.InviteStatusRevoked},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := sfid("org-noreap")
			org := &model.B2BOrg{UID: id}
			store := mock.NewMockB2BOrgSettings()
			if tc.settings != nil {
				store.Seed(id, tc.settings, 1)
			}

			pub := &subjectCapturingPublisher{}
			consumer := restoredOrgConsumer(pub, org, store, model.CDCChangeUndelete)
			require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

			for _, data := range pub.b2bOrgUpdateAccess(t) {
				assert.Empty(t, data.Relations["writer"],
					"a restore with no active principals must not publish an empty writer set — that would revoke, not restore")
				assert.Contains(t, data.ExcludeRelations, "writer",
					"writer must stay excluded so existing tuples are preserved")
			}
		})
	}
}

type projectResolverError struct {
	port.ProjectResolver
	err error
}

func (r projectResolverError) UIDFromSlug(context.Context, string) (string, error) {
	return "", r.err
}

func restoredMembership(uid string) *model.ProjectMembership {
	return &model.ProjectMembership{
		UID:        uid,
		B2BOrgUID:  sfid("org-" + uid),
		ProjectUID: "project-" + uid,
	}
}

type clearFailingGrantIndex struct {
	*mock.MockKeyContactGrantIndex
	puts int
}

func (i *clearFailingGrantIndex) Put(ctx context.Context, uid string, grant port.KeyContactGrant) error {
	i.puts++
	if i.puts == 2 {
		return errors.New("kv: marker clear failed")
	}
	return i.MockKeyContactGrantIndex.Put(ctx, uid, grant)
}

// TestCDCConsumer_Asset_Undelete_RebuildsKeyContactGrants verifies membership
// restoration. Salesforce is authoritative for current key contacts; the
// grant index can be incomplete for grants created before the index existed.
func TestCDCConsumer_Asset_Undelete_RebuildsKeyContactGrants(t *testing.T) {
	for _, changeType := range []model.CDCChangeType{
		model.CDCChangeUndelete,
		model.CDCChangeGapUndelete,
	} {
		t.Run(string(changeType), func(t *testing.T) {
			membershipUID := sfid("pm-restored")
			pm := restoredMembership(membershipUID)
			contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{
				{UID: sfid("kc-one"), MembershipUID: membershipUID, Username: "alice"},
				{UID: sfid("kc-two"), MembershipUID: membershipUID, Username: "bob"},
				{UID: sfid("kc-three"), MembershipUID: membershipUID, Username: "carol"},
			}}
			grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
				sfid("kc-one"): {MembershipUID: membershipUID, Username: "alice", Revision: 1},
				sfid("kc-two"): {MembershipUID: membershipUID, Username: "bob", Revision: 1},
			}}

			pub := &subjectCapturingPublisher{}
			replay := &fakeReplayStore{}
			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{
					{Entity: "Asset", ChangeType: changeType,
						RecordIDs: []string{membershipUID}, ReplayID: []byte("undel-pm")},
				}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				pub,
				"",
				svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
				svc.WithCDCKeyContactGrantIndex(grants),
				svc.WithCDCKeyContactsByMembershipReader(contacts),
			)

			require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

			var restored []string
			for i, subject := range pub.access {
				if subject != fgaconstants.GenericMemberPutSubject {
					continue
				}
				msg, ok := pub.accessMessages[i].(fgatypes.GenericFGAMessage)
				require.True(t, ok, "member_put payload must be a GenericFGAMessage")
				data, ok := msg.Data.(fgatypes.GenericMemberData)
				require.True(t, ok, "member_put must carry GenericMemberData")
				restored = append(restored, data.Username)
			}

			assert.ElementsMatch(t, []string{"alice", "bob", "carol"}, restored,
				"every current Salesforce key contact must be restored even when the grant index is incomplete")
			assert.Equal(t, 1, pub.flushCount, "all restored contacts must be confirmed by one trailing flush")
			assert.Equal(t, []byte("undel-pm"), replay.saved)
		})
	}
}

func TestCDCConsumer_Asset_Undelete_ConfirmsStructuralRestoreWithoutContacts(t *testing.T) {
	membershipUID := sfid("pm-structural")
	pm := &model.ProjectMembership{
		UID:        membershipUID,
		B2BOrgUID:  sfid("org-structural"),
		ProjectUID: "project-structural",
	}
	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("structural"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{
			Memberships: []*model.ProjectMembership{pm},
		}),
		svc.WithCDCKeyContactsByMembershipReader(&mock.MockKeyContactsByMembershipReader{}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	var restored *fgatypes.GenericAccessData
	for _, message := range pub.fgaMessages(t) {
		if message.ObjectType != "project_membership" {
			continue
		}
		data, ok := message.Data.(fgatypes.GenericAccessData)
		require.True(t, ok)
		restored = &data
	}
	require.NotNil(t, restored)
	assert.Equal(t, []string{"b2b_org:" + pm.B2BOrgUID}, restored.References["b2b_org"])
	assert.Equal(t, []string{"project:" + pm.ProjectUID}, restored.References["project"])
	assert.Equal(t, 1, pub.flushCount,
		"the structural restore must be confirmed even when there are no key contacts")
	assert.Equal(t, []byte("structural"), replay.saved)
}

func TestCDCConsumer_Asset_Undelete_RetriesStructuralPublishFailure(t *testing.T) {
	membershipUID := sfid("pm-structural-retry")
	pm := restoredMembership(membershipUID)
	pub := &subjectCapturingPublisher{}
	attempts := 0
	pub.beforeAccess = func(subject string, msg any) error {
		fgaMsg, ok := msg.(fgatypes.GenericFGAMessage)
		if subject != constants.FGASyncUpdateAccessSubject || !ok ||
			fgaMsg.ObjectType != "project_membership" {
			return nil
		}
		attempts++
		if attempts == 1 {
			return errors.New("nats: connection closed")
		}
		return nil
	}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("structural-retry"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{
			Memberships: []*model.ProjectMembership{pm},
		}),
		svc.WithCDCKeyContactsByMembershipReader(&mock.MockKeyContactsByMembershipReader{}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))
	assert.Equal(t, 2, attempts)
	assert.Equal(t, 1, pub.flushCount)
	assert.Equal(t, []byte("structural-retry"), replay.saved)
}

func TestCDCConsumer_Asset_Undelete_MissingSourceReferencesAdvances(t *testing.T) {
	membershipUID := sfid("pm-missing-refs")
	pm := &model.ProjectMembership{UID: membershipUID}
	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("missing-refs"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{
			Memberships: []*model.ProjectMembership{pm},
		}),
		svc.WithCDCKeyContactsByMembershipReader(&mock.MockKeyContactsByMembershipReader{}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))
	assert.Equal(t, []byte("missing-refs"), replay.saved,
		"missing source associations are deterministic and must not stall the channel")
	assert.Equal(t, 1, pub.flushCount)
}

func TestCDCConsumer_Asset_Undelete_ProjectResolverFailureRetries(t *testing.T) {
	membershipUID := sfid("pm-resolver-fail")
	pm := &model.ProjectMembership{
		UID:         membershipUID,
		B2BOrgUID:   sfid("org-resolver-fail"),
		ProjectSlug: "unknown-project",
	}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("resolver-fail"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		&subjectCapturingPublisher{},
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{
			Memberships: []*model.ProjectMembership{pm},
		}),
		svc.WithCDCProjectResolver(projectResolverError{err: errors.New("project service unavailable")}),
		svc.WithCDCKeyContactsByMembershipReader(&mock.MockKeyContactsByMembershipReader{}),
	)

	requireAuthorizationRetry(t, consumer, "/data/AssetChangeEvent", replay)
}

func TestCDCConsumer_Asset_Undelete_ProjectNotFoundAdvances(t *testing.T) {
	membershipUID := sfid("pm-project-miss")
	pm := &model.ProjectMembership{
		UID:         membershipUID,
		B2BOrgUID:   sfid("org-project-miss"),
		ProjectSlug: "unknown-project",
	}
	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("project-miss"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{
			Memberships: []*model.ProjectMembership{pm},
		}),
		svc.WithCDCProjectResolver(projectResolverError{
			err: pkgerrors.NewNotFound("project not found"),
		}),
		svc.WithCDCKeyContactsByMembershipReader(&mock.MockKeyContactsByMembershipReader{}),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))
	assert.Equal(t, []byte("project-miss"), replay.saved,
		"a definitive project miss cannot recover through retry")
	assert.Equal(t, 1, pub.flushCount)
}

func TestCDCConsumer_Asset_Undelete_GrantIndexFailureHoldsReplayCursor(t *testing.T) {
	membershipUID := sfid("pm-index-fail")
	pm := restoredMembership(membershipUID)
	contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{
		{UID: sfid("kc-index-fail"), MembershipUID: membershipUID, Username: "alice"},
	}}
	grants := &mock.MockKeyContactGrantIndex{GetErr: errors.New("kv: unavailable")}

	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("index-fail"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCKeyContactGrantIndex(grants),
		svc.WithCDCKeyContactsByMembershipReader(contacts),
	)

	requireAuthorizationRetry(t, consumer, "/data/AssetChangeEvent", replay)
}

func TestCDCConsumer_Asset_Undelete_ConfirmedRevokeMarkerClearFailureAdvancesCursor(t *testing.T) {
	membershipUID := sfid("pm-clear-fail")
	contactUID := sfid("kc-clear-fail")
	pm := restoredMembership(membershipUID)
	contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{{
		UID: contactUID, MembershipUID: membershipUID, Username: "alice",
	}}}
	grants := &clearFailingGrantIndex{MockKeyContactGrantIndex: &mock.MockKeyContactGrantIndex{
		Entries: map[string]port.KeyContactGrant{
			contactUID: {MembershipUID: sfid("pm-old"), Username: "alice", Revision: 1},
		},
	}}

	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("clear-fail"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCKeyContactGrantIndex(grants),
		svc.WithCDCKeyContactsByMembershipReader(contacts),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))
	assert.Equal(t, []byte("clear-fail"), replay.saved,
		"a stale marker after its revoke was flushed is bookkeeping debt, not an incomplete authorization change")
	assert.NotNil(t, grants.Entries[contactUID].PendingRevoke)
}

func TestCDCConsumer_Asset_Undelete_FlushFailureHoldsReplayCursor(t *testing.T) {
	membershipUID := sfid("pm-flush-fail")
	pm := restoredMembership(membershipUID)
	contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{
		{UID: sfid("kc-flush-fail"), MembershipUID: membershipUID, Username: "alice"},
	}}

	pub := &subjectCapturingPublisher{flushErr: errors.New("nats: flush timeout")}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{
			Entity: "Asset", ChangeType: model.CDCChangeUndelete,
			RecordIDs: []string{membershipUID}, ReplayID: []byte("flush-fail"),
		}}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCKeyContactGrantIndex(&mock.MockKeyContactGrantIndex{}),
		svc.WithCDCKeyContactsByMembershipReader(contacts),
	)

	requireAuthorizationRetry(t, consumer, "/data/AssetChangeEvent", replay)
	assert.GreaterOrEqual(t, pub.flushCount, 1)
}

// TestCDCConsumer_Asset_Undelete_PartialContactFailureRestoresRest keeps one
// unreachable contact from locking out the others: a contact that cannot be
// restored is a smaller failure than a membership that restores nobody.
func TestCDCConsumer_Asset_Undelete_PartialContactFailureRestoresRest(t *testing.T) {
	membershipUID := sfid("pm-partial")
	pm := restoredMembership(membershipUID)
	contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{
		{UID: sfid("kc-a"), MembershipUID: membershipUID, Username: "alice"},
		{UID: sfid("kc-b"), MembershipUID: membershipUID, Username: "bob"},
	}}
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		sfid("kc-a"): {MembershipUID: membershipUID, Username: "alice", Revision: 1},
		sfid("kc-b"): {MembershipUID: membershipUID, Username: "bob", Revision: 1},
	}}

	// Fail only the first member_put, leaving the second to succeed.
	var puts int
	pub := &subjectCapturingPublisher{}
	pub.beforeAccess = func(subject string, _ any) error {
		if subject != fgaconstants.GenericMemberPutSubject {
			return nil
		}
		puts++
		if puts == 1 {
			return errors.New("nats: connection closed")
		}
		return nil
	}

	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUndelete,
				RecordIDs: []string{membershipUID}, ReplayID: []byte("undel-partial")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCKeyContactGrantIndex(grants),
		svc.WithCDCKeyContactsByMembershipReader(contacts),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))

	assert.Equal(t, 4, puts,
		"the first attempt must continue, then the complete event must retry")
	assert.Equal(t, 2, pub.flushCount)
	assert.Equal(t, []byte("undel-partial"), replay.saved)
}

// TestCDCConsumer_Asset_Undelete_SalesforceReadFailureRestoresNothing pins the
// no-fallback policy: stale index entries must not be restored when the
// authoritative current-contact query fails.
func TestCDCConsumer_Asset_Undelete_SalesforceReadFailureRestoresNothing(t *testing.T) {
	membershipUID := sfid("pm-listerr")
	pm := restoredMembership(membershipUID)
	grants := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
		sfid("kc-stale"): {MembershipUID: membershipUID, Username: "stale-user", Revision: 1},
	}}
	contacts := &mock.MockKeyContactsByMembershipReader{Err: errors.New("salesforce: unavailable")}

	pub := &subjectCapturingPublisher{}
	replay := &fakeReplayStore{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Asset", ChangeType: model.CDCChangeUndelete,
				RecordIDs: []string{membershipUID}, ReplayID: []byte("undel-listerr")},
		}},
		&fakeB2BOrgReader{},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
		svc.WithCDCKeyContactGrantIndex(grants),
		svc.WithCDCKeyContactsByMembershipReader(contacts),
	)

	requireAuthorizationRetry(t, consumer, "/data/AssetChangeEvent", replay)

	assert.False(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject),
		"an unreadable authoritative source restores nothing rather than falling back to stale index entries")
	assert.False(t, pub.hasAccess(constants.FGASyncDeleteAccessSubject),
		"and it must never withdraw anything on a restore")
}

func TestCDCConsumer_Asset_Undelete_IdentityLookupClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lookupErr  error
		wantCursor bool
	}{
		{"transient failure holds cursor", errors.New("identity service unavailable"), false},
		{"not found advances cursor", pkgerrors.NewNotFound("no LFID for email"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			membershipUID := sfid("pm-identity")
			pm := restoredMembership(membershipUID)
			contacts := &mock.MockKeyContactsByMembershipReader{Contacts: []*model.KeyContact{{
				UID: sfid("kc-identity"), MembershipUID: membershipUID, Email: "person@example.com",
			}}}

			pub := &subjectCapturingPublisher{}
			replay := &fakeReplayStore{}
			consumer := newTestCDCConsumer(
				&fakeCDCSubscriber{events: []model.CDCEvent{{
					Entity: "Asset", ChangeType: model.CDCChangeUndelete,
					RecordIDs: []string{membershipUID}, ReplayID: []byte("identity"),
				}}},
				&fakeB2BOrgReader{},
				&mock.MockCacheInvalidator{},
				pub,
				"",
				svc.WithCDCMembershipBatchReader(&mock.MockMembershipBatchReader{Memberships: []*model.ProjectMembership{pm}}),
				svc.WithCDCKeyContactGrantIndex(&mock.MockKeyContactGrantIndex{}),
				svc.WithCDCKeyContactsByMembershipReader(contacts),
				svc.WithCDCUserReader(&fakeUserReader{err: tc.lookupErr}),
			)

			if tc.wantCursor {
				require.NoError(t, consumer.Run(context.Background(), "/data/AssetChangeEvent", replay))
				assert.Equal(t, []byte("identity"), replay.saved)
			} else {
				requireAuthorizationRetry(t, consumer, "/data/AssetChangeEvent", replay)
			}
			assert.False(t, pub.hasAccess(fgaconstants.GenericMemberPutSubject))
		})
	}
}

// TestCDCConsumer_Undelete_RedeliveredRestoreIsHarmless matches the purge
// equivalent above. Holding the cursor makes replay more likely, not less, so
// restoration has to be safe to repeat.
func TestCDCConsumer_Undelete_RedeliveredRestoreIsHarmless(t *testing.T) {
	id := sfid("org-restore2x")
	org := &model.B2BOrg{UID: id}
	settings := mock.NewMockB2BOrgSettings()
	settings.Seed(id, &model.B2BOrgSettings{
		UID:     id,
		Writers: []model.B2BOrgUser{acceptedUser("wuser", "writer")},
	}, 1)

	pub := &subjectCapturingPublisher{}
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{
			{Entity: "Account", ChangeType: model.CDCChangeUndelete, RecordIDs: []string{id}, ReplayID: []byte("u1")},
			{Entity: "Account", ChangeType: model.CDCChangeUndelete, RecordIDs: []string{id}, ReplayID: []byte("u1")},
		}},
		&fakeB2BOrgReader{org: org},
		&mock.MockCacheInvalidator{},
		pub,
		"",
		svc.WithCDCAccountBatchReader(&mock.MockAccountBatchReader{Orgs: []*model.B2BOrg{org}}),
		svc.WithCDCB2BOrgSettingsReader(settings),
	)

	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))

	var withWriters int
	for _, data := range pub.b2bOrgUpdateAccess(t) {
		if len(data.Relations["writer"]) > 0 {
			withWriters++
			assert.Equal(t, []string{"wuser"}, data.Relations["writer"],
				"each replay publishes the identical grant set — a full-sync of the same members is idempotent")
		}
	}
	assert.Equal(t, 2, withWriters, "both deliveries restore, and both restore the same thing")
}
