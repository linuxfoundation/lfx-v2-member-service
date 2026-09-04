// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// compile-time interface checks.
var (
	_ port.CacheInvalidator              = (*MockCacheInvalidator)(nil)
	_ port.MembershipCacheEvictor        = (*MockMembershipCacheEvictor)(nil)
	_ port.MembershipBatchReader         = (*MockMembershipBatchReader)(nil)
	_ port.KeyContactBatchReader         = (*MockKeyContactBatchReader)(nil)
	_ port.KeyContactsByMembershipReader = (*MockKeyContactsByMembershipReader)(nil)
	_ port.AccountBatchReader            = (*MockAccountBatchReader)(nil)
	_ port.SalesforceQuotaGauge          = (*MockSalesforceQuotaGauge)(nil)
	_ port.CDCRepairStore                = (*MockCDCRepairStore)(nil)
	_ port.KeyContactGrantIndex          = (*MockKeyContactGrantIndex)(nil)
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

// MockMembershipCacheEvictor is a test double for port.MembershipCacheEvictor
// that records each DeleteMembership call and can return a configured error.
type MockMembershipCacheEvictor struct {
	DeleteCalls int
	DeletedUIDs []string

	// DeleteErr is returned by DeleteMembership when non-nil.
	DeleteErr error
}

func (e *MockMembershipCacheEvictor) DeleteMembership(_ context.Context, uid string) error {
	e.DeleteCalls++
	e.DeletedUIDs = append(e.DeletedUIDs, uid)
	return e.DeleteErr
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

// MockKeyContactsByMembershipReader is a test double for
// port.KeyContactsByMembershipReader.
type MockKeyContactsByMembershipReader struct {
	Contacts   []*model.KeyContact
	Err        error
	Calls      int
	AssetSFIDs []string
}

func (r *MockKeyContactsByMembershipReader) FetchKeyContactsByAssetSFIDs(
	_ context.Context,
	assetSFIDs []string,
) (map[string][]*model.KeyContact, error) {
	r.Calls++
	r.AssetSFIDs = append(r.AssetSFIDs, assetSFIDs...)
	if r.Err != nil {
		return nil, r.Err
	}
	requested := make(map[string]struct{}, len(assetSFIDs))
	grouped := make(map[string][]*model.KeyContact, len(assetSFIDs))
	for _, assetSFID := range assetSFIDs {
		requested[assetSFID] = struct{}{}
		grouped[assetSFID] = nil
	}
	for _, contact := range r.Contacts {
		if _, ok := requested[contact.MembershipUID]; ok {
			grouped[contact.MembershipUID] = append(grouped[contact.MembershipUID], contact)
		}
	}
	return grouped, nil
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

// MockKeyContactGrantIndex is an in-memory test double for
// port.KeyContactGrantIndex. It enforces the same revision-conditional write
// semantics as the NATS adapter so tests can exercise the conflict-and-retry
// path rather than only the uncontended one.
type MockKeyContactGrantIndex struct {
	// Entries is the stored state, keyed by key contact UID. Seed it directly
	// to set up a test; each value's Revision is maintained by Put.
	Entries map[string]port.KeyContactGrant

	// Puts and Deletes record calls in order for assertions.
	Puts    []MockGrantPut
	Deletes []string

	// GetErr, PutErr and DeleteErr inject failures when non-nil.
	GetErr    error
	PutErr    error
	DeleteErr error

	// GetFn, when set, replaces Get entirely. Use it to simulate a grant that
	// changes between reads (for example, to drive a CAS retry).
	GetFn func(ctx context.Context, uid string) (port.KeyContactGrant, bool, error)
}

// MockGrantPut records a Put call, including the revision it was conditioned on.
type MockGrantPut struct {
	UID           string
	MembershipUID string
	Username      string
	Revision      uint64
}

func (i *MockKeyContactGrantIndex) Get(ctx context.Context, uid string) (port.KeyContactGrant, bool, error) {
	if i.GetFn != nil {
		return i.GetFn(ctx, uid)
	}
	if i.GetErr != nil {
		return port.KeyContactGrant{}, false, i.GetErr
	}
	grant, ok := i.Entries[uid]
	if !ok {
		return port.KeyContactGrant{}, false, nil
	}
	return grant, true, nil
}

func (i *MockKeyContactGrantIndex) Put(_ context.Context, uid string, grant port.KeyContactGrant) error {
	i.Puts = append(i.Puts, MockGrantPut{
		UID:           uid,
		MembershipUID: grant.MembershipUID,
		Username:      grant.Username,
		Revision:      grant.Revision,
	})
	if i.PutErr != nil {
		return i.PutErr
	}
	// Mirror the adapter's validateKeyContactGrant: reject a partial live pair
	// or partial PendingRevoke (one field set, the other not — both degrade
	// to fully empty on the wire's omitempty round-trip), and reject an entry
	// with neither a complete live pair nor a complete marker.
	if (grant.MembershipUID == "") != (grant.Username == "") {
		return errs.NewValidation(fmt.Sprintf("key-contact-grants: membership_uid and username must both be set or both be empty for %s", uid))
	}
	if grant.PendingRevoke != nil && (grant.PendingRevoke.MembershipUID == "") != (grant.PendingRevoke.Username == "") {
		return errs.NewValidation(fmt.Sprintf("key-contact-grants: PendingRevoke membership_uid and username must both be set or both be empty for %s", uid))
	}
	if grant.MembershipUID == "" && (grant.PendingRevoke == nil || grant.PendingRevoke.MembershipUID == "") {
		return errs.NewValidation(fmt.Sprintf("key-contact-grants: membership_uid and username are required for %s unless a complete PendingRevoke marker is set", uid))
	}
	stored, exists := i.Entries[uid]
	// Mirror the adapter: revision 0 means create-only, non-zero means the
	// stored revision must still match.
	conflict := exists != (grant.Revision != 0) || (exists && stored.Revision != grant.Revision)
	if conflict {
		return errs.NewConflict(fmt.Sprintf("key-contact-grants: grant for %s changed since read", uid))
	}
	if i.Entries == nil {
		i.Entries = make(map[string]port.KeyContactGrant)
	}
	grant.Revision = stored.Revision + 1
	i.Entries[uid] = grant
	return nil
}

func (i *MockKeyContactGrantIndex) Delete(_ context.Context, uid string, revision uint64) error {
	i.Deletes = append(i.Deletes, uid)
	if i.DeleteErr != nil {
		return i.DeleteErr
	}
	// Mirror the adapter: revision 0 (the caller read no entry) is a no-op —
	// it must never delete unconditionally. A non-zero revision that no longer
	// matches the stored entry means the grant changed since the caller read
	// it, so it must not be deleted either.
	if revision == 0 {
		return nil
	}
	if stored, exists := i.Entries[uid]; exists && stored.Revision != revision {
		return errs.NewConflict(fmt.Sprintf("key-contact-grants: grant for %s changed since read", uid))
	}
	delete(i.Entries, uid)
	return nil
}
