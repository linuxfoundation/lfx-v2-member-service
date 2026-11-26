// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// The fakeKV in storage_test.go models the subject message log. These methods
// add what the repair store needs on top of it: Put, create-only Create (which,
// like nats.go, writes over a delete marker but not over a live value) and a
// filtered key lister that replays each key's last revision in stream-sequence
// order, the ordering ListPending's starvation guarantee depends on.

func (f *fakeKV) Put(_ context.Context, key string, value []byte) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.msgs[key] = append(f.msgs[key], fakeKVEntry{key: key, value: value, rev: f.seq, op: jetstream.KeyValuePut})
	return f.seq, nil
}

func (f *fakeKV) Create(_ context.Context, key string, value []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.last(key); ok && last.op == jetstream.KeyValuePut {
		return 0, jetstream.ErrKeyExists
	}
	f.seq++
	f.msgs[key] = append(f.msgs[key], fakeKVEntry{key: key, value: value, rev: f.seq, op: jetstream.KeyValuePut})
	return f.seq, nil
}

type fakeKeyLister struct{ keys chan string }

func (l *fakeKeyLister) Keys() <-chan string { return l.keys }
func (l *fakeKeyLister) Stop() error         { return nil }

func (f *fakeKV) ListKeysFiltered(_ context.Context, filters ...string) (jetstream.KeyLister, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type live struct {
		key string
		rev uint64
	}
	var matched []live
	for key := range f.msgs {
		last, _ := f.last(key)
		if last.op != jetstream.KeyValuePut {
			continue
		}
		for _, filter := range filters {
			if strings.HasPrefix(key, strings.TrimSuffix(filter, ">")) {
				matched = append(matched, live{key: key, rev: last.rev})
				break
			}
		}
	}
	if len(matched) == 0 {
		return nil, jetstream.ErrNoKeysFound
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].rev < matched[j].rev })
	keys := make(chan string, len(matched))
	for _, m := range matched {
		keys <- m.key
	}
	close(keys)
	return &fakeKeyLister{keys: keys}, nil
}

func newFakeRepairStore() (*CDCRepairStore, *fakeKV) {
	kv := newFakeKV()
	return &CDCRepairStore{client: &NATSClient{kvStore: map[string]jetstream.KeyValue{constants.KVBucketNameCDCRepair: kv}}}, kv
}

// canonicalSFID returns a valid canonical 18-character key contact SFID.
func canonicalSFID(t *testing.T, id15 string) string {
	t.Helper()
	sfid, err := sfuuid.Normalize18(id15)
	require.NoError(t, err)
	return sfid
}

func pendingUIDs(markers []port.RepairMarker) []string {
	uids := make([]string, len(markers))
	for i, m := range markers {
		uids[i] = m.SFID
	}
	return uids
}

// TestCDCRepairStore_RequeuePending_ListsLast pins the guarantee a failing
// lookup cannot starve the markers behind it: a requeued marker is listed
// after every marker not rewritten since, and keeps its skipped_at while its
// attempts are persisted. The mock-backed runner tests assume this ordering;
// this is the check against the store's own key listing.
func TestCDCRepairStore_RequeuePending_ListsLast(t *testing.T) {
	ctx := context.Background()
	store, _ := newFakeRepairStore()
	first, second, third := canonicalSFID(t, "a0F000000000001"), canonicalSFID(t, "a0F000000000002"), canonicalSFID(t, "a0F000000000003")
	for _, sfid := range []string{first, second, third} {
		require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	}

	markers, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Equal(t, []string{first, second, third}, pendingUIDs(markers))

	require.NoError(t, store.RequeuePending(ctx, markers[0], 2))

	markers, err = store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Equal(t, []string{second, third, first}, pendingUIDs(markers))
	assert.Equal(t, 2, markers[2].LookupAttempts)
}

// TestCDCRepairStore_RequeuePending_StaleRevisionKeepsNewerMarker covers a
// marker rewritten since selection (a fresh skip, or another drain's
// requeue): the requeue is a Conflict and the stored marker is untouched.
func TestCDCRepairStore_RequeuePending_StaleRevisionKeepsNewerMarker(t *testing.T) {
	ctx := context.Background()
	store, _ := newFakeRepairStore()
	sfid := canonicalSFID(t, "a0F000000000001")
	require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	selected, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)

	// Another drain requeues the same marker first.
	require.NoError(t, store.RequeuePending(ctx, selected[0], 3))

	err = store.RequeuePending(ctx, selected[0], 1)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "a stale revision is a Conflict, got %v", err)

	stored, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, 3, stored[0].LookupAttempts, "the newer marker's attempts survive")
}

// TestCDCRepairStore_RequeuePending_RecreatesSettledMarker is the two-drain
// race: drain B selected the marker, drain A republished the contact with a
// resolved username and settled the marker, and then B's failed lookup
// published an empty username over that repair. B's requeue must recreate the
// marker with its attempts, or nothing retries the contact.
func TestCDCRepairStore_RequeuePending_RecreatesSettledMarker(t *testing.T) {
	ctx := context.Background()
	store, _ := newFakeRepairStore()
	sfid := canonicalSFID(t, "a0F000000000001")
	require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	selected, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)

	// Drain A settles the marker after a successful publish.
	require.NoError(t, store.DeletePending(ctx, constants.ReindexTypeKeyContact, sfid, selected[0].Revision))

	require.NoError(t, store.RequeuePending(ctx, selected[0], 1))

	stored, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Len(t, stored, 1, "the settled marker is recreated")
	assert.Equal(t, sfid, stored[0].SFID)
	assert.Equal(t, 1, stored[0].LookupAttempts)
	assert.False(t, stored[0].SkippedAt.IsZero(), "skipped_at is kept")
}

// TestCDCRepairStore_RequeuePending_RecreateLosesToFreshWrite: the recreate is
// create-only, so a marker written between the failed update and the recreate
// is never overwritten.
func TestCDCRepairStore_RequeuePending_RecreateLosesToFreshWrite(t *testing.T) {
	ctx := context.Background()
	store, kv := newFakeRepairStore()
	sfid := canonicalSFID(t, "a0F000000000001")
	require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	selected, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.NoError(t, store.DeletePending(ctx, constants.ReindexTypeKeyContact, sfid, selected[0].Revision))

	// The consumer re-skips the contact right after the drain settled it.
	require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	fresh, err := kv.Get(ctx, pendingKey(constants.ReindexTypeKeyContact, sfid))
	require.NoError(t, err)

	err = store.RequeuePending(ctx, selected[0], 4)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "got %v", err)

	after, err := kv.Get(ctx, pendingKey(constants.ReindexTypeKeyContact, sfid))
	require.NoError(t, err)
	assert.Equal(t, fresh.Revision(), after.Revision(), "the fresh marker is untouched")
	assert.JSONEq(t, string(fresh.Value()), string(after.Value()))
}
