// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package pubsub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

const (
	// replayKeyPrefix is the NATS KV key prefix for per-channel replay cursors.
	// Full key: "pubsub-replay.<channel>" where channel is the CDC topic name
	// (e.g. "/data/AccountChangeEvent"). Slashes in the channel name are replaced
	// with underscores to keep NATS key segments clean.
	replayKeyPrefix = "pubsub-replay."
)

// replayFromEarliest is stored in place of a cursor Salesforce rejected. It
// survives restarts, so streams keep requesting EARLIEST until the first Save
// of a real cursor overwrites it. Salesforce replay IDs are binary, so this
// text value cannot collide with one.
var replayFromEarliest = []byte("lfx:replay-from-earliest")

// Ensure ReplayStore satisfies port.ReplayStore at compile time.
var _ port.ReplayStore = (*ReplayStore)(nil)

// ReplayStore persists and retrieves the Pub/Sub replay cursor (opaque []byte)
// for a given CDC channel using the pubsub-state NATS KV bucket.
//
// Semantics: commit-after-process — the cursor is written only after the event
// has been fully handled by the orchestrator. A crash between receipt and commit
// causes the event to be redelivered on the next startup (at-least-once).
type ReplayStore struct {
	kv jetstream.KeyValue
}

// NewReplayStore returns a ReplayStore backed by the pubsub-state KV bucket.
// The bucket must already be initialized in the NATSClient (see
// NATSClient.KeyValueStore).
func NewReplayStore(kv jetstream.KeyValue) *ReplayStore {
	return &ReplayStore{kv: kv}
}

// Load returns the last committed replay cursor for channel, or nil if none
// has been persisted yet (first run → caller should use ReplayPreset_LATEST).
func (r *ReplayStore) Load(ctx context.Context, channel string) ([]byte, error) {
	entry, err := r.kv.Get(ctx, replayKey(channel))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.InfoContext(ctx, "pubsub: no replay cursor found, starting at LATEST",
				"channel", channel,
			)
			return nil, nil
		}
		return nil, fmt.Errorf("pubsub: load replay cursor for %q: %w", channel, err)
	}

	slog.DebugContext(ctx, "pubsub: loaded replay cursor",
		"channel", channel,
		"replay_id_len", len(entry.Value()),
	)
	return entry.Value(), nil
}

// Save persists replayID as the committed cursor for channel. Called by the
// orchestrator after each event is fully processed (commit-after-process).
func (r *ReplayStore) Save(ctx context.Context, channel string, replayID []byte) error {
	if len(replayID) == 0 {
		return nil
	}

	if _, err := r.kv.Put(ctx, replayKey(channel), replayID); err != nil {
		return fmt.Errorf("pubsub: save replay cursor for %q: %w", channel, err)
	}
	return nil
}

// Reset replaces the committed cursor for channel with replayFromEarliest and
// returns the time the cursor was last saved, read from the KV entry. The
// write is conditional on the revision read here (or on the key being absent),
// so a Save that lands in between wins and is reported as
// ErrReplayCursorChanged.
func (r *ReplayStore) Reset(ctx context.Context, channel string, rejected []byte) (time.Time, error) {
	key := replayKey(channel)
	entry, err := r.kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		_, err = r.kv.Create(ctx, key, replayFromEarliest)
		return time.Time{}, markReplayError(channel, err)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("pubsub: read replay cursor for %q: %w", channel, err)
	}
	if !bytes.Equal(entry.Value(), rejected) {
		return time.Time{}, port.ErrReplayCursorChanged
	}

	lastSavedAt := entry.Created()
	if _, err := r.kv.Update(ctx, key, replayFromEarliest, entry.Revision()); err != nil {
		return lastSavedAt, markReplayError(channel, err)
	}
	return lastSavedAt, nil
}

func markReplayError(channel string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		return port.ErrReplayCursorChanged
	}
	return fmt.Errorf("pubsub: mark replay cursor for %q: %w", channel, err)
}

// replayKey returns the NATS KV key for the given channel name.
// Slashes are replaced with underscores because NATS KV keys use dots as
// segment separators and slashes are not allowed.
func replayKey(channel string) string {
	return replayKeyPrefix + strings.ReplaceAll(channel, "/", "_")
}
