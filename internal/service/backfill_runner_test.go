// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

func TestBackfillRunner_FullMode_PublishesAllTypes(t *testing.T) {
	org := &model.B2BOrg{UID: "org-1"}
	pm := &model.ProjectMembership{UID: "pm-1"}
	kc := &model.KeyContact{UID: "kc-1"}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	iter := &mock.MockBackfillIterator{
		B2BOrgs:     [][]*model.B2BOrg{{org}},
		Memberships: [][]*model.ProjectMembership{{pm}},
		KeyContacts: [][]*model.KeyContact{{kc}},
	}

	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	// Each request carries a single type (BREAKING: no all-types shortcut), so a
	// full reindex is one run per type.
	for _, tp := range []string{"b2b_org", "project_membership", "key_contact"} {
		require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Type: tp}))
	}

	// 3 records × 1 publish each
	assert.Equal(t, int32(3), publishCount.Load(), "should publish one message per record")
}

func TestBackfillRunner_DryRun_DoesNotPublish(t *testing.T) {
	org := &model.B2BOrg{UID: "org-1"}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	iter := &mock.MockBackfillIterator{
		B2BOrgs: [][]*model.B2BOrg{{org}},
	}

	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	req := svc.BackfillRequest{RunID: "test-run", Type: "b2b_org", DryRun: true}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(0), publishCount.Load(), "dry_run must not publish")
}

func TestBackfillRunner_MidRunError_OtherTypesStillRun(t *testing.T) {
	iterErr := errors.New("salesforce timeout")
	pm := &model.ProjectMembership{UID: "pm-1"}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	iter := &mock.MockBackfillIterator{
		B2BErr:      iterErr,                            // b2b_org fails
		Memberships: [][]*model.ProjectMembership{{pm}}, // project_membership succeeds
	}

	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	// Single type per run: a failing b2b_org run must not affect a separate
	// project_membership run.
	require.Error(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Type: "b2b_org"}))
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Type: "project_membership"}))

	// b2b_org fails → 0 publishes; project_membership succeeds → 1 publish
	assert.Equal(t, int32(1), publishCount.Load(), "error in one type must not affect a separate type run")
}

func TestBackfillRunner_SinceFilter_PassedThroughToIterator(t *testing.T) {
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var capturedSince *time.Time

	iter := &capturingSinceIterator{capturedSince: &capturedSince}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil)
	req := svc.BackfillRequest{RunID: "test-run", Type: "b2b_org", Since: &since}
	require.NoError(t, runner.Run(context.Background(), req))

	require.NotNil(t, capturedSince, "since must be forwarded to the iterator")
	assert.Equal(t, since, *capturedSince)
}

func TestBackfillRunner_TargetedMode_FetchesLiveSObjectAndPublishes(t *testing.T) {
	const orgUID = "00000000-0000-0000-0000-000000000001"
	org := &model.B2BOrg{UID: orgUID}
	b2bReader := &seededB2BOrgReaderForBackfill{org: org}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, b2bReader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	req := svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org",
		Items: []string{orgUID},
	}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(1), publishCount.Load(), "targeted mode should publish the fetched record")
}

func TestBackfillRunner_TargetedMode_NotFoundIsSkipped(t *testing.T) {
	const orgUID = "00000000-0000-0000-0000-000000000002"

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	// MockB2BOrgReader always returns not-found
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	req := svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org",
		Items: []string{orgUID},
	}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(0), publishCount.Load(), "not-found items must not publish")
}

// ── Helpers ────────────────────────────────────────────────────────────────

// countingPublisher counts how many times Indexer is called.
type countingPublisher struct {
	count *atomic.Int32
}

func (p *countingPublisher) Indexer(_ context.Context, _ string, _ any, _ bool) error {
	p.count.Add(1)
	return nil
}

func (p *countingPublisher) Access(_ context.Context, _ string, _ any) error { return nil }

func (p *countingPublisher) Flush(_ context.Context) error { return nil }

// capturingSinceIterator captures the since/until parameters passed to IterB2BOrgs.
type capturingSinceIterator struct {
	capturedSince **time.Time
	capturedUntil **time.Time
}

func (c *capturingSinceIterator) IterB2BOrgs(_ context.Context, since, until *time.Time, _ func([]*model.B2BOrg) error) error {
	*c.capturedSince = since
	if c.capturedUntil != nil {
		*c.capturedUntil = until
	}
	return nil
}

func (c *capturingSinceIterator) IterProjectMemberships(_ context.Context, _, _ *time.Time, _ func([]*model.ProjectMembership) error) error {
	return nil
}

func (c *capturingSinceIterator) IterKeyContacts(_ context.Context, _, _ *time.Time, _ func([]*model.KeyContact) error) error {
	return nil
}

// seededB2BOrgReaderForBackfill returns a fixed org for any UID.
type seededB2BOrgReaderForBackfill struct{ org *model.B2BOrg }

func (r *seededB2BOrgReaderForBackfill) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	return r.org, nil
}

func (r *seededB2BOrgReaderForBackfill) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (r *seededB2BOrgReaderForBackfill) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// seededB2BOrgReaderWithChildren returns orgs with configurable child relationships and tracks fetch calls.
type seededB2BOrgReaderWithChildren struct {
	orgs                []*model.B2BOrg
	children            map[string][]string // parentUID → childUIDs
	fetchCallCount      atomic.Int32
	batchFetchCallCount atomic.Int32
	fetchedUIDs         map[string]bool // tracks which UIDs were fetched
	fetchedUIDsMutex    sync.Mutex
}

func (r *seededB2BOrgReaderWithChildren) GetB2BOrg(ctx context.Context, uid string) (*model.B2BOrg, error) {
	for _, org := range r.orgs {
		if org.UID == uid {
			return org, nil
		}
	}
	return nil, pkgerrors.NewNotFound("b2b org not found")
}

func (r *seededB2BOrgReaderWithChildren) FetchChildUIDsByParentUID(_ context.Context, parentUID string) ([]string, error) {
	r.fetchCallCount.Add(1)
	r.fetchedUIDsMutex.Lock()
	if r.fetchedUIDs != nil {
		r.fetchedUIDs[parentUID] = true
	}
	r.fetchedUIDsMutex.Unlock()
	if r.children != nil {
		if uids, ok := r.children[parentUID]; ok {
			return uids, nil
		}
	}
	return []string{}, nil
}

func (r *seededB2BOrgReaderWithChildren) FetchChildUIDsByParentUIDs(_ context.Context, parentUIDs []string) (map[string][]string, error) {
	r.batchFetchCallCount.Add(1)
	result := make(map[string][]string)
	if r.children != nil {
		for _, uid := range parentUIDs {
			if uids, ok := r.children[uid]; ok {
				result[uid] = uids
			}
		}
	}
	return result, nil
}

func (r *seededB2BOrgReaderWithChildren) getFetchCallCount() int32 {
	return r.fetchCallCount.Load()
}

// ── Children field in backfill tests ───────────────────────────────────────

func TestBackfillRunner_B2BOrgs_PopulatesChildrenFromCache(t *testing.T) {
	// Parent org with two children
	parentOrg := &model.B2BOrg{UID: "parent-uid"}
	child1Org := &model.B2BOrg{UID: "child-1-uid"}
	child2Org := &model.B2BOrg{UID: "child-2-uid"}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	childReader := &seededB2BOrgReaderWithChildren{
		orgs: []*model.B2BOrg{parentOrg, child1Org, child2Org},
		children: map[string][]string{
			"parent-uid": {"child-1-uid", "child-2-uid"},
		},
		fetchedUIDs: map[string]bool{},
	}

	iter := &mock.MockBackfillIterator{
		B2BOrgs: [][]*model.B2BOrg{{parentOrg, child1Org, child2Org}},
	}

	runner := svc.NewRunner(iter, childReader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	req := svc.BackfillRequest{RunID: "test-run", Type: "b2b_org"}
	require.NoError(t, runner.Run(context.Background(), req))

	// All 3 orgs should be published (parent + 2 children)
	assert.Equal(t, int32(3), publishCount.Load(), "should publish all orgs")

	// Batch fetch called once for the whole page; single fetch never called on IterB2BOrgs path.
	assert.Equal(t, int32(1), childReader.batchFetchCallCount.Load(), "batch fetch called once per page")
	assert.Equal(t, int32(0), childReader.getFetchCallCount(), "single per-org fetch not called on IterB2BOrgs path")
}

func TestBackfillRunner_B2BOrgs_MemoizesFetchesPerPage(t *testing.T) {
	// Two parent orgs in same page sharing the same parent — should only fetch once
	parentOrg := &model.B2BOrg{UID: "shared-parent-uid"}
	child1 := &model.B2BOrg{UID: "child-1-uid", ParentUID: "shared-parent-uid"}
	child2 := &model.B2BOrg{UID: "child-2-uid", ParentUID: "shared-parent-uid"}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	childReader := &seededB2BOrgReaderWithChildren{
		orgs: []*model.B2BOrg{parentOrg, child1, child2},
		children: map[string][]string{
			"shared-parent-uid": {"child-1-uid", "child-2-uid"},
			"child-1-uid":       {},
			"child-2-uid":       {},
		},
		fetchedUIDs: map[string]bool{},
	}

	// Put all 3 orgs in the same page to trigger memoization
	iter := &mock.MockBackfillIterator{
		B2BOrgs: [][]*model.B2BOrg{{parentOrg, child1, child2}},
	}

	runner := svc.NewRunner(iter, childReader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	req := svc.BackfillRequest{RunID: "test-run", Type: "b2b_org"}
	require.NoError(t, runner.Run(context.Background(), req))

	// Batch fetch called once for the whole page regardless of org count.
	// Single per-org fetch is never called on the IterB2BOrgs path.
	assert.Equal(t, int32(1), childReader.batchFetchCallCount.Load(),
		"batch fetch called once per page regardless of org count")
	assert.Equal(t, int32(0), childReader.getFetchCallCount(),
		"single per-org fetch not called on IterB2BOrgs path")
}

func TestBackfillRunner_TargetedB2BOrg_PopulatesChildren(t *testing.T) {
	const orgUID = "parent-org-uid"
	org := &model.B2BOrg{UID: orgUID}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	childReader := &seededB2BOrgReaderWithChildren{
		orgs: []*model.B2BOrg{org},
		children: map[string][]string{
			orgUID: {"child-1", "child-2"},
		},
		fetchedUIDs: map[string]bool{},
	}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, childReader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	req := svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org",
		Items: []string{orgUID},
	}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(1), publishCount.Load(), "targeted mode should publish the org")
	assert.True(t, childReader.fetchedUIDs[orgUID], "should fetch children for targeted org")
}

// ── B2BOrgSettings backfill tests ─────────────────────────────────────────────

func TestBackfillRunner_Settings_FullMode_PublishesOnePerUID(t *testing.T) {
	const uid1 = "00000000-0000-0000-0000-000000000001"
	const uid2 = "00000000-0000-0000-0000-000000000002"

	org1 := &model.B2BOrg{UID: uid1}
	org2 := &model.B2BOrg{UID: uid2}
	settings1 := &model.B2BOrgSettings{UID: uid1, Writers: []model.B2BOrgUser{{Username: "alice", Email: "alice@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted}}}
	settings2 := &model.B2BOrgSettings{UID: uid2, Writers: []model.B2BOrgUser{{Username: "bob", Email: "bob@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted}}}

	settingsStore := mock.NewMockB2BOrgSettings()
	settingsStore.Seed(uid1, settings1, 1)
	settingsStore.Seed(uid2, settings2, 1)

	b2bReader := &seededB2BOrgReaderForBackfill{org: org1}
	b2bMultiReader := &multiOrgReader{orgs: map[string]*model.B2BOrg{uid1: org1, uid2: org2}}

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, b2bMultiReader, mock.NewMockProjectMembershipReader(), nil, settingsStore, pub, nil, "", nil)
	req := svc.BackfillRequest{RunID: "test-run", Type: "b2b_org_settings"}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(2), publishCount.Load(), "should publish one indexer message per settings UID")
	_ = b2bReader
}

func TestBackfillRunner_Settings_TargetedMode_PublishesOne(t *testing.T) {
	const uid = "00000000-0000-0000-0000-000000000011"

	org := &model.B2BOrg{UID: uid}
	settings := &model.B2BOrgSettings{UID: uid, Writers: []model.B2BOrgUser{{Username: "alice", Email: "alice@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted}}}

	settingsStore := mock.NewMockB2BOrgSettings()
	settingsStore.Seed(uid, settings, 1)

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, &seededB2BOrgReaderForBackfill{org: org}, mock.NewMockProjectMembershipReader(), nil, settingsStore, pub, nil, "", nil)
	req := svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org_settings",
		Items: []string{uid},
	}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(1), publishCount.Load(), "targeted settings backfill should publish exactly one message")
}

func TestBackfillRunner_Settings_TargetedMode_OrgNotFound_Skips(t *testing.T) {
	const uid = "00000000-0000-0000-0000-000000000022"

	settingsStore := mock.NewMockB2BOrgSettings()
	settingsStore.Seed(uid, &model.B2BOrgSettings{UID: uid}, 1)

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	// MockB2BOrgReader always returns not-found
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, settingsStore, pub, nil, "", nil)
	req := svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org_settings",
		Items: []string{uid},
	}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(0), publishCount.Load(), "org-not-found must not publish")
}

func TestBackfillRunner_Settings_TargetedMode_SettingsAbsent_Skips(t *testing.T) {
	const uid = "00000000-0000-0000-0000-000000000033"

	org := &model.B2BOrg{UID: uid}
	// settingsStore is empty — GetSettings returns (nil, 0, nil)
	settingsStore := mock.NewMockB2BOrgSettings()

	var publishCount atomic.Int32
	pub := &countingPublisher{count: &publishCount}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, &seededB2BOrgReaderForBackfill{org: org}, mock.NewMockProjectMembershipReader(), nil, settingsStore, pub, nil, "", nil)
	req := svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org_settings",
		Items: []string{uid},
	}
	require.NoError(t, runner.Run(context.Background(), req))

	assert.Equal(t, int32(0), publishCount.Load(), "absent settings must not publish")
}

// multiOrgReader returns a different org per UID.
type multiOrgReader struct {
	orgs map[string]*model.B2BOrg
}

func (r *multiOrgReader) GetB2BOrg(_ context.Context, uid string) (*model.B2BOrg, error) {
	if org, ok := r.orgs[uid]; ok {
		return org, nil
	}
	return nil, pkgerrors.NewNotFound("b2b org not found")
}

func (r *multiOrgReader) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (r *multiOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// ── GlobalOrgAdminFGA publish ────────────────────────────────────────────────

func TestBackfillRunner_B2BOrg_GlobalAdminFGA(t *testing.T) {
	tests := []struct {
		name                  string
		globalOrgAdminTeamUID string
		wantAccessCount       int32
		assertFn              func(t *testing.T, got int32)
	}{
		{
			name:                  "published when UID set",
			globalOrgAdminTeamUID: "team-uid-abc",
			assertFn: func(t *testing.T, got int32) {
				assert.GreaterOrEqual(t, got, int32(1), "FGA access message must be published when globalOrgAdminTeamUID is set")
			},
		},
		{
			name:                  "skipped when UID empty",
			globalOrgAdminTeamUID: "",
			assertFn: func(t *testing.T, got int32) {
				assert.Equal(t, int32(0), got, "FGA access message must be skipped when globalOrgAdminTeamUID is empty")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			org := &model.B2BOrg{UID: "org-ga-1"}
			iter := &mock.MockBackfillIterator{B2BOrgs: [][]*model.B2BOrg{{org}}}
			var accessCount atomic.Int32
			pub := &countingAccessPublisher{accessCount: &accessCount}
			runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, tt.globalOrgAdminTeamUID, nil)
			require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Type: "b2b_org"}))
			tt.assertFn(t, accessCount.Load())
		})
	}
}

func TestBackfillRunner_TargetedMode_GlobalAdminFGA_PublishedWhenUIDSet(t *testing.T) {
	const orgUID = "00000000-0000-0000-0000-000000000099"
	org := &model.B2BOrg{UID: orgUID}

	var accessCount atomic.Int32
	pub := &countingAccessPublisher{accessCount: &accessCount}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, &seededB2BOrgReaderForBackfill{org: org}, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "team-uid-xyz", nil)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "test-run",
		Type:  "b2b_org",
		Items: []string{orgUID},
	}))

	assert.GreaterOrEqual(t, accessCount.Load(), int32(1), "targeted FGA access message must be published when globalOrgAdminTeamUID is set")
}

// countingAccessPublisher counts Access calls (FGA publish) separately from Indexer calls.
type countingAccessPublisher struct {
	accessCount *atomic.Int32
}

func (p *countingAccessPublisher) Indexer(_ context.Context, _ string, _ any, _ bool) error {
	return nil
}

func (p *countingAccessPublisher) Access(_ context.Context, _ string, _ any) error {
	p.accessCount.Add(1)
	return nil
}

func (p *countingAccessPublisher) Flush(_ context.Context) error { return nil }

// capturingBackfillPublisher captures both indexer message payloads and access
// call count so tests can assert on is_parent and FGA parent tuple messages.
type capturingBackfillPublisher struct {
	mu              sync.Mutex
	indexerMessages []any
	accessCount     int
}

func (p *capturingBackfillPublisher) Indexer(_ context.Context, _ string, msg any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.indexerMessages = append(p.indexerMessages, msg)
	return nil
}

func (p *capturingBackfillPublisher) Access(_ context.Context, _ string, _ any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accessCount++
	return nil
}

func (p *capturingBackfillPublisher) Flush(_ context.Context) error { return nil }

// indexerIsParentForUID returns true if any captured indexer message has
// data.uid == uid and data.is_parent == true.
func (p *capturingBackfillPublisher) indexerIsParentForUID(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, msg := range p.indexerMessages {
		b, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		var env struct {
			Data struct {
				UID      string `json:"uid"`
				IsParent bool   `json:"is_parent"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			continue
		}
		if env.Data.UID == uid && env.Data.IsParent {
			return true
		}
	}
	return false
}

// TestBackfillRunner_B2BOrgs_IsParentAndFGATuplesFromBatch verifies that after
// the batch child-list fetch, parent orgs have is_parent=true in the indexer
// message and FGA parent tuple Access calls are emitted for child orgs whose
// parent has entries in the batch result.
func TestBackfillRunner_B2BOrgs_IsParentAndFGATuplesFromBatch(t *testing.T) {
	t.Parallel()

	// parentOrg has children; child1 and child2 are those children (ParentUID set).
	parentOrg := &model.B2BOrg{UID: "parent-uid-fga"}
	child1 := &model.B2BOrg{UID: "child-uid-1", ParentUID: "parent-uid-fga"}
	child2 := &model.B2BOrg{UID: "child-uid-2", ParentUID: "parent-uid-fga"}

	childReader := &seededB2BOrgReaderWithChildren{
		orgs: []*model.B2BOrg{parentOrg, child1, child2},
		children: map[string][]string{
			"parent-uid-fga": {"child-uid-1", "child-uid-2"},
			"child-uid-1":    {},
			"child-uid-2":    {},
		},
	}

	pub := &capturingBackfillPublisher{}
	iter := &mock.MockBackfillIterator{
		B2BOrgs: [][]*model.B2BOrg{{parentOrg, child1, child2}},
	}

	runner := svc.NewRunner(iter, childReader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Type: "b2b_org"}))

	// Batch fetch called once — not per org.
	assert.Equal(t, int32(1), childReader.batchFetchCallCount.Load(),
		"FetchChildUIDsByParentUIDs must be called once per page")
	assert.Equal(t, int32(0), childReader.getFetchCallCount(),
		"single per-org FetchChildUIDsByParentUID must not be called on IterB2BOrgs path")

	// parentOrg has children → is_parent=true in its indexer message.
	assert.True(t, pub.indexerIsParentForUID("parent-uid-fga"),
		"parent org must have is_parent=true in indexer message")

	// child1 and child2 have a ParentUID whose children are in the cache →
	// PublishB2BOrgParentFGA emits Access calls for each child with a parent.
	// 3 indexer + at least 2 FGA parent tuple access calls (one per child with ParentUID).
	assert.Equal(t, 3, len(pub.indexerMessages), "all 3 orgs must be published")
	assert.GreaterOrEqual(t, pub.accessCount, 2,
		"FGA parent tuple Access calls must be emitted for child orgs")
}

// ── project_uid resolver (backfill runner) ────────────────────────────────────

// seededPMReaderForResolver returns a fixed ProjectMembership for any UID, or
// err when set (used to simulate NotFound / other fetch failures).
type seededPMReaderForResolver struct {
	pm  *model.ProjectMembership
	err error
}

func (r *seededPMReaderForResolver) AssembleProjectMembership(_ context.Context, _ string) (*model.ProjectMembership, time.Time, error) {
	if r.err != nil {
		return nil, time.Time{}, r.err
	}
	return r.pm, time.Now(), nil
}

// seededKCReaderForResolver returns a fixed KeyContact for any UID, or err
// when set (used to simulate NotFound / other fetch failures).
type seededKCReaderForResolver struct {
	kc  *model.KeyContact
	err error
}

func (r *seededKCReaderForResolver) AssembleKeyContact(_ context.Context, _ string) (*model.KeyContact, time.Time, error) {
	if r.err != nil {
		return nil, time.Time{}, r.err
	}
	return r.kc, time.Now(), nil
}

func TestBackfillRunner_ProjectMembership_FullMode_ResolverSuccess_StampsUID(t *testing.T) {
	// A ProjectMembership with a slug but no UID: the resolver stamps the UID and
	// the indexer message must carry the project_uid tag.
	pm := &model.ProjectMembership{UID: "pm-slug-1", ProjectSlug: "my-project", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid-pm", Slug: "my-project"})

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{Memberships: [][]*model.ProjectMembership{{pm}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "project_membership"}))

	require.NotEmpty(t, pub.indexerMessages, "resolver success must publish the project_membership")
	assert.Contains(t, pub.indexerTags(0), "project_uid:resolved-uid-pm",
		"indexer tags must carry the stamped project_uid; got %v", pub.indexerTags(0))
}

func TestBackfillRunner_ProjectMembership_MixedCaseSlug_ResolverSuccess_StampsUID(t *testing.T) {
	// Salesforce Slug__c may be mixed case (ToIP); v2 project-service stores lowercase (toip).
	pm := &model.ProjectMembership{UID: "pm-toip-1", ProjectSlug: "ToIP", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid-toip", Slug: "toip"})

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{Memberships: [][]*model.ProjectMembership{{pm}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "project_membership"}))

	require.NotEmpty(t, pub.indexerMessages, "mixed-case slug must resolve and publish the project_membership")
	assert.Contains(t, pub.indexerTags(0), "project_uid:resolved-uid-toip",
		"indexer tags must carry the stamped project_uid; got %v", pub.indexerTags(0))
}

func TestBackfillRunner_ProjectMembership_FullMode_ResolverFailure_PublishesFGAOnly(t *testing.T) {
	// A ProjectMembership with a slug that the resolver cannot map: the record
	// must skip indexer publish so an existing project_uid tag is never overwritten.
	pm := &model.ProjectMembership{UID: "pm-slug-2", ProjectSlug: "unknown-slug", B2BOrgUID: "org-2"}
	resolver := mock.NewMockProjectResolver()

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{Memberships: [][]*model.ProjectMembership{{pm}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "project_membership"}))

	assert.Empty(t, pub.indexerMessages, "resolver failure must skip the indexer publish")
	assert.NotEmpty(t, pub.accessMessages, "resolver failure must still publish OpenFGA")
}

func TestBackfillRunner_KeyContact_FullMode_ResolverSuccess_StampsUID(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-slug-1", ProjectSlug: "my-project", MembershipUID: "pm-1", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid-kc", Slug: "my-project"})

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{KeyContacts: [][]*model.KeyContact{{kc}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}))

	require.NotEmpty(t, pub.indexerMessages, "resolver success must publish the key_contact")
	assert.Contains(t, pub.indexerTags(0), "project_uid:resolved-uid-kc",
		"indexer tags must carry the stamped project_uid; got %v", pub.indexerTags(0))
}

func TestBackfillRunner_KeyContact_FullMode_ResolverFailure_PublishesFGAOnly(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-slug-2", ProjectSlug: "unknown-slug", MembershipUID: "pm-2", B2BOrgUID: "org-2", Username: "jdoe"}
	resolver := mock.NewMockProjectResolver()

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{KeyContacts: [][]*model.KeyContact{{kc}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}))

	assert.Empty(t, pub.indexerMessages, "resolver failure must skip the indexer publish")
	assert.NotEmpty(t, pub.accessMessages, "resolver failure must still publish OpenFGA")
}

func TestBackfillRunner_ProjectMembership_TargetedMode_ResolverFailure_PublishesFGAOnly(t *testing.T) {
	pm := &model.ProjectMembership{UID: "pm-tgt-1", ProjectSlug: "unknown-slug", B2BOrgUID: "org-tgt-1"}
	resolver := mock.NewMockProjectResolver()

	pub := &subjectCapturingPublisher{}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), &seededPMReaderForResolver{pm: pm}, nil, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "r",
		Type:  "project_membership",
		Items: []string{pm.UID},
	}))

	assert.Empty(t, pub.indexerMessages, "targeted resolver failure must skip the indexer publish")
	assert.NotEmpty(t, pub.accessMessages, "targeted resolver failure must still publish OpenFGA")
}

// TestBackfillRunner_KeyContact_ResolvesUsernameBeforePublishing covers the
// bug where every key_contact source this runner reads (SOQL page, single-item
// sObject assembly, and the SFID batch reader) sets Email but never Username —
// without an LFID lookup here, PublishKeyContactFGA was a guaranteed no-op for
// every reindexed key contact, silently defeating both the FGA grant and the
// key-contact-grants index it should populate.
func TestBackfillRunner_KeyContact_ResolvesUsernameBeforePublishing(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-lfid-1", ProjectSlug: "my-project", MembershipUID: "pm-1", B2BOrgUID: "org-1", Email: "alice@example.com"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "my-project"})

	pub := &subjectCapturingPublisher{}
	grants := &mock.MockKeyContactGrantIndex{}
	iter := &mock.MockBackfillIterator{KeyContacts: [][]*model.KeyContact{{kc}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver,
		svc.WithUserReader(userReaderFunc(func(_ context.Context, email string) (string, error) {
			assert.Equal(t, "alice@example.com", email)
			return "alice", nil
		})),
		svc.WithKeyContactGrantIndex(grants),
	)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}))

	fgaMsgs := pub.fgaMessages(t)
	require.Len(t, fgaMsgs, 1, "a resolved LFID must produce a member_put")
	assert.Equal(t, "member_put", fgaMsgs[0].Operation)
	assert.Contains(t, grants.Entries, "kc-lfid-1", "the resolved grant must be recorded in the index")
}

// indexerDataUsername extracts data.username from the i-th captured indexer
// message, for asserting the OpenSearch doc reflects a resolved LFID.
func indexerDataUsername(t *testing.T, p *subjectCapturingPublisher, i int) string {
	t.Helper()
	require.Greater(t, len(p.indexerMessages), i)
	b, err := json.Marshal(p.indexerMessages[i])
	require.NoError(t, err)
	var env struct {
		Data struct {
			Username string `json:"username"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(b, &env))
	return env.Data.Username
}

// TestBackfillRunner_KeyContact_FullMode_IndexerReflectsResolvedUsername
// The full/filtered key_contact page published the
// OpenSearch indexer document before resolving the LFID username, so the
// indexed doc kept an empty username while the FGA/grant-index publish right
// after it used the resolved one — inconsistent with the targeted reindex
// paths, which resolve first.
func TestBackfillRunner_KeyContact_FullMode_IndexerReflectsResolvedUsername(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-lfid-2", ProjectSlug: "my-project", MembershipUID: "pm-1", B2BOrgUID: "org-1", Email: "alice@example.com"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "my-project"})

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{KeyContacts: [][]*model.KeyContact{{kc}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver,
		svc.WithUserReader(userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice", nil })),
	)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}))

	require.NotEmpty(t, pub.indexerMessages)
	assert.Equal(t, "alice", indexerDataUsername(t, pub, 0),
		"the indexed doc must carry the resolved username, not the pre-resolution empty value")
}

// TestBackfillRunner_KeyContact_UnregisteredEmail_PublishesNothing covers the
// pending-contact case: an email with no LFID must leave Username empty and
// skip the FGA publish, same as CDC/API — not treat NotFound as an error.
func TestBackfillRunner_KeyContact_UnregisteredEmail_PublishesNothing(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-pending-1", ProjectSlug: "my-project", MembershipUID: "pm-1", B2BOrgUID: "org-1", Email: "unregistered@example.com"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "my-project"})

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{KeyContacts: [][]*model.KeyContact{{kc}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver,
		svc.WithUserReader(userReaderFunc(func(_ context.Context, _ string) (string, error) {
			return "", pkgerrors.NewNotFound("no LFID for email")
		})),
	)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}))

	assert.Empty(t, pub.fgaMessages(t), "an unregistered email must leave the contact pending, not error")
}

func TestBackfillRunner_KeyContact_TargetedMode_ResolverFailure_PublishesFGAOnly(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-tgt-1", ProjectSlug: "unknown-slug", MembershipUID: "pm-1", B2BOrgUID: "org-tgt-1", Username: "jdoe"}
	resolver := mock.NewMockProjectResolver()

	pub := &subjectCapturingPublisher{}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), &seededKCReaderForResolver{kc: kc}, nil, pub, nil, "", resolver)
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "r",
		Type:  "key_contact",
		Items: []string{kc.UID},
	}))

	assert.Empty(t, pub.indexerMessages, "targeted resolver failure must skip the indexer publish")
	assert.NotEmpty(t, pub.accessMessages, "targeted resolver failure must still publish OpenFGA")
}

// ── ValidateAndBuildRequest ──────────────────────────────────────────────────

func TestValidateAndBuildRequest_Since_ZonelessTimestamp_ReturnsValidationError(t *testing.T) {
	payload := &membershipservice.AdminReindexPayload{
		Since: strPtr("2026-05-20T00:00:00"), // no zone offset — must be rejected
	}
	_, err := svc.ValidateAndBuildRequest(payload)
	require.Error(t, err, "zone-less RFC 3339 timestamp must be rejected")
	var valErr pkgerrors.Validation
	assert.ErrorAs(t, err, &valErr, "expected Validation error, got: %v", err)
}

// ── cdc_repair drain (RunRepair) per-item outcome tests ───────────────────────

// configurableB2BOrgReaderForRepair is a port.B2BOrgReader test double with
// independently configurable GetB2BOrg / FetchChildUIDsByParentUID failures,
// used to drive reindexItem's b2b_org retry branches.
type configurableB2BOrgReaderForRepair struct {
	orgs     map[string]*model.B2BOrg
	getErr   error
	childErr error
	children map[string][]string
}

func (r *configurableB2BOrgReaderForRepair) GetB2BOrg(_ context.Context, uid string) (*model.B2BOrg, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if org, ok := r.orgs[uid]; ok {
		return org, nil
	}
	return nil, pkgerrors.NewNotFound("b2b org not found")
}

func (r *configurableB2BOrgReaderForRepair) FetchChildUIDsByParentUID(_ context.Context, parentUID string) ([]string, error) {
	if r.childErr != nil {
		return nil, r.childErr
	}
	return r.children[parentUID], nil
}

func (r *configurableB2BOrgReaderForRepair) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

func TestBackfillRunner_RunRepair_B2BOrg_Issued_DeletesMarker(t *testing.T) {
	org := &model.B2BOrg{UID: "001000000000001AAA"}
	reader := &configurableB2BOrgReaderForRepair{orgs: map[string]*model.B2BOrg{org.UID: org}}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, reader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "b2b_org", SFID: org.UID, Revision: 7}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, markers)

	assert.NotEmpty(t, pub.indexerMessages, "issued outcome must publish the org")
	require.Len(t, repairStore.Deletes, 1, "issued outcome must delete the marker")
	assert.Equal(t, mock.MockRepairDelete{Type: "b2b_org", SFID: org.UID, Revision: 7}, repairStore.Deletes[0])
}

func TestBackfillRunner_RunRepair_B2BOrg_NotFound_DeletesMarker(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "b2b_org", SFID: "001000000000009AAA", Revision: 3}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, markers)

	assert.Empty(t, pub.indexerMessages, "not_found outcome must not publish")
	require.Len(t, repairStore.Deletes, 1, "not_found outcome must still delete the marker")
}

func TestBackfillRunner_RunRepair_B2BOrg_ChildFetchError_RetriesAndKeepsMarker(t *testing.T) {
	org := &model.B2BOrg{UID: "001000000000002AAA"}
	reader := &configurableB2BOrgReaderForRepair{
		orgs:     map[string]*model.B2BOrg{org.UID: org},
		childErr: errors.New("soql timeout"),
	}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, reader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "b2b_org", SFID: org.UID, Revision: 1}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, markers)

	assert.Empty(t, pub.indexerMessages, "a dependency failure must not publish a partial projection")
	assert.Empty(t, repairStore.Deletes, "retry outcome must retain the marker (no delete)")
}

func TestBackfillRunner_RunRepair_ProjectMembership_Issued_DeletesMarker(t *testing.T) {
	pm := &model.ProjectMembership{UID: "001000000000003AAA", ProjectSlug: "my-project", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "my-project"})
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), &seededPMReaderForResolver{pm: pm}, nil, nil, pub, nil, "", resolver,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "project_membership", SFID: pm.UID, Revision: 5}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "project_membership"}, markers)

	assert.NotEmpty(t, pub.indexerMessages, "issued outcome must publish the membership")
	require.Len(t, repairStore.Deletes, 1)
	assert.Equal(t, mock.MockRepairDelete{Type: "project_membership", SFID: pm.UID, Revision: 5}, repairStore.Deletes[0])
}

func TestBackfillRunner_RunRepair_ProjectMembership_NotFound_DeletesMarker(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(),
		&seededPMReaderForResolver{err: pkgerrors.NewNotFound("project membership not found")}, nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "project_membership", SFID: "001000000000004AAA", Revision: 1}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "project_membership"}, markers)

	assert.Empty(t, pub.indexerMessages)
	require.Len(t, repairStore.Deletes, 1, "not_found outcome must still delete the marker")
}

func TestBackfillRunner_RunRepair_ProjectMembership_UnresolvedProjectUID_RetriesAndKeepsMarker(t *testing.T) {
	pm := &model.ProjectMembership{UID: "001000000000005AAA", ProjectSlug: "unknown-slug", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver() // unseeded — resolution fails
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), &seededPMReaderForResolver{pm: pm}, nil, nil, pub, nil, "", resolver,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "project_membership", SFID: pm.UID, Revision: 1}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "project_membership"}, markers)

	assert.Empty(t, pub.indexerMessages, "unresolved project_uid must skip the indexer publish")
	assert.NotEmpty(t, pub.accessMessages, "unresolved project_uid must still publish OpenFGA (preserving refs)")
	assert.Empty(t, repairStore.Deletes, "a partial projection (retry) must retain the marker")
}

func TestBackfillRunner_RunRepair_KeyContact_Issued_DeletesMarker(t *testing.T) {
	kc := &model.KeyContact{UID: "001000000000006AAA", ProjectSlug: "my-project", MembershipUID: "pm-1", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "my-project"})
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), &seededKCReaderForResolver{kc: kc}, nil, pub, nil, "", resolver,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "key_contact", SFID: kc.UID, Revision: 2}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}, markers)

	assert.NotEmpty(t, pub.indexerMessages, "issued outcome must publish the key contact")
	require.Len(t, repairStore.Deletes, 1)
	assert.Equal(t, mock.MockRepairDelete{Type: "key_contact", SFID: kc.UID, Revision: 2}, repairStore.Deletes[0])
}

func TestBackfillRunner_RunRepair_KeyContact_NotFound_DeletesMarker(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(),
		&seededKCReaderForResolver{err: pkgerrors.NewNotFound("key contact not found")}, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "key_contact", SFID: "001000000000007AAA", Revision: 1}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}, markers)

	assert.Empty(t, pub.indexerMessages)
	require.Len(t, repairStore.Deletes, 1, "not_found outcome must still delete the marker")
}

func TestBackfillRunner_RunRepair_KeyContact_UnresolvedProjectUID_RetriesAndKeepsMarker(t *testing.T) {
	kc := &model.KeyContact{UID: "001000000000008AAA", ProjectSlug: "unknown-slug", MembershipUID: "pm-1", B2BOrgUID: "org-1", Username: "jdoe"}
	resolver := mock.NewMockProjectResolver() // unseeded — resolution fails
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), &seededKCReaderForResolver{kc: kc}, nil, pub, nil, "", resolver,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "key_contact", SFID: kc.UID, Revision: 1}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "key_contact"}, markers)

	assert.Empty(t, pub.indexerMessages, "unresolved project_uid must skip the indexer publish")
	assert.NotEmpty(t, pub.accessMessages, "unresolved project_uid must still publish OpenFGA")
	assert.Empty(t, repairStore.Deletes, "a partial projection (retry) must retain the marker")
}

// ── cdc_repair drain (RunRepair) batch/gate tests ─────────────────────────────

func TestBackfillRunner_RunRepair_MixedOutcomes_CountsEachCorrectly(t *testing.T) {
	issuedOrg := &model.B2BOrg{UID: "001000000000011AAA"}
	reader := &configurableB2BOrgReaderForRepair{orgs: map[string]*model.B2BOrg{issuedOrg.UID: issuedOrg}}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, reader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{
		{Type: "b2b_org", SFID: issuedOrg.UID, Revision: 1},        // issued
		{Type: "b2b_org", SFID: "001000000000012AAA", Revision: 2}, // not_found (absent from reader.orgs)
	}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, markers)

	assert.Len(t, pub.indexerMessages, 1, "only the issued item must publish")
	assert.Len(t, repairStore.Deletes, 2, "both issued and not_found markers must be deleted")
}

func TestBackfillRunner_RunRepair_DeleteConflict_RetainsMarker_NoLostRecord(t *testing.T) {
	// Simulates the delete-race loser: the marker's DeletePending call fails
	// (e.g. revision Conflict from a concurrent skip or drain). The record must
	// not be silently dropped — it stays retained for the next drain.
	org := &model.B2BOrg{UID: "001000000000013AAA"}
	reader := &configurableB2BOrgReaderForRepair{orgs: map[string]*model.B2BOrg{org.UID: org}}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{DeleteErr: pkgerrors.NewConflict("revision mismatch")}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, reader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{{Type: "b2b_org", SFID: org.UID, Revision: 1}}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, markers)

	assert.NotEmpty(t, pub.indexerMessages, "the record was still reindexed (idempotent double-publish is safe)")
	// The delete call was attempted (and failed) — verify via the mock's recorded attempt.
	require.Len(t, repairStore.Deletes, 1, "a delete attempt must be recorded even though it failed")
}

func TestBackfillRunner_RunRepair_EmptyQueue_NoOp(t *testing.T) {
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, nil)

	assert.Empty(t, pub.indexerMessages)
	assert.Empty(t, repairStore.Deletes)
}

func TestBackfillRunner_RunRepair_StopsMidPage_WhenPassiveGaugeCrossesThreshold(t *testing.T) {
	org1 := &model.B2BOrg{UID: "001000000000014AAA"}
	org2 := &model.B2BOrg{UID: "001000000000015AAA"}
	reader := &configurableB2BOrgReaderForRepair{orgs: map[string]*model.B2BOrg{org1.UID: org1, org2.UID: org2}}
	pub := &subjectCapturingPublisher{}
	repairStore := &mock.MockCDCRepairStore{}
	// Gauge is already at/above the default 0.80 threshold — the drain was
	// gated in PrepareRepair on a snapshot taken before this call, but a
	// concurrent live request can push usage up before RunRepair starts
	// processing; repairMidPageQuotaExceeded must catch this without an
	// additional /limits call.
	gauge := &mock.MockSalesforceQuotaGauge{Current: 85, Limit: 100}

	runner := svc.NewRunner(&mock.MockBackfillIterator{}, reader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers := []port.RepairMarker{
		{Type: "b2b_org", SFID: org1.UID, Revision: 1},
		{Type: "b2b_org", SFID: org2.UID, Revision: 1},
	}
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"}, markers)

	assert.Empty(t, pub.indexerMessages, "the drain must stop before processing any item once above threshold")
	assert.Empty(t, repairStore.Deletes, "no marker must be touched when the drain stops before the first item")
	assert.Equal(t, 0, gauge.RefreshCalls, "the mid-page check must be passive — no additional /limits call")
}

// ── PrepareRepair gate tests ───────────────────────────────────────────────────

func TestBackfillRunner_PrepareRepair_NoRepairStore_ReturnsServiceUnavailable(t *testing.T) {
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}))

	_, err := runner.PrepareRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true})
	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "expected ServiceUnavailable, got: %v", err)
}

func TestBackfillRunner_PrepareRepair_NoQuotaGauge_ReturnsServiceUnavailable(t *testing.T) {
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithRepairStore(&mock.MockCDCRepairStore{}))

	_, err := runner.PrepareRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true})
	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "expected ServiceUnavailable, got: %v", err)
}

func TestBackfillRunner_PrepareRepair_QuotaUnreadable_ReturnsServiceUnavailable(t *testing.T) {
	// Never observed, and the active refresh also fails — truly unreadable.
	gauge := &mock.MockSalesforceQuotaGauge{Current: 0, Limit: 0, RefreshErr: errors.New("no route to host")}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithRepairStore(&mock.MockCDCRepairStore{}), svc.WithQuotaGauge(gauge))

	_, err := runner.PrepareRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true})
	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "expected ServiceUnavailable, got: %v", err)
}

func TestBackfillRunner_PrepareRepair_QuotaAtOrAboveThreshold_ReturnsServiceUnavailable(t *testing.T) {
	gauge := &mock.MockSalesforceQuotaGauge{Current: 80, Limit: 100} // 0.80 == default threshold
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithRepairStore(&mock.MockCDCRepairStore{}), svc.WithQuotaGauge(gauge))

	_, err := runner.PrepareRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true})
	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "expected ServiceUnavailable, got: %v", err)
}

func TestBackfillRunner_PrepareRepair_QuotaBelowThreshold_ReturnsSelectedMarkers(t *testing.T) {
	repairStore := &mock.MockCDCRepairStore{
		Pending: map[string][]port.RepairMarker{
			"b2b_org": {
				{Type: "b2b_org", SFID: "001000000000021AAA", Revision: 1},
				{Type: "b2b_org", SFID: "001000000000022AAA", Revision: 1},
			},
		},
	}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers, err := runner.PrepareRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true})
	require.NoError(t, err)
	assert.Len(t, markers, 2)
}

func TestBackfillRunner_PrepareRepair_RefreshFails_FallsBackToLastObservedSnapshot(t *testing.T) {
	// The active refresh request fails, but a prior valid observation exists and
	// is below threshold — PrepareRepair must proceed on the fallback reading
	// rather than refuse outright.
	repairStore := &mock.MockCDCRepairStore{
		Pending: map[string][]port.RepairMarker{
			"b2b_org": {{Type: "b2b_org", SFID: "001000000000023AAA", Revision: 1}},
		},
	}
	gauge := &mock.MockSalesforceQuotaGauge{
		Current: 10, Limit: 100, Gen: 1, // Observed() == true
		RefreshErr: errors.New("salesforce: rate limited"),
	}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithRepairStore(repairStore), svc.WithQuotaGauge(gauge))

	markers, err := runner.PrepareRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true})
	require.NoError(t, err)
	assert.Len(t, markers, 1)
}
