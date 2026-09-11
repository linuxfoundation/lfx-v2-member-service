// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// fakeKVEntry implements jetstream.KeyValueEntry over an in-memory message.
type fakeKVEntry struct {
	key   string
	value []byte
	rev   uint64
	op    jetstream.KeyValueOp
}

func (e fakeKVEntry) Bucket() string                  { return constants.KVBucketNameCache }
func (e fakeKVEntry) Key() string                     { return e.key }
func (e fakeKVEntry) Value() []byte                   { return e.value }
func (e fakeKVEntry) Revision() uint64                { return e.rev }
func (e fakeKVEntry) Created() time.Time              { return time.Time{} }
func (e fakeKVEntry) Delta() uint64                   { return 0 }
func (e fakeKVEntry) Operation() jetstream.KeyValueOp { return e.op }

// fakeKV is an in-memory jetstream.KeyValue implementing exactly the subject
// message log semantics the CAS write-back depends on: every put and delete
// marker occupies a stream sequence, Get hides delete markers behind
// ErrKeyNotFound, Update enforces the expected last sequence for the key
// (0 meaning "no message at all", tombstones included), and History exposes
// the markers. Unimplemented methods panic via the embedded nil interface.
type fakeKV struct {
	jetstream.KeyValue

	mu   sync.Mutex
	seq  uint64
	msgs map[string][]fakeKVEntry
}

func newFakeKV() *fakeKV { return &fakeKV{msgs: map[string][]fakeKVEntry{}} }

func (f *fakeKV) last(key string) (fakeKVEntry, bool) {
	entries := f.msgs[key]
	if len(entries) == 0 {
		return fakeKVEntry{}, false
	}
	return entries[len(entries)-1], true
}

func (f *fakeKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, ok := f.last(key)
	if !ok || last.op != jetstream.KeyValuePut {
		return nil, jetstream.ErrKeyNotFound
	}
	return last, nil
}

func (f *fakeKV) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lastSeq uint64
	if last, ok := f.last(key); ok {
		lastSeq = last.rev
	}
	if lastSeq != revision {
		// The server rejects with wrong-last-sequence (API code 10071), which
		// errors.Is-matches jetstream.ErrKeyExists; return that sentinel.
		return 0, jetstream.ErrKeyExists
	}
	f.seq++
	f.msgs[key] = append(f.msgs[key], fakeKVEntry{key: key, value: value, rev: f.seq, op: jetstream.KeyValuePut})
	return f.seq, nil
}

func (f *fakeKV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.msgs[key] = append(f.msgs[key], fakeKVEntry{key: key, rev: f.seq, op: jetstream.KeyValueDelete})
	return nil
}

func (f *fakeKV) History(_ context.Context, key string, _ ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries := f.msgs[key]
	if len(entries) == 0 {
		return nil, jetstream.ErrKeyNotFound
	}
	out := make([]jetstream.KeyValueEntry, len(entries))
	for i, e := range entries {
		out[i] = e
	}
	return out, nil
}

func newFakeStorage(kv jetstream.KeyValue) *Storage {
	return &Storage{
		client:    &NATSClient{kvStore: map[string]jetstream.KeyValue{constants.KVBucketNameCache: kv}},
		ttlConfig: DefaultTTLConfig,
	}
}

// TestPutMembershipAtRevision_MissThenEviction_Conflicts covers the
// miss → concurrent write → CDC eviction → stale write-back race: the
// revision-0 write must be strict, not kv.Create, which re-reads the delete
// marker at write time, retries over it, and resurrects the stale record.
func TestPutMembershipAtRevision_MissThenEviction_Conflicts(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := newFakeStorage(kv)
	uid := "pm-race-1"

	// Reader A misses on a key with no history.
	result, err := s.GetMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, CacheStatusMiss, result.Status)
	assert.Zero(t, result.Revision)

	// While A's Salesforce fetch is in flight, writer B populates fresh data
	// and a CDC event then evicts it.
	require.NoError(t, s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-new"}, 0))
	require.NoError(t, s.DeleteMembership(ctx, uid))

	// A's write-back at its read revision (0) must lose: the key now has a
	// delete marker, and writing over it would resurrect pre-eviction data.
	err = s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-stale"}, 0)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "expected Conflict, got %v", err)

	// The eviction stands: the next read is a miss, not the stale record.
	result, err = s.GetMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, CacheStatusMiss, result.Status)
}

// TestGetMembership_MissOnTombstone_CarriesMarkerRevision verifies a miss
// caused by an eviction returns the delete marker's revision, and that a
// write-back conditioned on that marker succeeds while the marker is still the
// latest operation (normal repopulation after an eviction).
func TestGetMembership_MissOnTombstone_CarriesMarkerRevision(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := newFakeStorage(kv)
	uid := "pm-tomb-1"

	require.NoError(t, s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-1"}, 0))
	require.NoError(t, s.DeleteMembership(ctx, uid))

	result, err := s.GetMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, CacheStatusMiss, result.Status)
	assert.Equal(t, uint64(2), result.Revision, "a tombstone miss must carry the delete marker's revision")

	// Repopulating at the observed marker revision succeeds.
	require.NoError(t, s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-1"}, result.Revision))
	result, err = s.GetMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, CacheStatusFresh, result.Status)
	assert.Equal(t, "p-1", result.Value.ProjectUID)
}

// TestPutMembershipAtRevision_TombstoneReadThenNewerEviction_Conflicts extends
// the race to a tombstone-observed read: a refresh that read marker revision N
// must not write back once a newer put and eviction have moved the key past N.
func TestPutMembershipAtRevision_TombstoneReadThenNewerEviction_Conflicts(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := newFakeStorage(kv)
	uid := "pm-tomb-2"

	require.NoError(t, s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-1"}, 0))
	require.NoError(t, s.DeleteMembership(ctx, uid))

	result, err := s.GetMembership(ctx, uid)
	require.NoError(t, err)
	readRevision := result.Revision
	require.Equal(t, uint64(2), readRevision)

	// Another writer repopulates fresh data and a newer CDC event evicts it.
	require.NoError(t, s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-new"}, readRevision))
	require.NoError(t, s.DeleteMembership(ctx, uid))

	err = s.PutMembershipAtRevision(ctx, &model.ProjectMembership{UID: uid, ProjectUID: "p-stale"}, readRevision)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "expected Conflict, got %v", err)
}

// TestPutKeyContactsForMembershipAtRevision_MissThenEviction_Conflicts covers
// the same race for the grouped key-contacts cache the member-tiers
// eligibility revalidation reads.
func TestPutKeyContactsForMembershipAtRevision_MissThenEviction_Conflicts(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	s := newFakeStorage(kv)
	uid := "pm-kc-race-1"

	result, err := s.GetKeyContactsForMembership(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, CacheStatusMiss, result.Status)
	require.Zero(t, result.Revision)

	fresh := []*model.KeyContact{{UID: "kc-1", MembershipUID: uid, Username: "alice", Status: "Active"}}
	require.NoError(t, s.PutKeyContactsForMembershipAtRevision(ctx, uid, fresh, 0))
	require.NoError(t, s.DeleteKeyContactsForMembership(ctx, uid))

	stale := []*model.KeyContact{{UID: "kc-1", MembershipUID: uid, Username: "mallory", Status: "Active"}}
	err = s.PutKeyContactsForMembershipAtRevision(ctx, uid, stale, 0)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "expected Conflict, got %v", err)

	result, err = s.GetKeyContactsForMembership(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, CacheStatusMiss, result.Status, "the eviction must stand; stale contacts must not be resurrected")
}
