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
)

type fakeKVEntry struct {
	jetstream.KeyValueEntry
	value   []byte
	created time.Time
}

func (e fakeKVEntry) Value() []byte      { return e.value }
func (e fakeKVEntry) Created() time.Time { return e.created }

type fakeKV struct {
	jetstream.KeyValue
	entry     jetstream.KeyValueEntry
	getErr    error
	deleteErr error
	deleted   []string
}

func (f *fakeKV) Get(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.entry, nil
}

func (f *fakeKV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func TestReplayStore_Reset_DeletesCursorAndReturnsLastSavedAt(t *testing.T) {
	savedAt := time.Date(2026, 7, 22, 20, 58, 29, 0, time.UTC)
	kv := &fakeKV{entry: fakeKVEntry{value: []byte("old"), created: savedAt}}

	got, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents")

	require.NoError(t, err)
	assert.Equal(t, savedAt, got)
	assert.Equal(t, []string{"pubsub-replay._data_ChangeEvents"}, kv.deleted)
}

func TestReplayStore_Reset_MissingCursorIsNotAnError(t *testing.T) {
	kv := &fakeKV{getErr: jetstream.ErrKeyNotFound}

	got, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents")

	require.NoError(t, err)
	assert.True(t, got.IsZero())
	assert.Empty(t, kv.deleted)
}

func TestReplayStore_Reset_ReadFailureReturnsError(t *testing.T) {
	kv := &fakeKV{getErr: errors.New("nats down")}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents")

	require.Error(t, err)
	assert.Empty(t, kv.deleted)
}

func TestReplayStore_Reset_DeleteFailureReturnsError(t *testing.T) {
	kv := &fakeKV{
		entry:     fakeKVEntry{value: []byte("old"), created: time.Now()},
		deleteErr: errors.New("nats down"),
	}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents")

	require.Error(t, err)
}
