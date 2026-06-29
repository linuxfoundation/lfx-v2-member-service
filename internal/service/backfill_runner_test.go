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
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org", "project_membership", "key_contact"}}
	runner.Run(context.Background(), req)

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
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org"}, DryRun: true}
	runner.Run(context.Background(), req)

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
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org", "project_membership"}}
	runner.Run(context.Background(), req)

	// b2b_org fails → 0 publishes; project_membership succeeds → 1 publish
	assert.Equal(t, int32(1), publishCount.Load(), "error in one type must not stop other types")
}

func TestBackfillRunner_SinceFilter_PassedThroughToIterator(t *testing.T) {
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var capturedSince *time.Time

	iter := &capturingSinceIterator{capturedSince: &capturedSince}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil)
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org"}, Since: &since}
	runner.Run(context.Background(), req)

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
		Items: []svc.ReindexItem{{Type: "b2b_org", UID: orgUID}},
	}
	runner.Run(context.Background(), req)

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
		Items: []svc.ReindexItem{{Type: "b2b_org", UID: orgUID}},
	}
	runner.Run(context.Background(), req)

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

func (p *countingPublisher) Access(_ context.Context, _ string, _ any, _ bool) error { return nil }

// capturingSinceIterator captures the since parameter passed to IterB2BOrgs.
type capturingSinceIterator struct {
	capturedSince **time.Time
}

func (c *capturingSinceIterator) IterB2BOrgs(_ context.Context, since *time.Time, _ func([]*model.B2BOrg) error) error {
	*c.capturedSince = since
	return nil
}

func (c *capturingSinceIterator) IterProjectMemberships(_ context.Context, _ *time.Time, _ func([]*model.ProjectMembership) error) error {
	return nil
}

func (c *capturingSinceIterator) IterKeyContacts(_ context.Context, _ *time.Time, _ func([]*model.KeyContact) error) error {
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
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org"}}
	runner.Run(context.Background(), req)

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
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org"}}
	runner.Run(context.Background(), req)

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
		Items: []svc.ReindexItem{{Type: "b2b_org", UID: orgUID}},
	}
	runner.Run(context.Background(), req)

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
	req := svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org_settings"}}
	runner.Run(context.Background(), req)

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
		Items: []svc.ReindexItem{{Type: "b2b_org_settings", UID: uid}},
	}
	runner.Run(context.Background(), req)

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
		Items: []svc.ReindexItem{{Type: "b2b_org_settings", UID: uid}},
	}
	runner.Run(context.Background(), req)

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
		Items: []svc.ReindexItem{{Type: "b2b_org_settings", UID: uid}},
	}
	runner.Run(context.Background(), req)

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
			runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org"}})
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
	runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "test-run",
		Items: []svc.ReindexItem{{Type: "b2b_org", UID: orgUID}},
	})

	assert.GreaterOrEqual(t, accessCount.Load(), int32(1), "targeted FGA access message must be published when globalOrgAdminTeamUID is set")
}

// countingAccessPublisher counts Access calls (FGA publish) separately from Indexer calls.
type countingAccessPublisher struct {
	accessCount *atomic.Int32
}

func (p *countingAccessPublisher) Indexer(_ context.Context, _ string, _ any, _ bool) error {
	return nil
}

func (p *countingAccessPublisher) Access(_ context.Context, _ string, _ any, _ bool) error {
	p.accessCount.Add(1)
	return nil
}

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

func (p *capturingBackfillPublisher) Access(_ context.Context, _ string, _ any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accessCount++
	return nil
}

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
	runner.Run(context.Background(), svc.BackfillRequest{RunID: "test-run", Types: []string{"b2b_org"}})

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
