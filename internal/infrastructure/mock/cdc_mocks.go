// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// compile-time interface checks.
var (
	_ port.CacheInvalidator      = (*MockCacheInvalidator)(nil)
	_ port.MembershipBatchReader = (*MockMembershipBatchReader)(nil)
	_ port.KeyContactBatchReader = (*MockKeyContactBatchReader)(nil)
	_ port.AccountBatchReader    = (*MockAccountBatchReader)(nil)
	_ port.SalesforceQuotaGauge  = (*MockSalesforceQuotaGauge)(nil)
	_ port.CDCRepairStore        = (*MockCDCRepairStore)(nil)
)

// MockCacheInvalidator is a test double for port.CacheInvalidator that counts
// calls per entity type and optionally returns a configured error.
//
// Use when a test needs to assert that cache invalidation was triggered the
// correct number of times, or to simulate an invalidation failure.
type MockCacheInvalidator struct {
	B2BOrgCalls     int
	MembershipCalls int
	KeyContactCalls int

	// InvalidateErr is returned by all three methods when non-nil.
	InvalidateErr error
}

func (c *MockCacheInvalidator) InvalidateB2BOrg(_ context.Context, _ string) error {
	c.B2BOrgCalls++
	return c.InvalidateErr
}
func (c *MockCacheInvalidator) InvalidateProjectMembership(_ context.Context, _ string) error {
	c.MembershipCalls++
	return c.InvalidateErr
}
func (c *MockCacheInvalidator) InvalidateKeyContact(_ context.Context, _ string) error {
	c.KeyContactCalls++
	return c.InvalidateErr
}

// MockMembershipBatchReader is a test double for port.MembershipBatchReader
// that returns a caller-supplied slice or error from FetchMembershipsBySFIDs.
// Set ConvErrSFIDs to simulate SFIDs that were present in the SOQL result but
// could not be converted (exercises the seenButFailed safety property).
type MockMembershipBatchReader struct {
	Memberships  []*model.ProjectMembership
	ConvErrSFIDs []string
	Err          error
}

func (r *MockMembershipBatchReader) FetchMembershipsBySFIDs(_ context.Context, _ []string) ([]*model.ProjectMembership, []string, error) {
	return r.Memberships, r.ConvErrSFIDs, r.Err
}

// MockKeyContactBatchReader is a test double for port.KeyContactBatchReader
// that returns a caller-supplied slice or error from FetchKeyContactsBySFIDs.
// Set ConvErrSFIDs to simulate SFIDs that were present in the SOQL result but
// could not be converted (exercises the seenButFailed safety property).
type MockKeyContactBatchReader struct {
	Contacts     []*model.KeyContact
	ConvErrSFIDs []string
	Err          error
}

func (r *MockKeyContactBatchReader) FetchKeyContactsBySFIDs(_ context.Context, _ []string) ([]*model.KeyContact, []string, error) {
	return r.Contacts, r.ConvErrSFIDs, r.Err
}

// MockAccountBatchReader is a test double for port.AccountBatchReader that
// returns a caller-supplied slice or error from FetchAccountsBySFIDs.
// Set ConvErrSFIDs to simulate SFIDs that were present in the SOQL result but
// could not be converted (exercises the seenButFailed safety property).
type MockAccountBatchReader struct {
	Orgs         []*model.B2BOrg
	ConvErrSFIDs []string
	Err          error
}

func (r *MockAccountBatchReader) FetchAccountsBySFIDs(_ context.Context, _ []string) ([]*model.B2BOrg, []string, error) {
	return r.Orgs, r.ConvErrSFIDs, r.Err
}

// MockSalesforceQuotaGauge is a test double for port.SalesforceQuotaGauge.
// Set Current and Limit to simulate quota states; the zero value (0, 0) is
// treated as limit ≤ 0 by quotaExceeded, so the guard fails open by default.
//
// Snapshot derives Generation from Gen (defaulting to 1 when a limit is set) so
// Observed() is true by default for a populated gauge. Refresh behavior is
// controlled via RefreshFn (highest precedence) or RefreshErr; RefreshCalls
// counts invocations for throttle assertions.
type MockSalesforceQuotaGauge struct {
	Current int64
	Limit   int64

	Gen        uint64
	ObservedAt time.Time

	RefreshErr   error
	RefreshFn    func(ctx context.Context) (port.QuotaSnapshot, error)
	RefreshCalls int
}

func (g *MockSalesforceQuotaGauge) APIUsage() (current, limit int64) {
	return g.Current, g.Limit
}

func (g *MockSalesforceQuotaGauge) Snapshot() port.QuotaSnapshot {
	gen := g.Gen
	if gen == 0 && g.Limit > 0 {
		gen = 1
	}
	return port.QuotaSnapshot{
		Current:    g.Current,
		Limit:      g.Limit,
		ObservedAt: g.ObservedAt,
		Generation: gen,
	}
}

func (g *MockSalesforceQuotaGauge) Refresh(ctx context.Context) (port.QuotaSnapshot, error) {
	g.RefreshCalls++
	if g.RefreshFn != nil {
		return g.RefreshFn(ctx)
	}
	if g.RefreshErr != nil {
		return g.Snapshot(), g.RefreshErr
	}
	return g.Snapshot(), nil
}

// MockCDCRepairStore is a test double for port.CDCRepairStore. Pending is keyed
// by reindex type. Puts and Deletes record calls for assertions; the *Err
// fields inject failures.
type MockCDCRepairStore struct {
	Pending map[string][]port.RepairMarker

	Puts    []MockRepairPut
	Deletes []MockRepairDelete

	PutErr    error
	ListErr   error
	DeleteErr error
}

// MockRepairPut records a PutPending call.
type MockRepairPut struct {
	Type string
	SFID string
}

// MockRepairDelete records a DeletePending call.
type MockRepairDelete struct {
	Type     string
	SFID     string
	Revision uint64
}

func (s *MockCDCRepairStore) PutPending(_ context.Context, reindexType, sfid string) error {
	s.Puts = append(s.Puts, MockRepairPut{Type: reindexType, SFID: sfid})
	return s.PutErr
}

func (s *MockCDCRepairStore) ListPending(_ context.Context, reindexType string, limit int) ([]port.RepairMarker, error) {
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	all := s.Pending[reindexType]
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (s *MockCDCRepairStore) DeletePending(_ context.Context, reindexType, sfid string, revision uint64) error {
	s.Deletes = append(s.Deletes, MockRepairDelete{Type: reindexType, SFID: sfid, Revision: revision})
	return s.DeleteErr
}
