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
}

// MembershipKVWriter writes membership data to the KV store (NATS)
type MembershipKVWriter interface {
	WriteMembership(ctx context.Context, membership *model.Membership) error
	WriteKeyContact(ctx context.Context, contact *model.KeyContact) error
	PurgeBucket(ctx context.Context, bucket string) error
}
