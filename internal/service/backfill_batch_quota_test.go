// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
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

// ── Part A: batched targeted reindex (PM + KC) ────────────────────────────────

// countingMembershipBatchReader records how many times the batch fetch is called
// so a test can assert the targeted path issues exactly one SOQL batch.
type countingMembershipBatchReader struct {
	memberships []*model.ProjectMembership
	convErr     []string
	calls       int
}

func (r *countingMembershipBatchReader) FetchMembershipsBySFIDs(_ context.Context, _ []string) ([]*model.ProjectMembership, []string, error) {
	r.calls++
	return r.memberships, r.convErr, nil
}

type countingKeyContactBatchReader struct {
	contacts []*model.KeyContact
	convErr  []string
	calls    int
}

func (r *countingKeyContactBatchReader) FetchKeyContactsBySFIDs(_ context.Context, _ []string) ([]*model.KeyContact, []string, error) {
	r.calls++
	return r.contacts, r.convErr, nil
}

func TestBackfillRunner_TargetedMembership_BatchPath_PublishesAllInOneFetch(t *testing.T) {
	pm1 := &model.ProjectMembership{UID: "001000000000001AAA", ProjectSlug: "proj", B2BOrgUID: "org-1"}
	pm2 := &model.ProjectMembership{UID: "001000000000002AAA", ProjectSlug: "proj", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "proj"})

	batch := &countingMembershipBatchReader{memberships: []*model.ProjectMembership{pm1, pm2}}
	pub := &subjectCapturingPublisher{}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver,
		svc.WithMembershipBatchReader(batch))

	// Request three items — two returned, one absent (not-found).
	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "r",
		Type:  "project_membership",
		Items: []string{pm1.UID, pm2.UID, "001000000000003AAA"},
	}))

	assert.Equal(t, 1, batch.calls, "targeted PM reindex must issue exactly one batch fetch")
	assert.Len(t, pub.indexerMessages, 2, "both returned records must be published; absent one skipped")
}

func TestBackfillRunner_TargetedMembership_BatchPath_ConversionErrorNotPublished(t *testing.T) {
	pm1 := &model.ProjectMembership{UID: "001000000000001AAA", ProjectSlug: "proj", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "proj"})

	// Requested two items: one convertible (published), one conversion-error
	// (present in SOQL but unconvertible → neither published nor absent-deleted).
	batch := &countingMembershipBatchReader{
		memberships: []*model.ProjectMembership{pm1},
		convErr:     []string{"001000000000002AAA"},
	}
	pub := &subjectCapturingPublisher{}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver,
		svc.WithMembershipBatchReader(batch))

	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "r",
		Type:  "project_membership",
		Items: []string{pm1.UID, "001000000000002AAA"},
	}))

	assert.Len(t, pub.indexerMessages, 1, "conversion-error SFID must not be published")
}

func TestBackfillRunner_TargetedKeyContact_BatchPath_Publishes(t *testing.T) {
	kc := &model.KeyContact{UID: "a0J000000000001AAA", ProjectSlug: "proj", MembershipUID: "pm-1", B2BOrgUID: "org-1"}
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "proj"})

	batch := &countingKeyContactBatchReader{contacts: []*model.KeyContact{kc}}
	pub := &subjectCapturingPublisher{}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", resolver,
		svc.WithKeyContactBatchReader(batch))

	require.NoError(t, runner.Run(context.Background(), svc.BackfillRequest{
		RunID: "r",
		Type:  "key_contact",
		Items: []string{kc.UID},
	}))

	assert.Equal(t, 1, batch.calls, "targeted KC reindex must issue exactly one batch fetch")
	assert.Len(t, pub.indexerMessages, 1, "returned key_contact must be published")
}

// ── Part B: backfill quota guard ─────────────────────────────────────────────

func TestBackfillRunner_GateBackfillStart_FullOverThreshold_ReturnsServiceUnavailable(t *testing.T) {
	gauge := &mock.MockSalesforceQuotaGauge{Current: 85, Limit: 100} // 0.85 >= 0.80
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithQuotaGauge(gauge))

	err := runner.GateBackfillStart(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"})
	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "full run over threshold must return ServiceUnavailable, got: %v", err)
}

func TestBackfillRunner_GateBackfillStart_TargetedExempt(t *testing.T) {
	gauge := &mock.MockSalesforceQuotaGauge{Current: 99, Limit: 100} // well over threshold
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithQuotaGauge(gauge))

	err := runner.GateBackfillStart(context.Background(), svc.BackfillRequest{
		RunID: "r", Type: "b2b_org", Items: []string{"001000000000001AAA"},
	})
	assert.NoError(t, err, "targeted (items) mode must be exempt from the quota gate")
}

func TestBackfillRunner_GateBackfillStart_FilteredOverThreshold_ReturnsServiceUnavailable(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gauge := &mock.MockSalesforceQuotaGauge{Current: 85, Limit: 100}
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil,
		svc.WithQuotaGauge(gauge))

	err := runner.GateBackfillStart(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", Since: &since})
	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr)
}

func TestBackfillRunner_GateBackfillStart_NilGauge_FailsOpen(t *testing.T) {
	runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, mock.NewMockMemberPublisher(), nil, "", nil)

	err := runner.GateBackfillStart(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"})
	assert.NoError(t, err, "an unwired gauge must fail open (preserve ungated behavior)")
}

func TestBackfillRunner_RunType_StartGate_StopsFullRunBeforePublishing(t *testing.T) {
	org := &model.B2BOrg{UID: "org-1"}
	gauge := &mock.MockSalesforceQuotaGauge{Current: 90, Limit: 100} // over threshold at start
	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{B2BOrgs: [][]*model.B2BOrg{{org}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithQuotaGauge(gauge))

	// The direct Run path (avatar Job) must be gated at runType start and return
	// a non-nil error so the Job exits non-zero.
	err := runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"})
	require.Error(t, err, "a gated start must surface an error for the Job exit code")
	assert.Empty(t, pub.indexerMessages, "nothing must be published when the start gate trips")
}

func TestBackfillRunner_MidRun_PassiveQuotaStop(t *testing.T) {
	org := &model.B2BOrg{UID: "org-1"}
	// Start gate passes (active Refresh reports below threshold) but the passive
	// Snapshot is over threshold, so the run stops at the first page callback.
	gauge := &mock.MockSalesforceQuotaGauge{Current: 90, Limit: 100} // Snapshot() → 0.90 (over)
	gauge.RefreshFn = func(_ context.Context) (port.QuotaSnapshot, error) {
		return port.QuotaSnapshot{Current: 10, Limit: 100, Generation: 1}, nil // 0.10 (below)
	}

	pub := &subjectCapturingPublisher{}
	iter := &mock.MockBackfillIterator{B2BOrgs: [][]*model.B2BOrg{{org}}}
	runner := svc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, nil, pub, nil, "", nil,
		svc.WithQuotaGauge(gauge))

	err := runner.Run(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org"})
	require.Error(t, err)
	assert.Empty(t, pub.indexerMessages, "mid-run passive stop must prevent publishing")
}

// ── Part D: windowed reindex (until) validation ───────────────────────────────

func TestValidateAndBuildRequest_UntilWithoutSince_ReturnsValidationError(t *testing.T) {
	_, err := svc.ValidateAndBuildRequest(&membershipservice.AdminReindexPayload{
		Type:  "b2b_org",
		Until: strPtr("2026-06-01T00:00:00Z"),
	})
	require.Error(t, err)
	var valErr pkgerrors.Validation
	assert.ErrorAs(t, err, &valErr)
}

func TestValidateAndBuildRequest_SinceAfterUntil_ReturnsValidationError(t *testing.T) {
	_, err := svc.ValidateAndBuildRequest(&membershipservice.AdminReindexPayload{
		Type:  "b2b_org",
		Since: strPtr("2026-06-01T00:00:00Z"),
		Until: strPtr("2026-05-01T00:00:00Z"),
	})
	require.Error(t, err)
	var valErr pkgerrors.Validation
	assert.ErrorAs(t, err, &valErr)
}

func TestValidateAndBuildRequest_UntilWithItems_ReturnsValidationError(t *testing.T) {
	_, err := svc.ValidateAndBuildRequest(&membershipservice.AdminReindexPayload{
		Type:  "b2b_org",
		Since: strPtr("2026-05-01T00:00:00Z"),
		Until: strPtr("2026-06-01T00:00:00Z"),
		Items: []*membershipservice.AdminReindexItem{{UID: "001000000000001AAA"}},
	})
	require.Error(t, err)
	var valErr pkgerrors.Validation
	assert.ErrorAs(t, err, &valErr)
}

func TestValidateAndBuildRequest_ValidWindow_NormalisesUTC(t *testing.T) {
	req, err := svc.ValidateAndBuildRequest(&membershipservice.AdminReindexPayload{
		Type:  "b2b_org",
		Since: strPtr("2026-05-01T00:00:00Z"),
		Until: strPtr("2026-06-01T00:00:00Z"),
	})
	require.NoError(t, err)
	require.NotNil(t, req.Since)
	require.NotNil(t, req.Until)
	assert.Equal(t, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), *req.Until)
}
