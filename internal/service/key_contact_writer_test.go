// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testMembershipUID = "00000000-0000-0000-0000-000000000010"
	testKCUID         = "00000000-0000-0000-0000-000000000020"
)

// ── Helpers ────────────────────────────────────────────────────────────────

// trackingPublisher records (subject, call order) to verify publish sequencing.
type trackingPublisher struct {
	mu  sync.Mutex
	log []string // subject per call
}

func (p *trackingPublisher) Indexer(_ context.Context, subject string, _ any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "indexer:"+subject)
	return nil
}

func (p *trackingPublisher) Access(_ context.Context, subject string, _ any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "access:"+subject)
	return nil
}

func (p *trackingPublisher) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.log))
	copy(out, p.log)
	return out
}

// accessPayloadPublisher records FGA Access payloads for username assertions.
type accessPayloadPublisher struct {
	trackingPublisher
	accessMsgs []any
}

func (p *accessPayloadPublisher) Access(_ context.Context, subject string, msg any, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, "access:"+subject)
	if strings.Contains(subject, fgaconstants.GenericMemberRemoveSubject) ||
		strings.Contains(subject, fgaconstants.GenericMemberPutSubject) {
		p.accessMsgs = append(p.accessMsgs, msg)
	}
	return nil
}

// errorPublisher returns an error from Access (FGA remove) to test error propagation.
type errorFGARemovePublisher struct{ trackingPublisher }

func (p *errorFGARemovePublisher) Access(ctx context.Context, subject string, msg any, sync bool) error {
	_ = p.trackingPublisher.Access(ctx, subject, msg, sync)
	if strings.Contains(subject, fgaconstants.GenericMemberRemoveSubject) {
		return pkgerrors.NewUnexpected("nats unavailable", nil)
	}
	return nil
}

// seededStorage is a port.MemberReader that returns a fixed key contact by UID.
type seededStorage struct {
	mock.MockMembershipRepository
	kcs        map[string]*model.KeyContact
	listOrgErr error // if set, ListKeyContactsForOrg returns this error
}

func newSeededStorage(kcs ...*model.KeyContact) *seededStorage {
	s := &seededStorage{kcs: make(map[string]*model.KeyContact)}
	for _, kc := range kcs {
		s.kcs[kc.UID] = kc
	}
	return s
}

func (s *seededStorage) GetKeyContact(_ context.Context, uid string) (*model.KeyContact, error) {
	if kc, ok := s.kcs[uid]; ok {
		return kc, nil
	}
	return nil, pkgerrors.NewNotFound("key contact not found")
}

// errorGetStorage is a port.MemberReader whose GetKeyContact returns a fixed
// non-NotFound error, used to verify the delete path does not sweep on backend errors.
type errorGetStorage struct {
	seededStorage
	err error
}

func (s *errorGetStorage) GetKeyContact(_ context.Context, _ string) (*model.KeyContact, error) {
	return nil, s.err
}

func (s *seededStorage) ListKeyContactsForMembership(_ context.Context, _ string) ([]*model.KeyContact, error) {
	var out []*model.KeyContact
	for _, kc := range s.kcs {
		out = append(out, kc)
	}
	return out, nil
}

func (s *seededStorage) ListKeyContactsForOrg(_ context.Context, orgSFID string) ([]*model.KeyContact, error) {
	if s.listOrgErr != nil {
		return nil, s.listOrgErr
	}
	var out []*model.KeyContact
	for _, kc := range s.kcs {
		if kc.B2BOrgUID == orgSFID {
			out = append(out, kc)
		}
	}
	return out, nil
}

// seededPMReader returns a fixed PM for any UID.
type seededPMReader struct{ pm *model.ProjectMembership }

func (r *seededPMReader) AssembleProjectMembership(_ context.Context, _ string) (*model.ProjectMembership, time.Time, error) {
	return r.pm, time.Time{}, nil
}

// userReaderFunc implements port.UserReader with a function.
type userReaderFunc func(ctx context.Context, email string) (string, error)

func (f userReaderFunc) UsernameByEmail(ctx context.Context, email string) (string, error) {
	return f(ctx, email)
}

func newKCWriter(storage svc.MemberStorageReader, pmReader svc.PMReader, pub svc.PublisherForKC, userReader svc.UserReaderForKC) svc.KeyContactWriter {
	return svc.NewKeyContactWriter(
		svc.WithKCStorage(storage),
		svc.WithKCWriter(mock.NewMockKeyContactWriterWithOK()),
		svc.WithKCProjectMembershipReader(pmReader),
		svc.WithKCPublisher(pub),
		svc.WithKCUserReader(userReader),
	)
}

// spyOrgSettings records AddPrincipal / RemovePrincipal / ChangePrincipalRole calls.
type spyOrgSettings struct {
	mu          sync.Mutex
	adds        []svc.B2BOrgSettingsAddPrincipal
	removes     []svc.B2BOrgSettingsRemovePrincipal
	roleChanges []svc.B2BOrgSettingsChangeRole
	addErr      error
	changeErr   error
}

func (s *spyOrgSettings) AddPrincipal(_ context.Context, in svc.B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adds = append(s.adds, in)
	return &model.B2BOrgSettings{}, s.addErr
}

func (s *spyOrgSettings) RemovePrincipal(_ context.Context, in svc.B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes = append(s.removes, in)
	return &model.B2BOrgSettings{}, nil
}

func (s *spyOrgSettings) ChangePrincipalRole(_ context.Context, in svc.B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roleChanges = append(s.roleChanges, in)
	return &model.B2BOrgSettings{}, s.changeErr
}

func newKCWriterWithOrgSettings(storage svc.MemberStorageReader, pmReader svc.PMReader, pub svc.PublisherForKC, userReader svc.UserReaderForKC, orgSettings *spyOrgSettings) svc.KeyContactWriter {
	return svc.NewKeyContactWriter(
		svc.WithKCStorage(storage),
		svc.WithKCWriter(mock.NewMockKeyContactWriterWithOK()),
		svc.WithKCProjectMembershipReader(pmReader),
		svc.WithKCPublisher(pub),
		svc.WithKCUserReader(userReader),
		svc.WithKCOrgSettings(orgSettings),
	)
}

// ── Create tests ──────────────────────────────────────────────────────────

func TestKeyContactWriter_Create_NormalPath_PublishesInOrder(t *testing.T) {
	pm := &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: "org-1", ProjectUID: "proj-1"}
	pmReader := &seededPMReader{pm: pm}
	pub := &trackingPublisher{}
	storage := newSeededStorage() // empty — no self-heal

	w := newKCWriter(storage, pmReader, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "alice", nil
	}))

	in := svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID,
		FirstName:     "Alice",
		LastName:      "Smith",
		Email:         "alice@example.com",
		Role:          "Technical Advisory Committee (TAC) Representative",
	}
	kc, err := w.Create(context.Background(), in)

	require.NoError(t, err)
	require.NotNil(t, kc)

	// Ordering invariant: PM FGA update_access → key_contact indexer → key_contact FGA put
	calls := pub.calls()
	require.True(t, len(calls) >= 3, "expected at least 3 publish calls, got %d: %v", len(calls), calls)
	// PM FGA is first
	assert.Contains(t, calls[0], "update_access", "first call must be PM FGA update_access")
	// indexer before FGA put
	indexerIdx := -1
	putIdx := -1
	for i, c := range calls {
		if strings.Contains(c, "indexer:") {
			indexerIdx = i
		}
		if strings.Contains(c, fgaconstants.GenericMemberPutSubject) {
			putIdx = i
		}
	}
	assert.Greater(t, putIdx, indexerIdx, "FGA put must come after indexer publish")
}

func TestKeyContactWriter_Create_SelfHeal_ReturnsExistingWithoutWrite(t *testing.T) {
	existing := &model.KeyContact{
		UID:           testKCUID,
		MembershipUID: testMembershipUID,
		Email:         "alice@example.com",
		Role:          "Technical Advisory Committee (TAC) Representative",
		Status:        "Active",
		UpdatedAt:     time.Now(),
	}
	storage := newSeededStorage(existing)
	pub := &trackingPublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{UID: testMembershipUID}}, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil
	}))

	in := svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID,
		FirstName:     "Alice",
		LastName:      "Smith",
		Email:         "alice@example.com", // same email + role → self-heal
		Role:          "Technical Advisory Committee (TAC) Representative",
	}
	kc, err := w.Create(context.Background(), in)

	require.NoError(t, err)
	assert.Equal(t, testKCUID, kc.UID, "self-heal must return existing record")
	assert.Empty(t, pub.calls(), "self-heal must not publish anything")
}

// ── Update tests ──────────────────────────────────────────────────────────

func TestKeyContactWriter_Update_NoOpETag_SkipsPublish(t *testing.T) {
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID,
		Email: "alice@example.com", Role: "role-a", Status: "Active",
		UpdatedAt: time.Now(),
	}
	storage := newSeededStorage(kc)
	pub := &trackingPublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "alice", nil
	}))

	// UpdateKeyContact with same data → writer returns identical kc → ETag unchanged → skip publish
	in := svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID,
		UID:           testKCUID,
	}
	_, err := w.Update(context.Background(), in)

	require.NoError(t, err)
	assert.Empty(t, pub.calls(), "no-op update must not publish")
}

func TestKeyContactWriter_Update_EmailChange_PutBeforeRemove(t *testing.T) {
	oldKC := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID,
		Email: "old@example.com", Username: "old-sub",
		Role: "role-a", Status: "Active", UpdatedAt: time.Now(),
	}
	storage := newSeededStorage(oldKC)
	pub := &trackingPublisher{}
	newEmail := "new@example.com"

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, email string) (string, error) {
		if strings.EqualFold(email, "new@example.com") {
			return "new-sub", nil
		}
		return "old-sub", nil
	}))

	in := svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID,
		UID:           testKCUID,
		Email:         &newEmail,
	}
	_, err := w.Update(context.Background(), in)

	require.NoError(t, err)

	// Ordering invariant: FGA put (new sub) BEFORE FGA remove (old sub)
	calls := pub.calls()
	putIdx := -1
	removeIdx := -1
	for i, c := range calls {
		if strings.Contains(c, fgaconstants.GenericMemberPutSubject) {
			putIdx = i
		}
		if strings.Contains(c, fgaconstants.GenericMemberRemoveSubject) {
			removeIdx = i
		}
	}
	require.NotEqual(t, -1, putIdx, "FGA put must be called")
	require.NotEqual(t, -1, removeIdx, "FGA remove must be called on email change")
	assert.Less(t, putIdx, removeIdx, "FGA put must precede FGA remove")
}

func TestKeyContactWriter_Update_EmailChange_RemoveError_NotPropagated(t *testing.T) {
	oldKC := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID,
		Email: "old@example.com", Username: "old-sub",
		Role: "role-a", Status: "Active", UpdatedAt: time.Now(),
	}
	storage := newSeededStorage(oldKC)
	pub := &errorFGARemovePublisher{}
	newEmail := "new@example.com"

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, email string) (string, error) {
		if strings.EqualFold(email, "new@example.com") {
			return "new-sub", nil
		}
		return "old-sub", nil
	}))

	in := svc.KeyContactUpdateInput{UID: testKCUID, MembershipUID: testMembershipUID, Email: &newEmail}
	_, err := w.Update(context.Background(), in)

	assert.NoError(t, err, "FGA remove error on email change must NOT be propagated")
}

func TestKeyContactWriter_Update_IfMatch_Mismatch_PreconditionFailed(t *testing.T) {
	kc := &model.KeyContact{UID: testKCUID, MembershipUID: testMembershipUID, UpdatedAt: time.Now()}
	storage := newSeededStorage(kc)

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, &trackingPublisher{}, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil
	}))

	in := svc.KeyContactUpdateInput{UID: testKCUID, MembershipUID: testMembershipUID, IfMatch: "\"stale\""}
	_, err := w.Update(context.Background(), in)

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err))
}

// ── Delete tests ──────────────────────────────────────────────────────────

func TestKeyContactWriter_Delete_LegacyAuth0Username_ResolvesToLFID(t *testing.T) {
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID,
		Email: "alice@example.com", Username: "auth0|alice",
	}
	storage := newSeededStorage(kc)
	pub := &accessPayloadPublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, email string) (string, error) {
		if strings.EqualFold(email, "alice@example.com") {
			return "alice", nil
		}
		return "", nil
	}))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.NoError(t, err)
	require.Len(t, pub.accessMsgs, 1)
	msg, ok := pub.accessMsgs[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	data, ok := msg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, "alice", data.Username)
}

func TestKeyContactWriter_Delete_OrderingInvariant_DeleteThenIndexerThenFGARemove(t *testing.T) {
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID,
		Email: "alice@example.com", Username: "alice",
	}
	storage := newSeededStorage(kc)
	pub := &trackingPublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "alice", nil
	}))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.NoError(t, err)
	calls := pub.calls()
	indexerIdx := -1
	removeIdx := -1
	for i, c := range calls {
		if strings.Contains(c, "indexer:") {
			indexerIdx = i
		}
		if strings.Contains(c, fgaconstants.GenericMemberRemoveSubject) {
			removeIdx = i
		}
	}
	require.NotEqual(t, -1, indexerIdx, "indexer must be called on delete")
	require.NotEqual(t, -1, removeIdx, "FGA remove must be called on delete")
	assert.Less(t, indexerIdx, removeIdx, "indexer must precede FGA remove")
}

func TestKeyContactWriter_Delete_FGARemoveError_Propagated(t *testing.T) {
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID,
		Email: "alice@example.com", Username: "alice",
	}
	storage := newSeededStorage(kc)
	pub := &errorFGARemovePublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "alice", nil
	}))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.Error(t, err, "FGA remove failure on delete must be propagated")
}

func TestKeyContactWriter_Delete_IfMatch_Mismatch_PreconditionFailed(t *testing.T) {
	kc := &model.KeyContact{UID: testKCUID, MembershipUID: testMembershipUID, UpdatedAt: time.Now()}
	storage := newSeededStorage(kc)

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, &trackingPublisher{}, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil
	}))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID, IfMatch: "\"stale\""}
	err := w.Delete(context.Background(), in)

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err))
}

func TestKeyContactWriter_Delete_IfMatch_Mismatch_PublishesNothing(t *testing.T) {
	kc := &model.KeyContact{UID: testKCUID, MembershipUID: testMembershipUID, UpdatedAt: time.Now()}
	storage := newSeededStorage(kc)
	pub := &trackingPublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil
	}))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID, IfMatch: "\"stale\""}
	err := w.Delete(context.Background(), in)

	require.True(t, pkgerrors.IsPreconditionFailed(err))
	assert.Empty(t, pub.calls(), "precondition failure must publish no indexer/FGA messages")
}

// ── Already-missing sweep tests ───────────────────────────────────────────────
// When Delete finds the source record is gone (NotFound), it runs a best-effort
// index/FGA sweep before returning 404. These tests exercise that path via Delete.

func TestKeyContactWriter_Delete_AlreadyMissing_PublishesIndexerAndFGA(t *testing.T) {
	pub := &trackingPublisher{}
	w := newKCWriter(newSeededStorage(), &seededPMReader{pm: &model.ProjectMembership{}}, pub,
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil }))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.True(t, pkgerrors.IsNotFound(err), "already-missing Delete must return NotFound")
	calls := pub.calls()
	var sawIndexer, sawFGARemove bool
	for _, c := range calls {
		if strings.Contains(c, "indexer:") {
			sawIndexer = true
		}
		if strings.Contains(c, fgaconstants.GenericMemberRemoveSubject) {
			sawFGARemove = true
		}
	}
	assert.True(t, sawIndexer, "already-missing Delete must publish an indexer delete")
	assert.True(t, sawFGARemove,
		"already-missing Delete must publish an FGA remove even with empty username")
}

func TestKeyContactWriter_Delete_AlreadyMissing_FGARemovePayloadIsObjectIDOnly(t *testing.T) {
	pub := &accessPayloadPublisher{}
	w := newKCWriter(newSeededStorage(), &seededPMReader{pm: &model.ProjectMembership{}}, pub,
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil }))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.True(t, pkgerrors.IsNotFound(err), "already-missing Delete must return NotFound")
	require.Len(t, pub.accessMsgs, 1, "exactly one FGA remove must be published on the sweep")
	msg, ok := pub.accessMsgs[0].(fgatypes.GenericFGAMessage)
	require.True(t, ok)
	data, ok := msg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, testMembershipUID, data.UID)
	assert.Empty(t, data.Username, "sweep FGA remove must be object-id-only (empty username)")
}

func TestKeyContactWriter_Delete_AlreadyMissing_FGAPublishFailsNoSideEffect(t *testing.T) {
	// FGA Access fails — sweep must not panic or propagate; Delete still returns NotFound.
	pub := &errorFGARemovePublisher{}
	w := newKCWriter(newSeededStorage(), &seededPMReader{pm: &model.ProjectMembership{}}, pub,
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil }))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	var err error
	assert.NotPanics(t, func() { err = w.Delete(context.Background(), in) })
	assert.True(t, pkgerrors.IsNotFound(err), "FGA publish failure must not mask the NotFound")
}

func TestKeyContactWriter_Delete_MembershipMismatch_ReturnsNotFoundNoPublish(t *testing.T) {
	// Safety invariant: when the requested contact UID exists but belongs to a
	// DIFFERENT membership, the endpoint must return 404 with NO indexer or FGA
	// publish. Tombstoning by UID here would delete the contact's real indexed
	// document and revoke FGA tuples owned by the other membership.
	kc := &model.KeyContact{UID: testKCUID, MembershipUID: testMembershipUID}
	storage := newSeededStorage(kc)
	pub := &trackingPublisher{}
	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub,
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil }))

	in := svc.KeyContactDeleteInput{MembershipUID: "other-membership-uid", UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "cross-membership delete must return 404 not 403")
	assert.Empty(t, pub.calls(),
		"cross-membership 404 MUST NOT publish any indexer or FGA message — the record belongs to another membership")
}

// ── Delete error-propagation tests ───────────────────────────────────────────

func TestKeyContactWriter_Delete_BackendError_PropagatesError(t *testing.T) {
	// Storage returns a non-NotFound error → original error returned, nothing published.
	storage := &errorGetStorage{err: pkgerrors.NewUnexpected("nats unavailable", nil)}
	pub := &trackingPublisher{}

	w := newKCWriter(storage, &seededPMReader{pm: &model.ProjectMembership{}}, pub, userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil
	}))

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.Error(t, err)
	assert.False(t, pkgerrors.IsNotFound(err), "a backend error must not be reported as NotFound")
	assert.Empty(t, pub.calls(), "a non-NotFound read error must publish nothing")
}

func TestKeyContactWriter_Delete_WriteFailsAfterRead_PublishesNothing(t *testing.T) {
	// GetKeyContact succeeds (record exists), but DeleteKeyContact write fails.
	// Expectation: original write error returned, no indexer-delete, no FGA-remove.
	kc := &model.KeyContact{UID: testKCUID, MembershipUID: testMembershipUID, Email: "alice@example.com", Username: "alice"}
	storage := newSeededStorage(kc)
	pub := &trackingPublisher{}

	// Wire a writer whose DeleteKeyContact always fails.
	failWriter := &failingKeyContactWriter{err: pkgerrors.NewUnexpected("salesforce write unavailable", nil)}
	w := svc.NewKeyContactWriter(
		svc.WithKCStorage(storage),
		svc.WithKCWriter(failWriter),
		svc.WithKCProjectMembershipReader(&seededPMReader{pm: &model.ProjectMembership{}}),
		svc.WithKCPublisher(pub),
		svc.WithKCUserReader(userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice", nil })),
	)

	in := svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID}
	err := w.Delete(context.Background(), in)

	require.Error(t, err)
	assert.False(t, pkgerrors.IsNotFound(err), "a write failure must not be reported as NotFound")
	assert.Empty(t, pub.calls(), "a delete write failure must not publish any indexer or FGA messages")
}

// failingKeyContactWriter is a port.KeyContactWriter whose DeleteKeyContact always fails.
type failingKeyContactWriter struct {
	mock.MockKeyContactWriterWithOK
	err error
}

func (w *failingKeyContactWriter) DeleteKeyContact(_ context.Context, _ string, _ string) error {
	return w.err
}

// ── Org-dashboard provisioning tests (Tasks 4, 5, 6) ─────────────────────────

const testOrgSFID = "001000000000000AAA"

func TestKeyContactWriter_Create_Registered_SilentProvision(t *testing.T) {
	// Registered user + send_invite=false → AddPrincipal called with SuppressNotification=true.
	pm := &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: testOrgSFID}
	spy := &spyOrgSettings{}
	storage := newSeededStorage()

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: pm}, &trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	_, err := w.Create(context.Background(), svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID, FirstName: "Alice", LastName: "Smith",
		Email: "alice@example.com", Role: "Technical Contact", SendInvite: false,
	})

	require.NoError(t, err)
	require.Len(t, spy.adds, 1)
	assert.True(t, spy.adds[0].SuppressNotification, "SuppressNotification must be true when send_invite=false")
	assert.Equal(t, testOrgSFID, spy.adds[0].OrgUID)
	assert.Equal(t, model.B2BOrgRoleAuditor, spy.adds[0].InvitedAs, "non-voting role maps to auditor")
}

func TestKeyContactWriter_Create_VotingContact_MapsToWriter(t *testing.T) {
	// Representative/Voting Contact role → InvitedAs=writer.
	pm := &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: testOrgSFID}
	spy := &spyOrgSettings{}
	storage := newSeededStorage()

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: pm}, &trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "bob-sub", nil }),
		spy,
	)

	_, err := w.Create(context.Background(), svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID, FirstName: "Bob", LastName: "Jones",
		Email: "bob@example.com", Role: "Representative/Voting Contact", SendInvite: false,
	})

	require.NoError(t, err)
	require.Len(t, spy.adds, 1)
	assert.Equal(t, model.B2BOrgRoleWriter, spy.adds[0].InvitedAs, "voting contact must map to writer")
}

func TestKeyContactWriter_Create_Unregistered_NoInvite_NoProvision(t *testing.T) {
	// Unregistered + send_invite=false → AddPrincipal NOT called.
	pm := &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: testOrgSFID}
	spy := &spyOrgSettings{}
	storage := newSeededStorage()

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: pm}, &trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil }),
		spy,
	)

	_, err := w.Create(context.Background(), svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID, FirstName: "Carol", LastName: "Doe",
		Email: "carol@example.com", Role: "Technical Contact", SendInvite: false,
	})

	require.NoError(t, err)
	assert.Empty(t, spy.adds, "AddPrincipal must NOT be called for unregistered user with send_invite=false")
}

func TestKeyContactWriter_Create_Unregistered_WithInvite_CallsAdd(t *testing.T) {
	// Unregistered + send_invite=true → AddPrincipal called with SuppressNotification=false.
	pm := &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: testOrgSFID}
	spy := &spyOrgSettings{}
	storage := newSeededStorage()

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: pm}, &trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil }),
		spy,
	)

	_, err := w.Create(context.Background(), svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID, FirstName: "Dave", LastName: "Lee",
		Email: "dave@example.com", Role: "Technical Contact", SendInvite: true,
	})

	require.NoError(t, err)
	require.Len(t, spy.adds, 1)
	assert.False(t, spy.adds[0].SuppressNotification, "SuppressNotification must be false when send_invite=true")
}

func TestKeyContactWriter_Create_AddPrincipalConflict_CreateSucceeds(t *testing.T) {
	// AddPrincipal returning Conflict must not fail Create (same email holds another role).
	pm := &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: testOrgSFID}
	spy := &spyOrgSettings{addErr: pkgerrors.NewConflict("already has access")}
	storage := newSeededStorage()

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: pm}, &trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	_, err := w.Create(context.Background(), svc.KeyContactCreateInput{
		MembershipUID: testMembershipUID, FirstName: "Alice", LastName: "Smith",
		Email: "alice@example.com", Role: "Technical Contact", SendInvite: false,
	})

	require.NoError(t, err, "Conflict from AddPrincipal must be swallowed by Create")
}

func TestKeyContactWriter_Update_RoleChange_NoEmailChange_RemapsOrgDashboard(t *testing.T) {
	// Role upgrade (Technical Contact → Representative/Voting Contact) without email change
	// → ChangePrincipalRole called with InvitedAs=writer; AddPrincipal/RemovePrincipal NOT called.
	votingRole := "Representative/Voting Contact"
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: "Technical Contact",
	}
	storage := newSeededStorage(kc)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: testKCUID,
		Role: &votingRole,
	})

	require.NoError(t, err)
	require.Len(t, spy.roleChanges, 1, "ChangePrincipalRole must be called on role change")
	assert.Equal(t, "alice@example.com", spy.roleChanges[0].Email)
	assert.Equal(t, model.B2BOrgRoleWriter, spy.roleChanges[0].InvitedAs)
	assert.Empty(t, spy.adds, "AddPrincipal must NOT be called on role-only change")
	assert.Empty(t, spy.removes, "RemovePrincipal must NOT be called on role-only change")
}

func TestKeyContactWriter_Update_NoRoleChange_NoEmailChange_SkipsRemap(t *testing.T) {
	// No role in input → ChangePrincipalRole NOT called.
	title := "CTO"
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: "Technical Contact",
	}
	storage := newSeededStorage(kc)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: testKCUID,
		Title: &title,
	})

	require.NoError(t, err)
	assert.Empty(t, spy.roleChanges, "ChangePrincipalRole must NOT be called when role is unchanged")
}

func TestKeyContactWriter_Update_NoEmailNoRole_PreservesRole(t *testing.T) {
	// When neither email nor role changes, the returned KeyContact must still
	// carry the original role (mock returns "" for unchanged Role input — coalesce).
	title := "CTO"
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: "Technical Contact",
	}
	storage := newSeededStorage(kc)

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		&spyOrgSettings{},
	)

	got, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: testKCUID,
		Title: &title,
	})

	require.NoError(t, err)
	assert.Equal(t, "Technical Contact", got.Role, "role must be preserved when not changed")
}

func TestKeyContactWriter_Update_RoleChange_NotFound_UpdateSucceeds(t *testing.T) {
	// ChangePrincipalRole returning NotFound (contact never provisioned) is a no-op.
	votingRole := "Representative/Voting Contact"
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: "Technical Contact",
	}
	storage := newSeededStorage(kc)
	spy := &spyOrgSettings{changeErr: pkgerrors.NewNotFound("principal not found")}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: testKCUID,
		Role: &votingRole,
	})

	require.NoError(t, err, "NotFound from ChangePrincipalRole must be swallowed")
}

func TestKeyContactWriter_Update_EmailChange_ProvisionNewRevokeOld(t *testing.T) {
	// Email change: new email provisioned + old email revoke guard run.
	// Old email is the only active contact → RemovePrincipal called.
	const orgUID = testOrgSFID
	oldKC := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: orgUID,
		Email: "old@example.com", Status: "Active", Role: "Technical Contact",
		FirstName: "Alice", LastName: "Smith",
	}
	storage := newSeededStorage(oldKC)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{UID: testMembershipUID, B2BOrgUID: orgUID}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "new-sub", nil }),
		spy,
	)

	newEmail := "new@example.com"
	_, err := w.Update(context.Background(), svc.KeyContactUpdateInput{
		MembershipUID: testMembershipUID, UID: testKCUID,
		Email: &newEmail, SendInvite: false,
	})

	require.NoError(t, err)
	require.Len(t, spy.adds, 1, "new email must be provisioned")
	assert.Equal(t, "new@example.com", spy.adds[0].Email)
	require.Len(t, spy.removes, 1, "old email must be revoked (last active contact)")
	assert.Equal(t, "old@example.com", spy.removes[0].Email)
}

func TestKeyContactWriter_Delete_LastActive_RevokesOrgAccess(t *testing.T) {
	// Delete when email is the only active contact in org → RemovePrincipal called.
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: "Technical Contact",
	}
	storage := newSeededStorage(kc)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	err := w.Delete(context.Background(), svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID})

	require.NoError(t, err)
	require.Len(t, spy.removes, 1, "RemovePrincipal must be called when no other active role remains")
	assert.Equal(t, "alice@example.com", spy.removes[0].Email)
}

func TestKeyContactWriter_Delete_OtherActiveRole_SameLevel_SkipsRevoke(t *testing.T) {
	// D=auditor, R=auditor: delete one auditor-level KC while another remains — no remove, no downgrade.
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: constants.RoleNameBillingContact,
	}
	otherKC := &model.KeyContact{
		UID: "other-kc-uid", MembershipUID: "other-membership", B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: constants.RoleNameTechnicalContact,
	}
	storage := newSeededStorage(kc, otherKC)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	err := w.Delete(context.Background(), svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID})

	require.NoError(t, err)
	assert.Empty(t, spy.removes, "RemovePrincipal must NOT be called when another active auditor-level role remains")
	assert.Empty(t, spy.roleChanges, "ChangePrincipalRole must NOT be called when roles are at the same level")
}

func TestKeyContactWriter_Delete_VotingContact_AuditorRemains_DowngradesRole(t *testing.T) {
	// D=writer, R=auditor: delete Voting Contact while Billing Contact stays → downgrade to auditor.
	votingKC := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: constants.RoleNameRepresentativeVotingContact,
	}
	billingKC := &model.KeyContact{
		UID: "billing-kc-uid", MembershipUID: "other-membership", B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: constants.RoleNameBillingContact,
	}
	storage := newSeededStorage(votingKC, billingKC)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	err := w.Delete(context.Background(), svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID})

	require.NoError(t, err)
	assert.Empty(t, spy.removes, "RemovePrincipal must NOT be called when another active role remains")
	require.Len(t, spy.roleChanges, 1, "ChangePrincipalRole must be called to downgrade from writer to auditor")
	assert.Equal(t, model.B2BOrgRoleAuditor, spy.roleChanges[0].InvitedAs, "must downgrade to auditor (max remaining role)")
	assert.Equal(t, "alice@example.com", spy.roleChanges[0].Email)
}

func TestKeyContactWriter_Delete_VotingContact_AnotherVotingRemains_NoChange(t *testing.T) {
	// D=writer, R=writer: delete one Voting Contact while another remains — no action.
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: constants.RoleNameRepresentativeVotingContact,
	}
	otherVoting := &model.KeyContact{
		UID: "other-voting-uid", MembershipUID: "other-membership", B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active", Role: constants.RoleNameRepresentativeVotingContact,
	}
	storage := newSeededStorage(kc, otherVoting)
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	err := w.Delete(context.Background(), svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID})

	require.NoError(t, err)
	assert.Empty(t, spy.removes, "RemovePrincipal must NOT be called when another writer-level KC exists")
	assert.Empty(t, spy.roleChanges, "ChangePrincipalRole must NOT be called when remaining role is equal or higher")
}

func TestKeyContactWriter_Delete_OrgScanError_SkipsRevoke(t *testing.T) {
	// Fail-safe: if the org scan errors (e.g. Salesforce down), skip revoke rather
	// than revoking prematurely and stranding a legitimate access holder.
	kc := &model.KeyContact{
		UID: testKCUID, MembershipUID: testMembershipUID, B2BOrgUID: testOrgSFID,
		Email: "alice@example.com", Status: "Active",
	}
	storage := newSeededStorage(kc)
	storage.listOrgErr = errors.New("salesforce unavailable")
	spy := &spyOrgSettings{}

	w := newKCWriterWithOrgSettings(storage, &seededPMReader{pm: &model.ProjectMembership{}},
		&trackingPublisher{},
		userReaderFunc(func(_ context.Context, _ string) (string, error) { return "alice-sub", nil }),
		spy,
	)

	err := w.Delete(context.Background(), svc.KeyContactDeleteInput{MembershipUID: testMembershipUID, UID: testKCUID})

	require.NoError(t, err)
	assert.Empty(t, spy.removes, "RemovePrincipal must NOT be called when the org scan fails (fail-safe)")
}
