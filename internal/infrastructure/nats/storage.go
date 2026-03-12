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
	client          *NATSClient
	projectResolver *projectResolver
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

// getValue retrieves the raw value from a NATS KV store key.
func (s *storage) getValue(ctx context.Context, bucket, key string) ([]byte, error) {
	data, err := s.client.kvStore[bucket].Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return data.Value(), nil
}

// ================== MemberReader implementation ==================

// GetMember retrieves a member by UID
func (s *storage) GetMember(ctx context.Context, uid string) (*model.Member, uint64, error) {
	member := &model.Member{}

	rev, errGet := s.get(ctx, constants.KVBucketNameMembers, uid, member)
	if errGet != nil {
		if errors.Is(errGet, jetstream.ErrKeyNotFound) {
			return nil, 0, errs.NewNotFound("member not found", fmt.Errorf("member UID: %s", uid))
		}
		return nil, 0, errs.NewUnexpected("failed to get member", errGet)
	}

	return member, rev, nil
}

// ListMembers retrieves members with pagination, filtering, and search
func (s *storage) ListMembers(ctx context.Context, params model.ListParams) ([]*model.Member, int, error) {
	slog.DebugContext(ctx, "listing members from NATS storage",
		"page_size", params.PageSize,
		"offset", params.Offset,
		"search", params.Search,
	)

	kv := s.client.kvStore[constants.KVBucketNameMembers]

	// If filtering by project_id, use the lookup index for fast retrieval.
	// When the caller supplies a v2 UUID, translate it to the B2B Salesforce
	// Project__c SFID that the lookup index is keyed on.
	if projectID, ok := params.Filters["project_id"]; ok {
		resolvedID := s.resolveProjectFilterID(ctx, projectID)
		return s.listMembersByProjectLookup(ctx, kv, resolvedID, params)
	}

	// If filtering by member_id (SFID or UUID), resolve to member UID
	if memberID, ok := params.Filters["member_id"]; ok {
		return s.listMembersByMemberID(ctx, memberID, params)
	}

	hasFilters := len(params.Filters) > 0 || params.Search != ""

	keys, errKeys := kv.ListKeys(ctx)
	if errKeys != nil {
		if errors.Is(errKeys, jetstream.ErrNoKeysFound) {
			return []*model.Member{}, 0, nil
		}
		return nil, 0, errs.NewUnexpected("failed to list keys from members bucket", errKeys)
	}

	// When no filters/search, paginate at the key level
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
		members := make([]*model.Member, 0, len(pageKeys))
		for _, key := range pageKeys {
			member := &model.Member{}
			_, errGet := s.get(ctx, constants.KVBucketNameMembers, key, member)
			if errGet != nil {
				slog.WarnContext(ctx, "failed to get member while listing",
					"key", key,
					"error", errGet,
				)
				continue
			}
			members = append(members, member)
		}

		slog.DebugContext(ctx, "retrieved members from NATS storage",
			"total_size", totalSize,
			"page_size", params.PageSize,
			"offset", params.Offset,
			"returned", len(members),
		)

		return members, totalSize, nil
	}

	// With filters/search, load and check each record
	var filtered []*model.Member
	for key := range keys.Keys() {
		if strings.HasPrefix(key, "lookup/") {
			continue
		}

		member := &model.Member{}
		_, errGet := s.get(ctx, constants.KVBucketNameMembers, key, member)
		if errGet != nil {
			slog.WarnContext(ctx, "failed to get member while listing",
				"key", key,
				"error", errGet,
			)
			continue
		}

		if !matchesMemberFilters(member, params.Filters, params.Search) {
			continue
		}

		filtered = append(filtered, member)
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

	slog.DebugContext(ctx, "retrieved members from NATS storage",
		"total_size", totalSize,
		"page_size", params.PageSize,
		"offset", params.Offset,
		"returned", end-start,
	)

	return filtered[start:end], totalSize, nil
}

// listMembersByProjectLookup uses the project lookup index to find members
func (s *storage) listMembersByProjectLookup(ctx context.Context, kv jetstream.KeyValue, projectID string, params model.ListParams) ([]*model.Member, int, error) {
	keys, errKeys := kv.ListKeys(ctx, jetstream.MetaOnly())
	if errKeys != nil {
		if errors.Is(errKeys, jetstream.ErrNoKeysFound) {
			return []*model.Member{}, 0, nil
		}
		return nil, 0, errs.NewUnexpected("failed to list keys from members bucket", errKeys)
	}

	lookupPrefix := fmt.Sprintf("lookup/member-project/%s/", projectID)

	// Collect member UIDs from lookup keys matching the prefix
	var memberUIDs []string
	for key := range keys.Keys() {
		if strings.HasPrefix(key, lookupPrefix) {
			parts := strings.Split(key, "/")
			if len(parts) > 0 {
				memberUIDs = append(memberUIDs, parts[len(parts)-1])
			}
		}
	}

	// Build remaining filters (excluding project_id)
	remainingFilters := make(map[string]string)
	for k, v := range params.Filters {
		if k != "project_id" {
			remainingFilters[k] = v
		}
	}

	hasRemainingFilters := len(remainingFilters) > 0 || params.Search != ""

	if hasRemainingFilters {
		var filtered []*model.Member
		for _, uid := range memberUIDs {
			member := &model.Member{}
			_, errGet := s.get(ctx, constants.KVBucketNameMembers, uid, member)
			if errGet != nil {
				slog.WarnContext(ctx, "failed to get member from lookup", "uid", uid, "error", errGet)
				continue
			}
			if matchesMemberFilters(member, remainingFilters, params.Search) {
				filtered = append(filtered, member)
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

	totalSize := len(memberUIDs)
	start := params.Offset
	if start > totalSize {
		start = totalSize
	}
	end := start + params.PageSize
	if end > totalSize {
		end = totalSize
	}

	pageUIDs := memberUIDs[start:end]
	members := make([]*model.Member, 0, len(pageUIDs))
	for _, uid := range pageUIDs {
		member := &model.Member{}
		_, errGet := s.get(ctx, constants.KVBucketNameMembers, uid, member)
		if errGet != nil {
			slog.WarnContext(ctx, "failed to get member from lookup", "uid", uid, "error", errGet)
			continue
		}
		members = append(members, member)
	}

	return members, totalSize, nil
}

// listMembersByMemberID resolves a member_id (SFID or UUID) and returns matching members
func (s *storage) listMembersByMemberID(ctx context.Context, memberID string, params model.ListParams) ([]*model.Member, int, error) {
	// Try direct UUID lookup first
	member := &model.Member{}
	_, errGet := s.get(ctx, constants.KVBucketNameMembers, memberID, member)
	if errGet == nil {
		return []*model.Member{member}, 1, nil
	}

	// Try SFID lookup
	sfidKey := fmt.Sprintf(constants.KVLookupMemberBySFIDPrefix, memberID)
	val, errVal := s.getValue(ctx, constants.KVBucketNameMembers, sfidKey)
	if errVal != nil {
		if errors.Is(errVal, jetstream.ErrKeyNotFound) {
			return []*model.Member{}, 0, nil
		}
		return nil, 0, errs.NewUnexpected("failed to resolve member_id", errVal)
	}

	memberUID := string(val)
	member = &model.Member{}
	_, errGet = s.get(ctx, constants.KVBucketNameMembers, memberUID, member)
	if errGet != nil {
		return []*model.Member{}, 0, nil
	}

	return []*model.Member{member}, 1, nil
}

// GetMembershipForMember retrieves a membership and verifies it belongs to the specified member
func (s *storage) GetMembershipForMember(ctx context.Context, memberUID, membershipUID string) (*model.Membership, uint64, error) {
	membership := &model.Membership{}

	rev, errGet := s.get(ctx, constants.KVBucketNameMemberships, membershipUID, membership)
	if errGet != nil {
		if errors.Is(errGet, jetstream.ErrKeyNotFound) {
			return nil, 0, errs.NewNotFound("membership not found", fmt.Errorf("membership UID: %s", membershipUID))
		}
		return nil, 0, errs.NewUnexpected("failed to get membership", errGet)
	}

	if membership.MemberUID != memberUID {
		return nil, 0, errs.NewNotFound("membership not found for this member",
			fmt.Errorf("membership %s does not belong to member %s", membershipUID, memberUID))
	}

	return membership, rev, nil
}

// ListKeyContactsForMembership retrieves key contacts for a membership after verifying member ownership
func (s *storage) ListKeyContactsForMembership(ctx context.Context, memberUID, membershipUID string) ([]*model.KeyContact, error) {
	// Verify membership belongs to member
	_, _, err := s.GetMembershipForMember(ctx, memberUID, membershipUID)
	if err != nil {
		return nil, err
	}

	// Reuse existing contact lookup logic
	return s.ListKeyContacts(ctx, membershipUID)
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

	// If filtering by project_id, use the lookup index for efficient retrieval.
	// When the caller supplies a v2 UUID, translate it to the B2B Salesforce
	// Project__c SFID that the lookup index is keyed on.
	if projectID, ok := params.Filters["project_id"]; ok {
		resolvedID := s.resolveProjectFilterID(ctx, projectID)
		// Build remaining filters (excluding project_id which is handled by lookup)
		remainingFilters := make(map[string]string)
		for k, v := range params.Filters {
			if k != "project_id" {
				remainingFilters[k] = v
			}
		}
		return s.listMembershipsByLookup(ctx, kv, fmt.Sprintf("lookup/project/%s/", resolvedID), params, remainingFilters)
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

// WriteMember writes a member to the KV store and creates lookup keys
func (s *storage) WriteMember(ctx context.Context, member *model.Member) error {
	if member == nil {
		return errs.NewValidation("member cannot be nil")
	}

	memberBytes, errMarshal := json.Marshal(member)
	if errMarshal != nil {
		return errs.NewUnexpected("failed to marshal member", errMarshal)
	}

	kv := s.client.kvStore[constants.KVBucketNameMembers]

	_, errPut := kv.Put(ctx, member.UID, memberBytes)
	if errPut != nil {
		return errs.NewUnexpected("failed to write member", errPut)
	}

	// Write SFID lookup keys for dual-ID resolution
	for _, sfid := range member.SFIDs {
		if sfid == "" {
			continue
		}
		lookupKey := fmt.Sprintf(constants.KVLookupMemberBySFIDPrefix, sfid)
		if _, err := kv.Put(ctx, lookupKey, []byte(member.UID)); err != nil {
			slog.WarnContext(ctx, "failed to write member SFID lookup key",
				"key", lookupKey,
				"error", err,
			)
		}
	}

	return nil
}

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

	// Write member-membership lookup key
	if membership.MemberUID != "" {
		lookupKey := fmt.Sprintf(constants.KVLookupMembershipByMemberPrefix, membership.MemberUID, membership.UID)
		if _, err := kv.Put(ctx, lookupKey, []byte(membership.UID)); err != nil {
			slog.WarnContext(ctx, "failed to write member-membership lookup key",
				"key", lookupKey,
				"error", err,
			)
		}

		// Write member-project lookup key in the members bucket
		if membership.Project.ID != "" {
			memberKV := s.client.kvStore[constants.KVBucketNameMembers]
			memberProjectKey := fmt.Sprintf(constants.KVLookupMemberByProjectPrefix, membership.Project.ID, membership.MemberUID)
			if _, err := memberKV.Put(ctx, memberProjectKey, []byte(membership.MemberUID)); err != nil {
				slog.WarnContext(ctx, "failed to write member-project lookup key",
					"key", memberProjectKey,
					"error", err,
				)
			}
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

// WriteProjectAliasLookups writes additional lookup keys under a PCC project ID alias
func (s *storage) WriteProjectAliasLookups(ctx context.Context, aliasProjectID, membershipUID, memberUID string) error {
	// Write project lookup in memberships bucket: lookup/project/{pccProjectID}/{membershipUID}
	membershipKV := s.client.kvStore[constants.KVBucketNameMemberships]
	projectKey := fmt.Sprintf(constants.KVLookupProjectPrefix, aliasProjectID, membershipUID)
	if _, err := membershipKV.Put(ctx, projectKey, []byte(membershipUID)); err != nil {
		return fmt.Errorf("failed to write project alias lookup: %w", err)
	}

	// Write member-project lookup in members bucket: lookup/member-project/{pccProjectID}/{memberUID}
	if memberUID != "" {
		memberKV := s.client.kvStore[constants.KVBucketNameMembers]
		memberProjectKey := fmt.Sprintf(constants.KVLookupMemberByProjectPrefix, aliasProjectID, memberUID)
		if _, err := memberKV.Put(ctx, memberProjectKey, []byte(memberUID)); err != nil {
			return fmt.Errorf("failed to write member-project alias lookup: %w", err)
		}
	}

	return nil
}

// PurgeBucket deletes all keys in a bucket and recreates it to ensure a clean state
func (s *storage) PurgeBucket(ctx context.Context, bucket string) error {
	kv, ok := s.client.kvStore[bucket]
	if !ok {
		return errs.NewValidation(fmt.Sprintf("bucket %s not found", bucket))
	}

	keys, err := kv.ListKeys(ctx, jetstream.MetaOnly())
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return errs.NewUnexpected("failed to list keys for purge", err)
	}

	deleted := 0
	for key := range keys.Keys() {
		if err := kv.Delete(ctx, key); err != nil {
			slog.WarnContext(ctx, "failed to delete key during purge",
				"bucket", bucket,
				"key", key,
				"error", err,
			)
			continue
		}
		deleted++
	}

	slog.InfoContext(ctx, "purged bucket",
		"bucket", bucket,
		"deleted_keys", deleted,
	)
	return nil
}

// IsReady checks if NATS is ready
func (s *storage) IsReady(ctx context.Context) error {
	return s.client.IsReady(ctx)
}

// NewStorage creates a new NATS storage implementation
func NewStorage(client *NATSClient) *storage {
	return &storage{
		client:          client,
		projectResolver: newProjectResolver(),
	}
}

// resolveProjectFilterID translates a project_id filter value to the B2B Salesforce
// Project__c SFID used as the key in the membership lookup index. When the supplied
// value is already a raw SFID (not a v2 UUID) it is returned unchanged, preserving
// backward compatibility for callers that pass SFIDs directly.
//
// The two-stage resolution is:
//  1. NATS RPC "project.uid.{v2_uid}" → B2C Salesforce SFID.
//  2. v1-objects KV fetch "salesforce-project__c.{b2c_sfid}" → "saleforce_id" field
//     (note: intentional typo in source data) → B2B Project__c.Id SFID.
//
// Returns the original value unchanged when resolution fails or is unavailable,
// so that callers always have a usable lookup key.
func (s *storage) resolveProjectFilterID(ctx context.Context, projectID string) string {
	if !isV2UUID(projectID) {
		// Already looks like a raw Salesforce SFID — use as-is.
		return projectID
	}

	resolved := s.projectResolver.ResolveB2BSFID(ctx, s.client, projectID)
	if resolved == "" {
		slog.DebugContext(ctx, "project filter: could not resolve v2 UID to B2B SFID, using original value",
			"project_id", projectID,
		)
		return projectID
	}

	return resolved
}

// matchesFilters checks if a membership matches the given filters.
//
// The account_id, project_id, contact_id, and product_id filter keys are
// internal lookup filters that compare against raw Salesforce SFIDs stored in
// the domain model, not the v2 UIDs returned in API responses. If these are
// ever exposed as public API filter parameters, they will need translation
// from v2 UIDs to SFIDs (similar to how project_id is already translated in
// resolveProjectFilterID before reaching this function).
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

// matchesMemberFilters checks if a member matches the given filters and search term.
// Filters operate on the MembershipSummary to enable filtering by membership-level fields.
func matchesMemberFilters(m *model.Member, filters map[string]string, search string) bool {
	// Check search term first (case-insensitive substring across name, project names, tiers)
	if search != "" {
		searchLower := strings.ToLower(search)
		found := false

		if strings.Contains(strings.ToLower(m.Name), searchLower) {
			found = true
		}

		if !found && m.MembershipSummary != nil {
			for _, ms := range m.MembershipSummary.Memberships {
				if strings.Contains(strings.ToLower(ms.Project.Name), searchLower) ||
					strings.Contains(strings.ToLower(ms.Project.Slug), searchLower) ||
					strings.Contains(strings.ToLower(ms.Tier), searchLower) ||
					strings.Contains(strings.ToLower(ms.Name), searchLower) {
					found = true
					break
				}
			}
		}

		if !found {
			return false
		}
	}

	// Check filters
	for key, value := range filters {
		switch strings.ToLower(key) {
		case "name":
			if !strings.Contains(strings.ToLower(m.Name), strings.ToLower(value)) {
				return false
			}
		case "tier":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return strings.EqualFold(ms.Tier, value)
			}) {
				return false
			}
		case "status":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return strings.EqualFold(ms.Status, value)
			}) {
				return false
			}
		case "year":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return ms.Year == value
			}) {
				return false
			}
		case "product_name":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return strings.Contains(strings.ToLower(ms.Product.Name), strings.ToLower(value))
			}) {
				return false
			}
		case "project_name":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return strings.Contains(strings.ToLower(ms.Project.Name), strings.ToLower(value))
			}) {
				return false
			}
		case "project_slug":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return strings.EqualFold(ms.Project.Slug, value)
			}) {
				return false
			}
		case "membership_type":
			if !memberHasMembershipMatch(m, func(ms model.MembershipSummaryItem) bool {
				return strings.EqualFold(ms.MembershipType, value)
			}) {
				return false
			}
		}
	}
	return true
}

// memberHasMembershipMatch checks if any membership in the summary matches the predicate
func memberHasMembershipMatch(m *model.Member, predicate func(model.MembershipSummaryItem) bool) bool {
	if m.MembershipSummary == nil {
		return false
	}
	for _, ms := range m.MembershipSummary.Memberships {
		if predicate(ms) {
			return true
		}
	}
	return false
}
