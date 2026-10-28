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

type kvWrite struct {
	op       string
	key      string
	value    []byte
	revision uint64
}

type fakeKV struct {
	jetstream.KeyValue
	entry    jetstream.KeyValueEntry
	getErr   error
	writeErr error
	writes   []kvWrite
}

func (f *fakeKV) Get(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.entry, nil
}

func (f *fakeKV) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.writes = append(f.writes, kvWrite{op: "update", key: key, value: value, revision: revision})
	return revision + 1, nil
}

func (f *fakeKV) Create(_ context.Context, key string, value []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.writes = append(f.writes, kvWrite{op: "create", key: key, value: value})
	return 1, nil
}

func wrongLastSequence() error {
	return &jetstream.APIError{Code: 400, ErrorCode: jetstream.JSErrCodeStreamWrongLastSequence}
}

func TestReplayStore_Reset_MarksCursorAtTheRevisionRead(t *testing.T) {
	savedAt := time.Date(2026, 7, 22, 20, 58, 29, 0, time.UTC)
	kv := &fakeKV{entry: fakeKVEntry{value: []byte("old"), created: savedAt, revision: 7}}

	got, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.NoError(t, err)
	assert.Equal(t, savedAt, got)
	assert.Equal(t, []kvWrite{{
		op: "update", key: "pubsub-replay._data_ChangeEvents", value: replayFromEarliest, revision: 7,
	}}, kv.writes)
}

func TestReplayStore_Reset_NewerCursorIsNotReplaced(t *testing.T) {
	kv := &fakeKV{entry: fakeKVEntry{value: []byte("newer"), created: time.Now()}}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.ErrorIs(t, err, port.ErrReplayCursorChanged)
	assert.Empty(t, kv.writes)
}

func TestReplayStore_Reset_RevisionConflictMeansCursorChanged(t *testing.T) {
	kv := &fakeKV{
		entry:    fakeKVEntry{value: []byte("old"), created: time.Now(), revision: 7},
		writeErr: wrongLastSequence(),
	}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.ErrorIs(t, err, port.ErrReplayCursorChanged)
}

func TestReplayStore_Reset_MissingCursorIsMarked(t *testing.T) {
	kv := &fakeKV{getErr: jetstream.ErrKeyNotFound}

	got, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.NoError(t, err)
	assert.True(t, got.IsZero())
	assert.Equal(t, []kvWrite{{
		op: "create", key: "pubsub-replay._data_ChangeEvents", value: replayFromEarliest,
	}}, kv.writes)
}

func TestReplayStore_Reset_MissingCursorSavedMeanwhileMeansCursorChanged(t *testing.T) {
	kv := &fakeKV{getErr: jetstream.ErrKeyNotFound, writeErr: wrongLastSequence()}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.ErrorIs(t, err, port.ErrReplayCursorChanged)
}

func TestReplayStore_Reset_ReadFailureReturnsError(t *testing.T) {
	kv := &fakeKV{getErr: errors.New("nats down")}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.Error(t, err)
	assert.NotErrorIs(t, err, port.ErrReplayCursorChanged)
	assert.Empty(t, kv.writes)
}

func TestReplayStore_Reset_WriteFailureReturnsError(t *testing.T) {
	kv := &fakeKV{
		entry:    fakeKVEntry{value: []byte("old"), created: time.Now()},
		writeErr: errors.New("nats down"),
	}

	_, err := NewReplayStore(kv).Reset(context.Background(), "/data/ChangeEvents", []byte("old"))

	require.Error(t, err)
	assert.NotErrorIs(t, err, port.ErrReplayCursorChanged)
}
