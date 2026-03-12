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
	"github.com/vmihailenco/msgpack/v5"
)

// resolveProject returns the v2 UID and v1 B2B project name and slug structured in projectInfo
// for the given Salesforce project SFID, consulting the in-memory cache first and falling back to:
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
	kvKey := fmt.Sprintf("salesforce_b2b-Project__c.%s", projectSFID)
	projData, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.InfoContext(ctx, "b2b resolvers: project__c record not found in v1-objects KV (project may not be in v2)",
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
		slog.InfoContext(ctx, "b2b resolvers: v2 project UID not found for SFID (project may not be in v2)",
			"project_sfid", projectSFID,
		)
		return nil, false
	}

	// Name and slug are v1 B2B values, but are expected to be safe to denormalize
	// in v2 context (expected to be equal!).
	info := projectInfo{
		uid:  projectUID,
		name: proj.Name,
		slug: proj.Slug,
	}

	c.projectCache.set(projectSFID, info)

	return &info, false
}

// resolveAccount fetches and decodes the salesforce_b2b-Account record for the given
// account SFID from the v1-objects KV bucket. When the record is not found in KV and
// a PostgreSQL fallback is configured, it falls back to a direct point-lookup query.
// Returns nil (non-retryable) when the record is not found anywhere, and (nil, true)
// on transient errors.
func (c *Consumer) resolveAccount(ctx context.Context, accountSFID string) (*SFAccount, bool) {
	if accountSFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-Account.%s", accountSFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// KV miss — try PostgreSQL fallback before giving up.
			if c.pgFallback != nil {
				account, pgErr := c.pgFallback.FetchAccount(ctx, accountSFID)
				if pgErr != nil {
					slog.ErrorContext(ctx, "b2b resolvers: pg fallback failed for account",
						"account_sfid", accountSFID,
						"error", pgErr,
					)
					return nil, true
				}
				if account != nil {
					slog.InfoContext(ctx, "b2b resolvers: resolved account via pg fallback (not yet in KV)",
						"account_sfid", accountSFID,
					)
					return account, false
				}
			}
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

// resolveProduct2 fetches and decodes the salesforce_b2b-Product2 record for the given
// SFID from the v1-objects KV bucket. When the record is not found in KV and a PostgreSQL
// fallback is configured, it falls back to a direct point-lookup query. Returns nil
// (non-retryable) when the record is not found anywhere, and (nil, true) on transient errors.
func (c *Consumer) resolveProduct2(ctx context.Context, product2SFID string) (*SFProduct2, bool) {
	if product2SFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-Product2.%s", product2SFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// KV miss — try PostgreSQL fallback before giving up.
			if c.pgFallback != nil {
				product, pgErr := c.pgFallback.FetchProduct2(ctx, product2SFID)
				if pgErr != nil {
					slog.ErrorContext(ctx, "b2b resolvers: pg fallback failed for product2",
						"product2_sfid", product2SFID,
						"error", pgErr,
					)
					return nil, true
				}
				if product != nil {
					slog.InfoContext(ctx, "b2b resolvers: resolved product2 via pg fallback (not yet in KV)",
						"product2_sfid", product2SFID,
					)
					return product, false
				}
			}
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

// resolveAsset fetches and decodes the salesforce_b2b-Asset record for the given SFID
// from the v1-objects KV bucket. No PostgreSQL fallback is used here because Asset is a
// trigger record (not a dependency). Returns nil (non-retryable) when the record is not
// found, and (nil, true) on transient errors.
func (c *Consumer) resolveAsset(ctx context.Context, assetSFID string) (*SFAsset, bool) {
	if assetSFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-Asset.%s", assetSFID)
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

// resolveContact fetches and decodes the salesforce_b2b-Contact record for the given
// SFID from the v1-objects KV bucket. When the record is not found in KV and a PostgreSQL
// fallback is configured, it falls back to a direct point-lookup query. Returns nil
// (non-retryable) when the record is not found anywhere, and (nil, true) on transient errors.
func (c *Consumer) resolveContact(ctx context.Context, contactSFID string) (*SFContact, bool) {
	if contactSFID == "" {
		return nil, false
	}

	kvKey := fmt.Sprintf("salesforce_b2b-Contact.%s", contactSFID)
	data, err := c.fetchKVRecord(ctx, kvKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// KV miss — try PostgreSQL fallback before giving up.
			if c.pgFallback != nil {
				contact, pgErr := c.pgFallback.FetchContact(ctx, contactSFID)
				if pgErr != nil {
					slog.ErrorContext(ctx, "b2b resolvers: pg fallback failed for contact",
						"contact_sfid", contactSFID,
						"error", pgErr,
					)
					return nil, true
				}
				if contact != nil {
					slog.InfoContext(ctx, "b2b resolvers: resolved contact via pg fallback (not yet in KV)",
						"contact_sfid", contactSFID,
					)
					return contact, false
				}
			}
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
// consulting the contact → emails forward-lookup index and then scanning the
// alternate_email__c entries stored in the v1-objects KV bucket. When the forward-lookup
// index is empty and a PostgreSQL fallback is configured, it falls back to a direct
// query against Alternate_Email__c.
//
// The lookup strategy mirrors the v1-sync-helper logic:
//  1. Retrieve the list of alternate_email SFIDs from the contact.emails index
//     (populated when alternate_email__c upserts are processed).
//  2. For each candidate, fetch the salesforce_b2b-Alternate_Email__c record and check
//     whether the record is active (Active__c == true), not soft-deleted (both SDC and
//     SFDC deleted flags), and whether Primary_Email__c is true.
//  3. If no primary email is found, fall back to the first active non-deleted email.
//  4. If the forward-lookup index has no entries and a PG fallback is available, query
//     PostgreSQL directly.
//
// Returns an empty string when no email is found or on any error (non-fatal).
func (c *Consumer) resolvePrimaryEmail(ctx context.Context, contactSFID string) string {
	// Retrieve candidate alternate_email SFIDs from the contact.emails forward-lookup index.
	emailSFIDs, err := c.mapping.getEmailsForContact(ctx, contactSFID)
	if err != nil {
		slog.DebugContext(ctx, "b2b resolvers: no emails index found for contact",
			"contact_sfid", contactSFID,
			"error", err,
		)
		// Fall through to PG fallback below.
		emailSFIDs = nil
	}

	if len(emailSFIDs) == 0 {
		// No KV email index entries — try PostgreSQL fallback.
		if c.pgFallback != nil {
			email, pgErr := c.pgFallback.FetchPrimaryEmail(ctx, contactSFID)
			if pgErr != nil {
				slog.WarnContext(ctx, "b2b resolvers: pg fallback failed for primary email",
					"contact_sfid", contactSFID,
					"error", pgErr,
				)
			} else if email != "" {
				slog.InfoContext(ctx, "b2b resolvers: resolved primary email via pg fallback (not yet in KV)",
					"contact_sfid", contactSFID,
				)
				return email
			}
		}
		return ""
	}

	// Single pass: look for primary email while tracking first valid fallback.
	var fallbackEmail string
	for _, emailSFID := range emailSFIDs {
		kvKey := fmt.Sprintf("salesforce_b2b-Alternate_Email__c.%s", emailSFID)
		data, fetchErr := c.fetchKVRecord(ctx, kvKey)
		if fetchErr != nil {
			slog.DebugContext(ctx, "b2b resolvers: failed to fetch alternate email record",
				"email_sfid", emailSFID,
				"error", fetchErr,
			)
			continue
		}

		var ae SFAlternateEmail
		if decodeErr := decodeTyped(data, &ae); decodeErr != nil {
			slog.DebugContext(ctx, "b2b resolvers: failed to decode alternate email record",
				"email_sfid", emailSFID,
				"error", decodeErr,
			)
			continue
		}

		// Skip inactive emails.
		if !ae.Active {
			slog.DebugContext(ctx, "b2b resolvers: skipping inactive email",
				"email_sfid", emailSFID,
			)
			continue
		}

		// Skip soft-deleted records (both SDC and SFDC deleted flags).
		if ae.IsDeleted || (ae.SDCDeletedAt != nil && *ae.SDCDeletedAt != "") {
			slog.DebugContext(ctx, "b2b resolvers: skipping deleted email record",
				"email_sfid", emailSFID,
			)
			continue
		}

		// Verify the contact association matches.
		if ae.ContactIDC != contactSFID {
			continue
		}

		if ae.AlternateEmailAddress == "" {
			continue
		}

		// Return immediately if this is the primary email.
		if ae.PrimaryEmail {
			return ae.AlternateEmailAddress
		}

		// Track the first valid email as a fallback.
		if fallbackEmail == "" {
			fallbackEmail = ae.AlternateEmailAddress
		}
	}

	// If no primary email found, return the first active email as fallback.
	if fallbackEmail != "" {
		slog.DebugContext(ctx, "b2b resolvers: using first active email as fallback (no primary found)",
			"contact_sfid", contactSFID,
			"email", fallbackEmail,
		)
	}

	return fallbackEmail
}

// fetchKVRecord retrieves and auto-decodes (JSON or msgpack) a record from the v1-objects
// KV bucket by key. The raw KV key (e.g. "salesforce_b2b-Asset.{sfid}") is used directly.
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

// decodePayloadMsgpack unmarshals msgpack bytes directly into dst using the
// vmihailenco/msgpack library.
func decodePayloadMsgpack(data []byte, dst any) error {
	return msgpack.Unmarshal(data, dst)
}

// lookupV2ProjectUID calls the v1-sync-helper NATS RPC endpoint to translate a Salesforce
// B2B project SFID to a v2 project UID. The mapping key format follows the v1-sync-helper
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
