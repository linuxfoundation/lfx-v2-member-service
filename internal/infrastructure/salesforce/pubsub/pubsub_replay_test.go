// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package pubsub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

type fakeKVEntry struct {
	jetstream.KeyValueEntry
	value    []byte
	created  time.Time
	revision uint64
}

func (e fakeKVEntry) Value() []byte      { return e.value }
func (e fakeKVEntry) Created() time.Time { return e.created }
func (e fakeKVEntry) Revision() uint64   { return e.revision }

type fakeKV struct {
	jetstream.KeyValue
	entry      jetstream.KeyValueEntry
	getErr     error
	deleteErr  error
	deleted    []string
	deleteOpts int
}

func (f *fakeKV) Get(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.entry, nil
}

func (f *fakeKV) Delete(_ context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, key)
	f.deleteOpts = len(opts)
	return nil
}

func TestReplayStore_Reset_DeletesCursorAndReturnsLastSavedAt(t *testing.T) {
	savedAt := time.Date(2026, 7, 22, 20, 58, 29, 0, time.UTC)
	kv := &fakeKV{entry: fakeKVEntry{value: []byte("old"), created: savedAt, revision: 7}}

	got, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.NoError(t, err)
	assert.Equal(t, savedAt, got)
	assert.Equal(t, []string{"pubsub-replay._data_ChangeEvents"}, kv.deleted)
	assert.Equal(t, 1, kv.deleteOpts, "delete must be conditional on the revision read")
}

func TestReplayStore_Reset_NewerCursorIsNotDeleted(t *testing.T) {
	kv := &fakeKV{entry: fakeKVEntry{value: []byte("newer"), created: time.Now()}}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.ErrorIs(t, err, port.ErrReplayCursorChanged)
	assert.Empty(t, kv.deleted)
}

func TestReplayStore_Reset_MissingCursorIsNotAnError(t *testing.T) {
	kv := &fakeKV{getErr: jetstream.ErrKeyNotFound}

	got, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.NoError(t, err)
	assert.True(t, got.IsZero())
	assert.Empty(t, kv.deleted)
}

func TestReplayStore_Reset_ReadFailureReturnsError(t *testing.T) {
	kv := &fakeKV{getErr: errors.New("nats down")}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.Error(t, err)
	assert.Empty(t, kv.deleted)
}

func TestReplayStore_Reset_DeleteFailureReturnsError(t *testing.T) {
	kv := &fakeKV{
		entry:     fakeKVEntry{value: []byte("old"), created: time.Now()},
		deleteErr: errors.New("wrong last sequence"),
	}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.Error(t, err)
}
