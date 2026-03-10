// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vmihailenco/msgpack/v5"
)

// kvKeyPrefix is the prefix stripped from JetStream KV subjects to obtain the key.
const kvKeyPrefix = "$KV.v1-objects."

// handleKVMessage is the jetstream.MessageHandler registered with the b2b pull consumer.
// It parses the KV entry from the raw JetStream message, dispatches to the appropriate
// type handler, and ACKs or NAKs the message with exponential backoff.
func (c *Consumer) handleKVMessage(msg jetstream.Msg) {
	subject := msg.Subject()

	// Extract the KV key from the subject ($KV.v1-objects.{key}).
	key := ""
	if strings.HasPrefix(subject, kvKeyPrefix) {
		key = subject[len(kvKeyPrefix):]
	}

	if key == "" {
		slog.Warn("b2b consumer received message with unparseable subject, ACKing to skip",
			"subject", subject,
		)
		_ = msg.Ack()
		return
	}

	// Determine KV operation from the KV-Operation header.
	operation := jetstream.KeyValuePut
	if opHeader := msg.Headers().Get("KV-Operation"); opHeader != "" {
		switch opHeader {
		case "DEL":
			operation = jetstream.KeyValueDelete
		case "PURGE":
			operation = jetstream.KeyValuePurge
		}
	}

	ctx := context.Background()

	slog.DebugContext(ctx, "b2b consumer processing KV entry",
		"key", key,
		"operation", operation.String(),
	)

	shouldRetry := c.dispatchKVEntry(ctx, key, operation, msg.Data())

	if shouldRetry {
		metadata, err := msg.Metadata()
		if err != nil {
			slog.WarnContext(ctx, "b2b consumer failed to get message metadata, using default NAK delay",
				"key", key,
				"error", err,
			)
			metadata = &jetstream.MsgMetadata{NumDelivered: 1}
		}

		// Exponential backoff: 2s → 10s → 20s (matches v1-sync-helper pattern).
		var delay time.Duration
		switch metadata.NumDelivered {
		case 1:
			delay = 2 * time.Second
		case 2:
			delay = 10 * time.Second
		default:
			delay = 20 * time.Second
		}

		if err := msg.NakWithDelay(delay); err != nil {
			slog.ErrorContext(ctx, "b2b consumer failed to NAK message for retry",
				"key", key,
				"attempt", metadata.NumDelivered,
				"error", err,
			)
		} else {
			slog.DebugContext(ctx, "b2b consumer NAKed message for retry",
				"key", key,
				"attempt", metadata.NumDelivered,
				"delay_seconds", delay.Seconds(),
			)
		}
		return
	}

	if err := msg.Ack(); err != nil {
		slog.ErrorContext(ctx, "b2b consumer failed to ACK message",
			"key", key,
			"error", err,
		)
	}
}

// dispatchKVEntry routes the KV entry to the appropriate handler based on the key prefix
// and the operation type. Returns true if the message should be retried (NAK).
func (c *Consumer) dispatchKVEntry(ctx context.Context, key string, operation jetstream.KeyValueOp, data []byte) bool {
	// Determine the key prefix (everything before the first dot).
	prefix := key
	if idx := strings.Index(key, "."); idx != -1 {
		prefix = key[:idx]
	}

	// Extract the SFID (everything after the first dot).
	sfid := ""
	if idx := strings.Index(key, "."); idx != -1 && idx < len(key)-1 {
		sfid = key[idx+1:]
	}

	if sfid == "" {
		slog.WarnContext(ctx, "b2b consumer cannot extract SFID from key, skipping",
			"key", key,
		)
		return false
	}

	switch operation {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		return c.handleHardDelete(ctx, prefix, sfid)
	case jetstream.KeyValuePut:
		return c.handlePut(ctx, prefix, sfid, data)
	default:
		slog.DebugContext(ctx, "b2b consumer ignoring unknown KV operation",
			"key", key,
			"operation", operation.String(),
		)
		return false
	}
}

// handlePut processes a KV PUT operation. It decodes the payload (JSON or msgpack),
// checks for soft-deletes, and dispatches to the type-specific upsert or delete handler.
// Returns true if the message should be retried.
func (c *Consumer) handlePut(ctx context.Context, prefix, sfid string, data []byte) bool {
	// Decode the payload; try JSON first, fall back to msgpack.
	decoded, err := decodePayload(data)
	if err != nil {
		slog.ErrorContext(ctx, "b2b consumer failed to decode KV payload",
			"prefix", prefix,
			"sfid", sfid,
			"error", err,
		)
		// Non-retryable: malformed payload will not improve on retry.
		return false
	}

	// Check for a soft delete (_sdc_deleted_at present and non-empty).
	if isSoftDeleted(decoded) {
		slog.InfoContext(ctx, "b2b consumer processing soft delete",
			"prefix", prefix,
			"sfid", sfid,
		)
		return c.handleSoftDelete(ctx, prefix, sfid, decoded)
	}

	return c.handleUpsert(ctx, prefix, sfid, decoded)
}

// handleUpsert dispatches a decoded upsert payload to the appropriate type handler.
// Returns true if the message should be retried.
func (c *Consumer) handleUpsert(ctx context.Context, prefix, sfid string, data map[string]any) bool {
	switch prefix {
	case "salesforce_b2b-product2":
		return c.handleProduct2Upsert(ctx, sfid, data)
	case "salesforce_b2b-asset":
		return c.handleAssetUpsert(ctx, sfid, data)
	case "salesforce_b2b-project_role__c":
		return c.handleProjectRoleUpsert(ctx, sfid, data)
	case "salesforce_b2b-account":
		return c.handleAccountUpdate(ctx, sfid, data)
	case "salesforce_b2b-contact":
		return c.handleContactUpdate(ctx, sfid, data)
	case "salesforce_b2b-project__c":
		return c.handleProjectUpdate(ctx, sfid, data)
	case "salesforce_b2b-alternate_email__c":
		// Alternate email records are looked up on-demand via the v1-objects KV; no
		// additional indexer action is needed on upsert beyond the KV write itself.
		slog.DebugContext(ctx, "b2b consumer skipping alternate_email__c upsert (read on-demand)",
			"sfid", sfid,
		)
		return false
	default:
		slog.WarnContext(ctx, "b2b consumer received unknown key prefix on upsert, skipping",
			"prefix", prefix,
			"sfid", sfid,
		)
		return false
	}
}

// handleSoftDelete dispatches a soft-delete to the appropriate type handler.
// Returns true if the message should be retried.
func (c *Consumer) handleSoftDelete(ctx context.Context, prefix, sfid string, _ map[string]any) bool {
	return c.handleHardDelete(ctx, prefix, sfid)
}

// handleHardDelete dispatches a hard (KV DEL/PURGE) or soft delete to the
// appropriate type handler. Returns true if the message should be retried.
func (c *Consumer) handleHardDelete(ctx context.Context, prefix, sfid string) bool {
	switch prefix {
	case "salesforce_b2b-product2":
		return c.handleProduct2Delete(ctx, sfid)
	case "salesforce_b2b-asset":
		return c.handleAssetDelete(ctx, sfid)
	case "salesforce_b2b-project_role__c":
		return c.handleProjectRoleDelete(ctx, sfid)
	case "salesforce_b2b-account",
		"salesforce_b2b-contact",
		"salesforce_b2b-project__c",
		"salesforce_b2b-alternate_email__c":
		// Deletions of reference records (account, contact, project, alternate_email) are
		// not cascaded to the indexer — dependent documents remain until their own delete
		// event arrives. Log at debug level to avoid spurious warnings.
		slog.DebugContext(ctx, "b2b consumer skipping hard delete for reference record type",
			"prefix", prefix,
			"sfid", sfid,
		)
		return false
	default:
		slog.WarnContext(ctx, "b2b consumer received unknown key prefix on delete, skipping",
			"prefix", prefix,
			"sfid", sfid,
		)
		return false
	}
}

// ---- Decoding helpers ----

// decodePayload attempts to unmarshal data as JSON first; if that fails it tries msgpack.
// This matches the auto-detect pattern used throughout the v1-sync-helper codebase.
func decodePayload(data []byte) (map[string]any, error) {
	var result map[string]any

	if err := json.Unmarshal(data, &result); err == nil {
		return result, nil
	}

	if err := msgpack.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// isSoftDeleted returns true if the decoded record contains a non-nil, non-empty
// _sdc_deleted_at field, indicating a WAL-generated soft delete.
func isSoftDeleted(data map[string]any) bool {
	v, ok := data["_sdc_deleted_at"]
	if !ok || v == nil {
		return false
	}
	s, isStr := v.(string)
	return !isStr || s != ""
}

// decodeTyped re-marshals a generic decoded map into the given typed struct pointer using
// JSON as the intermediate representation. This avoids duplicating field-mapping logic.
func decodeTyped(data map[string]any, dst any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
