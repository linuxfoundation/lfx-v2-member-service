// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// newBrokerRepairStore returns a CDCRepairStore over a real JetStream KV bucket
// on a throwaway nats-server, created like the service creates it (default
// history of 1). The fakeKV tests assume how the broker orders keys and maps
// stale-revision errors; these tests check those assumptions against the real
// thing. They need a nats-server binary and are skipped without one, so CI
// without it does not run them.
func newBrokerRepairStore(t *testing.T) *CDCRepairStore {
	t.Helper()
	bin, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server is not installed; skipping the real-broker repair store tests")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, bin, "-a", "127.0.0.1", "-p", fmt.Sprint(port), "-js", "-sd", t.TempDir())
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		cancel()
		_ = server.Wait()
	})

	var conn *natsgo.Conn
	require.Eventually(t, func() bool {
		var connErr error
		conn, connErr = natsgo.Connect(fmt.Sprintf("nats://127.0.0.1:%d", port))
		return connErr == nil
	}, 10*time.Second, 50*time.Millisecond, "nats-server did not start")
	t.Cleanup(conn.Close)

	js, err := jetstream.New(conn)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: constants.KVBucketNameCDCRepair})
	require.NoError(t, err)
	return &CDCRepairStore{client: &NATSClient{kvStore: map[string]jetstream.KeyValue{constants.KVBucketNameCDCRepair: kv}}}
}

// The key lister must replay each key's last revision in stream order, so a
// requeued marker is listed after every marker not rewritten since; without
// that a page of failing lookups starves the markers behind it.
func TestBrokerCDCRepairStore_RequeuePending_ListsLast(t *testing.T) {
	ctx := context.Background()
	store := newBrokerRepairStore(t)
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

	// A limited page is the front of that order.
	page, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{second, third}, pendingUIDs(page))
}

// A stale revision is a Conflict that leaves the newer marker, and a settled
// marker is recreated over the delete marker, losing to a fresh write.
func TestBrokerCDCRepairStore_RequeuePending_ConflictAndRecreate(t *testing.T) {
	ctx := context.Background()
	store := newBrokerRepairStore(t)
	sfid := canonicalSFID(t, "a0F000000000001")
	require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	selected, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)

	// Another drain requeues first: this caller's revision is stale.
	require.NoError(t, store.RequeuePending(ctx, selected[0], 3))
	err = store.RequeuePending(ctx, selected[0], 1)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "a stale revision is a Conflict, got %v", err)
	stored, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, 3, stored[0].LookupAttempts, "the newer marker survives")

	// Another drain settles the marker: the requeue recreates it over the
	// delete marker.
	require.NoError(t, store.DeletePending(ctx, constants.ReindexTypeKeyContact, sfid, stored[0].Revision))
	gone, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Empty(t, gone)
	require.NoError(t, store.RequeuePending(ctx, selected[0], 1))
	recreated, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Len(t, recreated, 1)
	assert.Equal(t, 1, recreated[0].LookupAttempts)

	// A fresh write after another settle is never overwritten by the recreate.
	require.NoError(t, store.DeletePending(ctx, constants.ReindexTypeKeyContact, sfid, recreated[0].Revision))
	require.NoError(t, store.PutPending(ctx, constants.ReindexTypeKeyContact, sfid))
	fresh, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Len(t, fresh, 1)
	err = store.RequeuePending(ctx, selected[0], 4)
	require.Error(t, err)
	assert.True(t, errs.IsConflict(err), "got %v", err)
	after, err := store.ListPending(ctx, constants.ReindexTypeKeyContact, 10)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, fresh[0].Revision, after[0].Revision, "the fresh marker is untouched")
	assert.Equal(t, 0, after[0].LookupAttempts)
}
