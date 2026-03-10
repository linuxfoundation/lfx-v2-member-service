// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// resolveProject returns the projectInfo for the given Salesforce project SFID,
// consulting the in-memory cache first and falling back to:
//  1. The salesforce_b2b-project__c record in the v1-objects KV bucket (for name/slug).
//  2. A NATS RPC call to v1-sync-helper (lfx.lookup_v1_mapping) to translate the
//     project SFID to a v2 project UID.
//
// Returns (nil, false) when the project is not found (non-retryable skip).
// Returns (nil, true) when a transient error occurred and the caller should retry.
func (c *Consumer) resolveProject(ctx context.Context, projectSFID string) (*projectInfo, bool) {
	if projectSFID == "" {
		return nil, false
	}

	// Check the in-memory cache first.
	if cached, ok := c.projectCache.get(projectSFID); ok {
		return &cached, false
	}

	// Fetch the Project__c record from the v1-objects KV bucket for name/slug.
	kvKey := fmt.Sprintf("salesforce_b2b-project__c.%s", projectSFID)
	projData, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "b2b resolvers: project__c record not found in v1-objects KV",
				"project_sfid", projectSFID,
			)
			return nil, false
		}
		slog.ErrorContext(ctx, "b2b resolvers: failed to fetch project__c record from KV",
			"project_sfid", projectSFID,
			"error", err,
		)
		return nil, true
	}

	var proj SFProject
	if err := decodeTyped(projData, &proj); err != nil {
		slog.ErrorContext(ctx, "b2b resolvers: failed to decode project__c record",
			"project_sfid", projectSFID,
			"error", err,
		)
		return nil, false
	}

	// Translate the Salesforce SFID to a v2 project UID via NATS RPC lookup.
	projectUID, retry := c.lookupV2ProjectUID(ctx, projectSFID)
	if retry {
		return nil, true
	}
	if projectUID == "" {
		slog.WarnContext(ctx, "b2b resolvers: v2 project UID not found for SFID",
			"project_sfid", projectSFID,
		)
		return nil, false
	}

	info := projectInfo{
		uid:  projectUID,
		name: proj.Name,
		slug: proj.Slug,
	}

	c.projectCache.set(projectSFID, info)

	return &info, false
}

// resolveAccount fetches and decodes the salesforce_b2b-account record for the given
// account SFID from the v1-objects KV bucket. Returns nil (non-retryable) when the
// record is not found, and (nil, true) on transient errors.
func (c *Consumer) resolveAccount(ctx context.Context, accountSFID string) (*SFAccount, bool) {
	if accountSFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-account.%s", accountSFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "b2b resolvers: account record not found in v1-objects KV",
				"account_sfid", accountSFID,
			)
			return nil, false
		}
		slog.ErrorContext(ctx, "b2b resolvers: failed to fetch account record from KV",
			"account_sfid", accountSFID,
			"error", err,
		)
		return nil, true
	}

	var account SFAccount
	if err := decodeTyped(data, &account); err != nil {
		slog.ErrorContext(ctx, "b2b resolvers: failed to decode account record",
			"account_sfid", accountSFID,
			"error", err,
		)
		return nil, false
	}

	return &account, false
}

// resolveProduct2 fetches and decodes the salesforce_b2b-product2 record for the given
// SFID from the v1-objects KV bucket. Returns nil (non-retryable) when the record is not
// found, and (nil, true) on transient errors.
func (c *Consumer) resolveProduct2(ctx context.Context, product2SFID string) (*SFProduct2, bool) {
	if product2SFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-product2.%s", product2SFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "b2b resolvers: product2 record not found in v1-objects KV",
				"product2_sfid", product2SFID,
			)
			return nil, false
		}
		slog.ErrorContext(ctx, "b2b resolvers: failed to fetch product2 record from KV",
			"product2_sfid", product2SFID,
			"error", err,
		)
		return nil, true
	}

	var product SFProduct2
	if err := decodeTyped(data, &product); err != nil {
		slog.ErrorContext(ctx, "b2b resolvers: failed to decode product2 record",
			"product2_sfid", product2SFID,
			"error", err,
		)
		return nil, false
	}

	return &product, false
}

// resolveAsset fetches and decodes the salesforce_b2b-asset record for the given
// SFID from the v1-objects KV bucket. Returns nil (non-retryable) when the record is not
// found, and (nil, true) on transient errors.
func (c *Consumer) resolveAsset(ctx context.Context, assetSFID string) (*SFAsset, bool) {
	if assetSFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-asset.%s", assetSFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "b2b resolvers: asset record not found in v1-objects KV",
				"asset_sfid", assetSFID,
			)
			return nil, false
		}
		slog.ErrorContext(ctx, "b2b resolvers: failed to fetch asset record from KV",
			"asset_sfid", assetSFID,
			"error", err,
		)
		return nil, true
	}

	var asset SFAsset
	if err := decodeTyped(data, &asset); err != nil {
		slog.ErrorContext(ctx, "b2b resolvers: failed to decode asset record",
			"asset_sfid", assetSFID,
			"error", err,
		)
		return nil, false
	}

	return &asset, false
}

// resolveContact fetches and decodes the salesforce_b2b-contact record for the given
// SFID from the v1-objects KV bucket. Returns nil (non-retryable) when the record is not
// found, and (nil, true) on transient errors.
func (c *Consumer) resolveContact(ctx context.Context, contactSFID string) (*SFContact, bool) {
	if contactSFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-contact.%s", contactSFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "b2b resolvers: contact record not found in v1-objects KV",
				"contact_sfid", contactSFID,
			)
			return nil, false
		}
		slog.ErrorContext(ctx, "b2b resolvers: failed to fetch contact record from KV",
			"contact_sfid", contactSFID,
			"error", err,
		)
		return nil, true
	}

	var contact SFContact
	if err := decodeTyped(data, &contact); err != nil {
		slog.ErrorContext(ctx, "b2b resolvers: failed to decode contact record",
			"contact_sfid", contactSFID,
			"error", err,
		)
		return nil, false
	}

	return &contact, false
}

// resolvePrimaryEmail finds the primary email address for the given contact SFID by
// consulting the contact → project-roles forward-lookup index and then scanning the
// alternate_email__c entries stored in the v1-objects KV bucket.
//
// The lookup strategy:
//  1. Retrieve the list of alternate_email SFID candidates from the
//     contact.project-roles index (populated when alternate email upserts are processed).
//     NOTE: At this time we use a KV prefix scan as a fallback since the alternate_email
//     index is not separately maintained; this is acceptable given the expected data volume.
//  2. For each candidate, fetch the salesforce_b2b-alternate_email__c record and check
//     whether Primary_Email__c is true and Contact_ID__c matches.
//
// Returns an empty string when no primary email is found or on any error (non-fatal).
func (c *Consumer) resolvePrimaryEmail(ctx context.Context, contactSFID string) string {
	// Retrieve candidate alternate_email SFIDs from the forward-lookup index.
	emailSFIDs, err := c.mapping.getProjectRolesForContact(ctx, contactSFID)
	if err != nil {
		slog.WarnContext(ctx, "b2b resolvers: failed to retrieve contact→project_role index for email resolution",
			"contact_sfid", contactSFID,
			"error", err,
		)
		// Fall through to KV scan.
	}

	// Attempt to find the primary email by scanning alternate_email entries in the
	// contact's email index (stored under a separate index key in the mapping bucket).
	email := c.findPrimaryEmailFromIndex(ctx, contactSFID)
	if email != "" {
		return email
	}

	// As a secondary check, attempt to resolve via any known alternate_email SFIDs
	// that happen to be co-listed in the emailSFIDs list (defensive).
	for _, sfid := range emailSFIDs {
		kvKey := fmt.Sprintf("salesforce_b2b-alternate_email__c.%s", sfid)
		data, fetchErr := c.fetchKVRecord(ctx, kvKey)
		if fetchErr != nil {
			continue
		}
		var ae SFAlternateEmail
		if decodeErr := decodeTyped(data, &ae); decodeErr != nil {
			continue
		}
		if ae.ContactIDC == contactSFID && ae.PrimaryEmail && !isSoftDeletedRecord(ae.SDCDeletedAt) {
			return ae.AlternateEmailAddress
		}
	}

	return ""
}

// findPrimaryEmailFromIndex looks up the primary email for a contact from the dedicated
// alternate-email forward-lookup index stored in the mapping KV bucket under the key
// "contact.emails.{contactSfid}". Returns empty string when not found.
func (c *Consumer) findPrimaryEmailFromIndex(ctx context.Context, contactSFID string) string {
	indexKey := fmt.Sprintf("contact.emails.%s", contactSFID)
	emailSFIDs, err := c.mapping.listValues(ctx, indexKey)
	if err != nil || len(emailSFIDs) == 0 {
		return ""
	}

	for _, emailSFID := range emailSFIDs {
		kvKey := fmt.Sprintf("salesforce_b2b-alternate_email__c.%s", emailSFID)
		data, fetchErr := c.fetchKVRecord(ctx, kvKey)
		if fetchErr != nil {
			continue
		}
		var ae SFAlternateEmail
		if decodeErr := decodeTyped(data, &ae); decodeErr != nil {
			continue
		}
		if ae.PrimaryEmail && !isSoftDeletedRecord(ae.SDCDeletedAt) {
			return ae.AlternateEmailAddress
		}
	}

	return ""
}

// fetchKVRecord retrieves and auto-decodes (JSON or msgpack) a record from the v1-objects
// KV bucket by key. The raw KV key (e.g. "salesforce_b2b-asset.{sfid}") is used directly.
// Returns a map[string]any suitable for decodeTyped, or an error.
func (c *Consumer) fetchKVRecord(ctx context.Context, key string) (map[string]any, error) {
	entry, err := c.v1ObjectsKV.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	data := entry.Value()

	var result map[string]any
	if jsonErr := json.Unmarshal(data, &result); jsonErr == nil {
		return result, nil
	}

	// JSON failed; try msgpack.
	if mpErr := decodePayloadMsgpack(data, &result); mpErr != nil {
		return nil, fmt.Errorf("fetchKVRecord: could not decode %q as JSON or msgpack", key)
	}

	return result, nil
}

// decodePayloadMsgpack is a small wrapper to unmarshal msgpack bytes into dst using the
// vmihailenco/msgpack library. Kept here so handler.go's decodePayload stays standalone.
func decodePayloadMsgpack(data []byte, dst any) error {
	// Import is handled via the msgpack import in handler.go; here we re-use json as an
	// intermediate step via a direct call to avoid a circular helper dependency.
	// In practice the vmihailenco/msgpack library is already in scope via handler.go in
	// the same package — we call it through the package-level decodePayload helper.
	decoded, err := decodePayload(data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// lookupV2ProjectUID calls the v1-sync-helper NATS RPC endpoint to translate a Salesforce
// project SFID to a v2 project UID. The mapping key format follows the v1-sync-helper
// convention: "salesforce-project__c.{sfid}" → v2 UID.
//
// Returns ("", false) when the mapping does not exist (non-retryable skip).
// Returns ("", true) on transient NATS errors (caller should retry).
func (c *Consumer) lookupV2ProjectUID(ctx context.Context, projectSFID string) (string, bool) {
	mappingKey := fmt.Sprintf("salesforce-project__c.%s", projectSFID)

	// Set a reasonable per-call timeout to avoid blocking the consumer.
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	msg, err := c.natsConn.RequestWithContext(reqCtx, c.lookupSubject, []byte(mappingKey))
	if err != nil {
		slog.WarnContext(ctx, "b2b resolvers: NATS RPC lookup for project UID failed",
			"project_sfid", projectSFID,
			"mapping_key", mappingKey,
			"error", err,
		)
		// Treat as transient — the v1-sync-helper may not yet have processed the project.
		return "", true
	}

	response := strings.TrimSpace(string(msg.Data))

	if response == "" {
		// Mapping not found.
		return "", false
	}

	if strings.HasPrefix(response, "error: ") {
		slog.WarnContext(ctx, "b2b resolvers: NATS RPC lookup returned error for project UID",
			"project_sfid", projectSFID,
			"response", response,
		)
		// The lookup service returned an error; treat as transient.
		return "", true
	}

	return response, false
}

// isSoftDeletedRecord returns true when the _sdc_deleted_at pointer is non-nil and
// points to a non-empty string. Used when working with already-decoded typed structs.
func isSoftDeletedRecord(deletedAt *string) bool {
	return deletedAt != nil && *deletedAt != ""
}
