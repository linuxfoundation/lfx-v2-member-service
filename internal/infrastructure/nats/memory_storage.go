// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// memoryKVEntry implements jetstream.KeyValueEntry over an in-memory message.
type memoryKVEntry struct {
	key   string
	value []byte
	rev   uint64
	op    jetstream.KeyValueOp
}

func (e memoryKVEntry) Bucket() string                  { return constants.KVBucketNameCache }
func (e memoryKVEntry) Key() string                     { return e.key }
func (e memoryKVEntry) Value() []byte                   { return e.value }
func (e memoryKVEntry) Revision() uint64                { return e.rev }
func (e memoryKVEntry) Created() time.Time              { return time.Time{} }
func (e memoryKVEntry) Delta() uint64                   { return 0 }
func (e memoryKVEntry) Operation() jetstream.KeyValueOp { return e.op }

// memoryKV is an in-memory jetstream.KeyValue exposing the same CAS
// semantics (revision-conditioned Update, delete markers surfaced via
// ErrKeyNotFound on Get) that Storage's write-back logic depends on.
// Unimplemented methods panic via the embedded nil interface.
type memoryKV struct {
	jetstream.KeyValue

	mu   sync.Mutex
	seq  uint64
	msgs map[string][]memoryKVEntry
}

func (f *memoryKV) last(key string) (memoryKVEntry, bool) {
	entries := f.msgs[key]
	if len(entries) == 0 {
		return memoryKVEntry{}, false
	}
	return entries[len(entries)-1], true
}

func (f *memoryKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, ok := f.last(key)
	if !ok || last.op != jetstream.KeyValuePut {
		return nil, jetstream.ErrKeyNotFound
	}
	return last, nil
}

func (f *memoryKV) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lastSeq uint64
	if last, ok := f.last(key); ok {
		lastSeq = last.rev
	}
	if lastSeq != revision {
		return 0, jetstream.ErrKeyExists
	}
	f.seq++
	f.msgs[key] = append(f.msgs[key], memoryKVEntry{key: key, value: value, rev: f.seq, op: jetstream.KeyValuePut})
	return f.seq, nil
}

func (f *memoryKV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.msgs[key] = append(f.msgs[key], memoryKVEntry{key: key, rev: f.seq, op: jetstream.KeyValueDelete})
	return nil
}

func (f *memoryKV) History(_ context.Context, key string, _ ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
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

// NewMemoryStorage returns a Storage backed by an in-memory KV store using
// DefaultTTLConfig, for tests in other packages that exercise Storage's cache
// semantics (revision-conditioned CAS, tombstones) without a running NATS
// server.
func NewMemoryStorage() *Storage {
	return NewMemoryStorageWithTTL(DefaultTTLConfig)
}

// NewMemoryStorageWithTTL is NewMemoryStorage with an overridden TTLConfig,
// letting tests force a Fresh/Stale/Expired classification deterministically
// (e.g. a negative StaleDuration makes every write immediately stale).
func NewMemoryStorageWithTTL(ttl TTLConfig) *Storage {
	kv := &memoryKV{msgs: map[string][]memoryKVEntry{}}
	return &Storage{
		client:    &NATSClient{kvStore: map[string]jetstream.KeyValue{constants.KVBucketNameCache: kv}},
		ttlConfig: ttl,
	}
}
