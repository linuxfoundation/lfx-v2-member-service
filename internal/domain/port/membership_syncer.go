// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// MembershipSourceReader reads membership data from the source (PostgreSQL)
type MembershipSourceReader interface {
	FetchAllMemberships(ctx context.Context) ([]*model.Membership, error)
	FetchAllKeyContacts(ctx context.Context) ([]*model.KeyContact, error)
	FetchAllMembers(ctx context.Context) ([]*model.Member, error)
}

// MembershipKVWriter writes membership data to the KV store (NATS)
type MembershipKVWriter interface {
	WriteMembership(ctx context.Context, membership *model.Membership) error
	WriteKeyContact(ctx context.Context, contact *model.KeyContact) error
	WriteMember(ctx context.Context, member *model.Member) error
	WriteProjectAliasLookups(ctx context.Context, aliasProjectID, membershipUID, memberUID string) error
	PurgeBucket(ctx context.Context, bucket string) error
}

// ProjectIDMapper fetches PCC→Salesforce project ID mappings
type ProjectIDMapper interface {
	FetchProjectIDMapping(ctx context.Context) (map[string]string, error)
}
