// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// MemberReader provides read access to member and membership data from NATS KV
type MemberReader interface {
	GetMember(ctx context.Context, uid string) (*model.Member, uint64, error)
	ListMembers(ctx context.Context, params model.ListParams) ([]*model.Member, int, error)
	GetMembershipForMember(ctx context.Context, memberUID, membershipUID string) (*model.Membership, uint64, error)
	ListKeyContactsForMembership(ctx context.Context, memberUID, membershipUID string) ([]*model.KeyContact, error)
	IsReady(ctx context.Context) error
}
