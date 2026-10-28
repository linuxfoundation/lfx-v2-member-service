// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"errors"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// CDCSubscriber is the hexagonal seam between the gRPC+Avro adapter and the
// CDC consumer orchestrator. The adapter
// (internal/infrastructure/salesforce/pubsub) implements it; goavro/gRPC/proto
// types never cross this boundary.
type CDCSubscriber interface {
	// Subscribe streams normalized CDCEvents for a CDC channel starting at the
	// given replay cursor (nil = LATEST). On reconnects the adapter reloads the
	// cursor from replayStore so events delivered but not yet persisted are
	// re-delivered rather than silently skipped.
	// The returned channel is closed when ctx is cancelled or the stream ends
	// unrecoverably.
	Subscribe(ctx context.Context, channel string, replayID []byte, replayStore ReplayStore) (<-chan model.CDCEvent, error)
}

// ReplayStore is the hexagonal seam for persisting the Pub/Sub replay cursor.
// The infrastructure adapter (pubsub.ReplayStore) implements it using the
// pubsub-state NATS KV bucket.
type ReplayStore interface {
	// Load returns the last committed replay cursor for channel, or nil on first
	// run (caller should use ReplayPreset_LATEST).
	Load(ctx context.Context, channel string) ([]byte, error)

	// Save persists replayID as the committed cursor for channel. Called after
	// each event is fully processed (commit-after-process semantics).
	Save(ctx context.Context, channel string, replayID []byte) error

	// Reset deletes the committed cursor for channel so the next Load returns
	// nil (LATEST), and returns when that cursor was last saved. Used when
	// Salesforce rejects the cursor as outside its retention window. It deletes
	// only if the stored cursor is still rejected; when the consumer has saved
	// a newer one since, it returns ErrReplayCursorChanged and deletes nothing.
	// A missing cursor returns the zero time and a nil error.
	Reset(ctx context.Context, channel string, rejected []byte) (lastSavedAt time.Time, err error)
}

// ErrReplayCursorChanged reports that the stored cursor no longer matches the
// one Salesforce rejected, so it may still be valid and must not be deleted.
var ErrReplayCursorChanged = errors.New("replay cursor changed since it was rejected")
