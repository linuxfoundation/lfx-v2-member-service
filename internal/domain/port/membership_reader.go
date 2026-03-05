// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// MembershipReader provides read access to membership data from NATS KV
type MembershipReader interface {
	GetMembership(ctx context.Context, uid string) (*model.Membership, uint64, error)
	ListMemberships(ctx context.Context, params model.ListParams) ([]*model.Membership, int, error)
	ListKeyContacts(ctx context.Context, membershipUID string) ([]*model.KeyContact, error)
	IsReady(ctx context.Context) error
}
