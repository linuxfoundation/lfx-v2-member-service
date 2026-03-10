// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vmihailenco/msgpack/v5"
)

// kvKeyPrefix is the prefix stripped from JetStream KV subjects to obtain the key.
// Derived from the V1ObjectsKVBucket constant to avoid static duplication.
var kvKeyPrefix = "$KV." + constants.V1ObjectsKVBucket + "."

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
		// Hard deletions have no payload; pass nil as old data.
		return c.handleDelete(ctx, prefix, sfid, nil)
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
// checks for soft-deletes (both _sdc_deleted_at and IsDeleted), and dispatches to the
// type-specific upsert or delete handler.
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

	// Check for soft deletes. _sdc_deleted_at indicates an upstream hard-deletion
	// propagated through Meltano/Singer. IsDeleted is the native Salesforce soft-delete
	// flag. Either condition triggers delete handling.
	if isSoftDeleted(decoded) {
		slog.InfoContext(ctx, "b2b consumer processing soft delete",
			"prefix", prefix,
			"sfid", sfid,
		)
		// Soft deletions pass through the decoded payload as "old data" so that delete
		// handlers can use field values (e.g. account ID) to clean up forward indexes.
		return c.handleDelete(ctx, prefix, sfid, decoded)
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
		return c.handleAlternateEmailUpsert(ctx, sfid, data)
	default:
		slog.WarnContext(ctx, "b2b consumer received unknown key prefix on upsert, skipping",
			"prefix", prefix,
			"sfid", sfid,
		)
		return false
	}
}

// handleDelete dispatches a delete event to the appropriate type handler.
// oldData contains the decoded record fields for soft deletions (allowing forward-index
// cleanup) and is nil for hard deletions (KV DEL/PURGE).
// Returns true if the message should be retried.
func (c *Consumer) handleDelete(ctx context.Context, prefix, sfid string, oldData map[string]any) bool {
	switch prefix {
	case "salesforce_b2b-product2":
		return c.handleProduct2Delete(ctx, sfid)
	case "salesforce_b2b-asset":
		return c.handleAssetDeleteWithCleanup(ctx, sfid, oldData)
	case "salesforce_b2b-project_role__c":
		return c.handleProjectRoleDeleteWithCleanup(ctx, sfid, oldData)
	case "salesforce_b2b-alternate_email__c":
		return c.handleAlternateEmailDelete(ctx, sfid, oldData)
	case "salesforce_b2b-account",
		"salesforce_b2b-contact",
		"salesforce_b2b-project__c":
		// Deletions of reference records (account, contact, project) are not cascaded
		// to the indexer — dependent documents remain until their own delete event
		// arrives. Log at debug level to avoid spurious warnings.
		slog.DebugContext(ctx, "b2b consumer skipping delete for reference record type",
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

// handleAlternateEmailUpsert maintains the contact → alternate_email forward-lookup
// index when an alternate_email__c record is created or updated. This index is required
// by resolvePrimaryEmail to find the primary email for a contact.
// Returns true if the message should be retried.
func (c *Consumer) handleAlternateEmailUpsert(ctx context.Context, sfid string, data map[string]any) bool {
	var ae SFAlternateEmail
	if err := decodeTyped(data, &ae); err != nil {
		slog.ErrorContext(ctx, "b2b handler: failed to decode Alternate_Email__c record",
			"sfid", sfid,
			"error", err,
		)
		return false
	}

	if ae.ContactIDC == "" {
		slog.WarnContext(ctx, "b2b handler: Alternate_Email__c has no Contact_ID__c, skipping",
			"sfid", sfid,
		)
		return false
	}

	if err := c.mapping.addEmailToContact(ctx, ae.ContactIDC, sfid); err != nil {
		slog.WarnContext(ctx, "b2b handler: failed to update contact→email mapping",
			"sfid", sfid,
			"contact_sfid", ae.ContactIDC,
			"error", err,
		)
	}

	// Also trigger re-indexing of any key_contact records linked to this contact, so
	// that email field changes are reflected in the indexed documents.
	c.triggerContactProjectRoleReindex(ctx, ae.ContactIDC)

	return false
}

// handleAlternateEmailDelete removes an alternate_email SFID from the contact → emails
// forward-lookup index and re-indexes affected key_contact records.
// Returns true if the message should be retried.
func (c *Consumer) handleAlternateEmailDelete(ctx context.Context, sfid string, oldData map[string]any) bool {
	// For soft deletes, we have the old data and can extract the contact SFID directly.
	contactSFID := ""
	if oldData != nil {
		var ae SFAlternateEmail
		if err := decodeTyped(oldData, &ae); err == nil {
			contactSFID = ae.ContactIDC
		}
	}

	if contactSFID == "" {
		// Hard deletion — no way to know which contact this email belonged to without
		// a reverse index. The forward index will contain a stale reference that will
		// be ignored on next read (the KV fetch will return key-not-found).
		slog.DebugContext(ctx, "b2b handler: alternate_email__c hard delete, cannot clean up forward index",
			"sfid", sfid,
		)
		return false
	}

	if err := c.mapping.removeEmailFromContact(ctx, contactSFID, sfid); err != nil {
		slog.WarnContext(ctx, "b2b handler: failed to remove email from contact→email mapping",
			"sfid", sfid,
			"contact_sfid", contactSFID,
			"error", err,
		)
	}

	// Re-index affected key_contact records so the email field is refreshed.
	c.triggerContactProjectRoleReindex(ctx, contactSFID)

	return false
}

// triggerContactProjectRoleReindex re-indexes all key_contact documents linked to the
// given contact SFID via the contact → project_roles forward-lookup index.
func (c *Consumer) triggerContactProjectRoleReindex(ctx context.Context, contactSFID string) {
	roleSFIDs, err := c.mapping.getProjectRolesForContact(ctx, contactSFID)
	if err != nil {
		slog.WarnContext(ctx, "b2b handler: failed to retrieve contact→project_role index for email re-index",
			"contact_sfid", contactSFID,
			"error", err,
		)
		return
	}

	for _, roleSFID := range roleSFIDs {
		roleData, fetchErr := c.fetchKVRecord(ctx, "salesforce_b2b-project_role__c."+roleSFID)
		if fetchErr != nil {
			slog.DebugContext(ctx, "b2b handler: stale project_role reference in contact index, skipping",
				"contact_sfid", contactSFID,
				"role_sfid", roleSFID,
				"error", fetchErr,
			)
			continue
		}

		// Skip soft-deleted project_role records.
		if isSoftDeleted(roleData) {
			slog.DebugContext(ctx, "b2b handler: skipping soft-deleted project_role during email re-index",
				"role_sfid", roleSFID,
			)
			continue
		}

		c.handleProjectRoleUpsert(ctx, roleSFID, roleData)
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

// isSoftDeleted returns true if the decoded record is logically deleted:
//   - _sdc_deleted_at is present and non-empty (Meltano/Singer soft-deletion,
//     indicating an upstream hard-deletion), OR
//   - IsDeleted is true (native Salesforce soft-delete flag).
func isSoftDeleted(data map[string]any) bool {
	// Check _sdc_deleted_at (Meltano/Singer deletion marker).
	if v, ok := data["_sdc_deleted_at"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr || s != "" {
			return true
		}
	}

	// Check IsDeleted (native Salesforce soft-delete flag).
	if v, ok := data["IsDeleted"]; ok {
		switch b := v.(type) {
		case bool:
			if b {
				return true
			}
		case string:
			if strings.EqualFold(b, "true") {
				return true
			}
		}
	}

	return false
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
