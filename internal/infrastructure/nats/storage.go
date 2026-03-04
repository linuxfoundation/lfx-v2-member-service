// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"

	"github.com/nats-io/nats.go/jetstream"
)

type storage struct {
	client *NATSClient
}

// get retrieves a model from the NATS KV store by bucket and UID.
func (s *storage) get(ctx context.Context, bucket, uid string, model any) (uint64, error) {
	if uid == "" {
		return 0, errs.NewValidation("UID cannot be empty")
	}

	data, errGet := s.client.kvStore[bucket].Get(ctx, uid)
	if errGet != nil {
		return 0, errGet
	}

	errUnmarshal := json.Unmarshal(data.Value(), model)
	if errUnmarshal != nil {
		return 0, errUnmarshal
	}

	return data.Revision(), nil
}

// ================== MembershipReader implementation ==================

// GetMembership retrieves a membership by UID
func (s *storage) GetMembership(ctx context.Context, uid string) (*model.Membership, uint64, error) {
	membership := &model.Membership{}

	rev, errGet := s.get(ctx, constants.KVBucketNameMemberships, uid, membership)
	if errGet != nil {
		if errors.Is(errGet, jetstream.ErrKeyNotFound) {
			return nil, 0, errs.NewNotFound("membership not found", fmt.Errorf("membership UID: %s", uid))
		}
		return nil, 0, errs.NewUnexpected("failed to get membership", errGet)
	}

	return membership, rev, nil
}

// ListMemberships retrieves memberships with pagination and filtering
func (s *storage) ListMemberships(ctx context.Context, params model.ListParams) ([]*model.Membership, int, error) {
	slog.DebugContext(ctx, "listing memberships from NATS storage",
		"page_size", params.PageSize,
		"offset", params.Offset,
	)

	kv := s.client.kvStore[constants.KVBucketNameMemberships]

	// If filtering by project_id, use the lookup index for fast retrieval
	if projectID, ok := params.Filters["project_id"]; ok {
		// Build remaining filters (excluding project_id which is handled by lookup)
		remainingFilters := make(map[string]string)
		for k, v := range params.Filters {
			if k != "project_id" {
				remainingFilters[k] = v
			}
		}
		return s.listMembershipsByLookup(ctx, kv, fmt.Sprintf("lookup/project/%s/", projectID), params, remainingFilters)
	}

	hasFilters := len(params.Filters) > 0

	keys, errKeys := kv.ListKeys(ctx)
	if errKeys != nil {
		if errors.Is(errKeys, jetstream.ErrNoKeysFound) {
			return []*model.Membership{}, 0, nil
		}
		return nil, 0, errs.NewUnexpected("failed to list keys from memberships bucket", errKeys)
	}

	// When no filters, paginate at the key level to avoid loading all records
	if !hasFilters {
		var allKeys []string
		for key := range keys.Keys() {
			if strings.HasPrefix(key, "lookup/") {
				continue
			}
			allKeys = append(allKeys, key)
		}

		totalSize := len(allKeys)

		start := params.Offset
		if start > totalSize {
			start = totalSize
		}
		end := start + params.PageSize
		if end > totalSize {
			end = totalSize
		}

		pageKeys := allKeys[start:end]
		memberships := make([]*model.Membership, 0, len(pageKeys))
		for _, key := range pageKeys {
			membership := &model.Membership{}
			_, errGet := s.get(ctx, constants.KVBucketNameMemberships, key, membership)
			if errGet != nil {
				slog.WarnContext(ctx, "failed to get membership while listing",
					"key", key,
					"error", errGet,
				)
				continue
			}
			memberships = append(memberships, membership)
		}

		slog.DebugContext(ctx, "retrieved memberships from NATS storage",
			"total_size", totalSize,
			"page_size", params.PageSize,
			"offset", params.Offset,
			"returned", len(memberships),
		)

		return memberships, totalSize, nil
	}

	// With other filters, we must load and check each record
	var filtered []*model.Membership
	for key := range keys.Keys() {
		if strings.HasPrefix(key, "lookup/") {
			continue
		}

		membership := &model.Membership{}
		_, errGet := s.get(ctx, constants.KVBucketNameMemberships, key, membership)
		if errGet != nil {
			slog.WarnContext(ctx, "failed to get membership while listing",
				"key", key,
				"error", errGet,
			)
			continue
		}

		if !matchesFilters(membership, params.Filters) {
			continue
		}

		filtered = append(filtered, membership)
	}

	totalSize := len(filtered)

	start := params.Offset
	if start > totalSize {
		start = totalSize
	}
	end := start + params.PageSize
	if end > totalSize {
		end = totalSize
	}

	slog.DebugContext(ctx, "retrieved memberships from NATS storage",
		"total_size", totalSize,
		"page_size", params.PageSize,
		"offset", params.Offset,
		"returned", end-start,
	)

	return filtered[start:end], totalSize, nil
}

// listMembershipsByLookup uses a lookup prefix to efficiently find memberships.
// remainingFilters are applied after fetching records from the lookup index.
func (s *storage) listMembershipsByLookup(ctx context.Context, kv jetstream.KeyValue, lookupPrefix string, params model.ListParams, remainingFilters map[string]string) ([]*model.Membership, int, error) {
	keys, errKeys := kv.ListKeys(ctx, jetstream.MetaOnly())
	if errKeys != nil {
		if errors.Is(errKeys, jetstream.ErrNoKeysFound) {
			return []*model.Membership{}, 0, nil
		}
		return nil, 0, errs.NewUnexpected("failed to list keys from memberships bucket", errKeys)
	}

	// Collect membership UIDs from lookup keys matching the prefix
	var membershipUIDs []string
	for key := range keys.Keys() {
		if strings.HasPrefix(key, lookupPrefix) {
			// Extract membership UID from the lookup key (last segment)
			parts := strings.Split(key, "/")
			if len(parts) > 0 {
				membershipUIDs = append(membershipUIDs, parts[len(parts)-1])
			}
		}
	}

	// If there are remaining filters, we must fetch and filter all records before paginating
	if len(remainingFilters) > 0 {
		var filtered []*model.Membership
		for _, uid := range membershipUIDs {
			membership := &model.Membership{}
			_, errGet := s.get(ctx, constants.KVBucketNameMemberships, uid, membership)
			if errGet != nil {
				slog.WarnContext(ctx, "failed to get membership from lookup",
					"uid", uid,
					"error", errGet,
				)
				continue
			}
			if matchesFilters(membership, remainingFilters) {
				filtered = append(filtered, membership)
			}
		}

		totalSize := len(filtered)
		start := params.Offset
		if start > totalSize {
			start = totalSize
		}
		end := start + params.PageSize
		if end > totalSize {
			end = totalSize
		}

		return filtered[start:end], totalSize, nil
	}

	totalSize := len(membershipUIDs)

	// Paginate at the key level
	start := params.Offset
	if start > totalSize {
		start = totalSize
	}
	end := start + params.PageSize
	if end > totalSize {
		end = totalSize
	}

	pageUIDs := membershipUIDs[start:end]
	memberships := make([]*model.Membership, 0, len(pageUIDs))
	for _, uid := range pageUIDs {
		membership := &model.Membership{}
		_, errGet := s.get(ctx, constants.KVBucketNameMemberships, uid, membership)
		if errGet != nil {
			slog.WarnContext(ctx, "failed to get membership from lookup",
				"uid", uid,
				"error", errGet,
			)
			continue
		}
		memberships = append(memberships, membership)
	}

	slog.DebugContext(ctx, "retrieved memberships via lookup",
		"lookup_prefix", lookupPrefix,
		"total_size", totalSize,
		"returned", len(memberships),
	)

	return memberships, totalSize, nil
}

// ListKeyContacts retrieves key contacts for a membership using the lookup index
func (s *storage) ListKeyContacts(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	slog.DebugContext(ctx, "listing key contacts from NATS storage", "membership_uid", membershipUID)

	kv := s.client.kvStore[constants.KVBucketNameMembershipContacts]
	lookupPrefix := fmt.Sprintf("lookup/membership/%s/", membershipUID)

	keys, errKeys := kv.ListKeys(ctx, jetstream.MetaOnly())
	if errKeys != nil {
		if errors.Is(errKeys, jetstream.ErrNoKeysFound) {
			return []*model.KeyContact{}, nil
		}
		return nil, errs.NewUnexpected("failed to list keys from membership-contacts bucket", errKeys)
	}

	// Collect contact UIDs from lookup keys
	var contactUIDs []string
	for key := range keys.Keys() {
		if strings.HasPrefix(key, lookupPrefix) {
			parts := strings.Split(key, "/")
			if len(parts) > 0 {
				contactUIDs = append(contactUIDs, parts[len(parts)-1])
			}
		}
	}

	contacts := make([]*model.KeyContact, 0, len(contactUIDs))
	for _, uid := range contactUIDs {
		contact := &model.KeyContact{}
		_, errGet := s.get(ctx, constants.KVBucketNameMembershipContacts, uid, contact)
		if errGet != nil {
			slog.WarnContext(ctx, "failed to get key contact from lookup",
				"uid", uid,
				"error", errGet,
			)
			continue
		}
		contacts = append(contacts, contact)
	}

	slog.DebugContext(ctx, "retrieved key contacts from NATS storage",
		"membership_uid", membershipUID,
		"contact_count", len(contacts),
	)

	return contacts, nil
}

// ================== MembershipKVWriter implementation ==================

// WriteMembership writes a membership to the KV store and creates lookup keys
func (s *storage) WriteMembership(ctx context.Context, membership *model.Membership) error {
	if membership == nil {
		return errs.NewValidation("membership cannot be nil")
	}

	membershipBytes, errMarshal := json.Marshal(membership)
	if errMarshal != nil {
		return errs.NewUnexpected("failed to marshal membership", errMarshal)
	}

	kv := s.client.kvStore[constants.KVBucketNameMemberships]

	_, errPut := kv.Put(ctx, membership.UID, membershipBytes)
	if errPut != nil {
		return errs.NewUnexpected("failed to write membership", errPut)
	}

	// Write project lookup key
	if membership.Project.ID != "" {
		lookupKey := fmt.Sprintf(constants.KVLookupProjectPrefix, membership.Project.ID, membership.UID)
		if _, err := kv.Put(ctx, lookupKey, []byte(membership.UID)); err != nil {
			slog.WarnContext(ctx, "failed to write project lookup key",
				"key", lookupKey,
				"error", err,
			)
		}
	}

	return nil
}

// WriteKeyContact writes a key contact to the KV store and creates lookup keys
func (s *storage) WriteKeyContact(ctx context.Context, contact *model.KeyContact) error {
	if contact == nil {
		return errs.NewValidation("key contact cannot be nil")
	}

	contactBytes, errMarshal := json.Marshal(contact)
	if errMarshal != nil {
		return errs.NewUnexpected("failed to marshal key contact", errMarshal)
	}

	kv := s.client.kvStore[constants.KVBucketNameMembershipContacts]

	_, errPut := kv.Put(ctx, contact.UID, contactBytes)
	if errPut != nil {
		return errs.NewUnexpected("failed to write key contact", errPut)
	}

	// Write membership lookup key
	if contact.MembershipUID != "" {
		lookupKey := fmt.Sprintf(constants.KVLookupMembershipContactPrefix, contact.MembershipUID, contact.UID)
		if _, err := kv.Put(ctx, lookupKey, []byte(contact.UID)); err != nil {
			slog.WarnContext(ctx, "failed to write membership contact lookup key",
				"key", lookupKey,
				"error", err,
			)
		}
	}

	return nil
}

// IsReady checks if NATS is ready
func (s *storage) IsReady(ctx context.Context) error {
	return s.client.IsReady(ctx)
}

// NewStorage creates a new NATS storage implementation
func NewStorage(client *NATSClient) *storage {
	return &storage{
		client: client,
	}
}

// matchesFilters checks if a membership matches the given filters
func matchesFilters(m *model.Membership, filters map[string]string) bool {
	for key, value := range filters {
		switch strings.ToLower(key) {
		case "status":
			if !strings.EqualFold(m.Status, value) {
				return false
			}
		case "membership_type":
			if !strings.EqualFold(m.MembershipType, value) {
				return false
			}
		case "account_id":
			if m.Account.ID != value {
				return false
			}
		case "project_id":
			if m.Project.ID != value {
				return false
			}
		case "product_id":
			if m.Product.ID != value {
				return false
			}
		case "year":
			if m.Year != value {
				return false
			}
		case "tier":
			if !strings.EqualFold(m.Tier, value) {
				return false
			}
		case "contact_id":
			if m.Contact.ID != value {
				return false
			}
		case "auto_renew":
			if value == "true" && !m.AutoRenew {
				return false
			}
			if value == "false" && m.AutoRenew {
				return false
			}
		}
	}
	return true
}
