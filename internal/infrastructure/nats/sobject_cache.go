// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// SObjectCacheEntry is the JSON envelope stored in the member-service-cache KV
// bucket for each cached sObject record. It carries the raw HTTP conditional-GET
// metadata (ETag and Last-Modified) alongside the opaque JSON body returned by
// the Salesforce sObject REST API so that subsequent fetches can send
// If-None-Match / If-Modified-Since headers and avoid unnecessary data transfer.
type SObjectCacheEntry struct {
	// ETag is the value of the ETag response header from the last successful
	// 200 OK fetch. Empty when Salesforce did not return an ETag.
	ETag string `json:"etag,omitempty"`

	// LastModified is the RFC 1123 HTTP date used for If-Modified-Since on
	// subsequent fetches. It is populated from the Last-Modified response header
	// when Salesforce returns one, or derived from the SystemModstamp /
	// LastModifiedDate field in the response body when the header is absent
	// (common for sObject types such as Asset in some orgs). Sub-second
	// precision is truncated to the second (floor) before formatting.
	// Empty when no suitable timestamp was available.
	LastModified string `json:"last_modified,omitempty"`

	// Body is the raw JSON response body returned by the Salesforce sObject REST
	// API on the last successful 200 OK fetch.
	Body json.RawMessage `json:"body"`
}

// SObjectCache is a NATS KV-backed cache for Salesforce sObject records. Keys
// follow the pattern "{sobject_type}.{uid}" (e.g. "b2b_org_v2.{uid}",
// "project_membership.{uid}"). Values are JSON-encoded SObjectCacheEntry
// envelopes carrying HTTP ETag / Last-Modified metadata alongside the raw
// sObject body, enabling efficient conditional GET re-validation.
//
// Unlike Storage (which uses CachedValue soft-TTL envelopes), SObjectCache
// delegates freshness entirely to HTTP conditional GET semantics: the sObject
// client sends If-None-Match / If-Modified-Since on every re-fetch and updates
// the cache only on 200 OK. A 304 Not Modified response leaves the cache entry
// untouched and the caller serves the stored body as-is.
type SObjectCache struct {
	client *NATSClient
}

// NewSObjectCache creates an SObjectCache backed by the given NATSClient.
// The member-service-cache bucket must already be initialized on the client
// (via KeyValueStore) before this is called.
func NewSObjectCache(client *NATSClient) *SObjectCache {
	return &SObjectCache{client: client}
}

// Get retrieves the cached SObjectCacheEntry for the given key. The key should
// be pre-formed by the caller in the "{sobject_type}.{uid}" pattern.
//
// Returns (nil, nil) on a cache miss (key not found). Returns a non-nil error
// only for infrastructure failures — a miss is not an error.
func (c *SObjectCache) Get(ctx context.Context, key string) (*SObjectCacheEntry, error) {
	if key == "" {
		return nil, errs.NewValidation("sobject cache key cannot be empty")
	}

	kv, ok := c.client.kvStore[constants.KVBucketNameSObjectCache]
	if !ok {
		return nil, errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameSObjectCache))
	}

	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, errs.NewUnexpected(
			fmt.Sprintf("failed to get sObject cache entry for key %q from bucket %q", key, constants.KVBucketNameSObjectCache),
			err,
		)
	}

	var cached SObjectCacheEntry
	if unmarshalErr := json.Unmarshal(entry.Value(), &cached); unmarshalErr != nil {
		slog.WarnContext(ctx, "failed to unmarshal sObject cache entry; treating as miss",
			"bucket", constants.KVBucketNameSObjectCache,
			"key", key,
			"error", unmarshalErr,
		)
		// Best-effort delete so the corrupted entry is not repeatedly served as a
		// miss and does not generate log spam on every subsequent request.
		if delErr := kv.Delete(ctx, key); delErr != nil {
			slog.WarnContext(ctx, "failed to delete corrupted sObject cache entry after unmarshal failure",
				"bucket", constants.KVBucketNameSObjectCache,
				"key", key,
				"error", delErr,
			)
		}
		return nil, nil
	}

	return &cached, nil
}

// Put writes an SObjectCacheEntry to the member-service-cache bucket under the
// given key. The key should be pre-formed by the caller in the
// "{sobject_type}.{uid}" pattern. Overwrites any existing entry.
func (c *SObjectCache) Put(ctx context.Context, key string, entry *SObjectCacheEntry) error {
	if key == "" {
		return errs.NewValidation("sobject cache key cannot be empty")
	}
	if entry == nil {
		return errs.NewValidation("sobject cache entry cannot be nil")
	}

	kv, ok := c.client.kvStore[constants.KVBucketNameSObjectCache]
	if !ok {
		return errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameSObjectCache))
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return errs.NewUnexpected(
			fmt.Sprintf("failed to marshal sObject cache entry for key %q in bucket %q", key, constants.KVBucketNameSObjectCache),
			err,
		)
	}

	if _, err := kv.Put(ctx, key, data); err != nil {
		slog.WarnContext(ctx, "failed to write sObject cache entry to KV bucket",
			"bucket", constants.KVBucketNameSObjectCache,
			"key", key,
			"error", err,
		)
		return errs.NewUnexpected(
			fmt.Sprintf("failed to put sObject cache entry for key %q in bucket %q", key, constants.KVBucketNameSObjectCache),
			err,
		)
	}

	return nil
}

// Delete removes the cached SObjectCacheEntry for the given key. Returns nil if
// the key does not exist — a missing entry is already invalidated.
func (c *SObjectCache) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errs.NewValidation("sobject cache key cannot be empty")
	}

	kv, ok := c.client.kvStore[constants.KVBucketNameSObjectCache]
	if !ok {
		return errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameSObjectCache))
	}

	if err := kv.Delete(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return errs.NewUnexpected(
			fmt.Sprintf("failed to delete sObject cache entry for key %q from bucket %q", key, constants.KVBucketNameSObjectCache),
			err,
		)
	}

	return nil
}
