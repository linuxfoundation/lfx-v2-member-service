// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// MemberReader defines the interface for member read operations
type MemberReader interface {
	ListMembers(ctx context.Context, params model.ListParams) ([]*model.Member, int, error)
	GetMembershipForMember(ctx context.Context, memberUID, membershipUID string) (*model.Membership, uint64, error)
	ListKeyContactsForMembership(ctx context.Context, memberUID, membershipUID string) ([]*model.KeyContact, error)
}

// memberReaderOrchestratorOption defines a function type for setting options
type memberReaderOrchestratorOption func(*memberReaderOrchestrator)

// WithMemberReader sets the member reader
func WithMemberReader(reader port.MemberReader) memberReaderOrchestratorOption {
	return func(r *memberReaderOrchestrator) {
		r.memberReader = reader
	}
}

// memberReaderOrchestrator orchestrates the member reading process
type memberReaderOrchestrator struct {
	memberReader port.MemberReader
}

// ListMembers retrieves members with pagination, filtering, and search
func (rc *memberReaderOrchestrator) ListMembers(ctx context.Context, params model.ListParams) ([]*model.Member, int, error) {
	slog.DebugContext(ctx, "executing list members use case",
		"page_size", params.PageSize,
		"offset", params.Offset,
		"search", params.Search,
	)

	members, totalSize, err := rc.memberReader.ListMembers(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list members", "error", err)
		return nil, 0, err
	}

	slog.DebugContext(ctx, "members retrieved successfully", "total_size", totalSize)
	return members, totalSize, nil
}

// GetMembershipForMember retrieves a membership for a specific member
func (rc *memberReaderOrchestrator) GetMembershipForMember(ctx context.Context, memberUID, membershipUID string) (*model.Membership, uint64, error) {
	slog.DebugContext(ctx, "executing get membership for member use case",
		"member_uid", memberUID,
		"membership_uid", membershipUID,
	)

	membership, revision, err := rc.memberReader.GetMembershipForMember(ctx, memberUID, membershipUID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get membership for member",
			"error", err,
			"member_uid", memberUID,
			"membership_uid", membershipUID,
		)
		return nil, 0, err
	}

	slog.DebugContext(ctx, "membership for member retrieved successfully",
		"member_uid", memberUID,
		"membership_uid", membershipUID,
		"revision", revision,
	)
	return membership, revision, nil
}

// ListKeyContactsForMembership retrieves key contacts for a membership under a member
func (rc *memberReaderOrchestrator) ListKeyContactsForMembership(ctx context.Context, memberUID, membershipUID string) ([]*model.KeyContact, error) {
	slog.DebugContext(ctx, "executing list key contacts for membership use case",
		"member_uid", memberUID,
		"membership_uid", membershipUID,
	)

	contacts, err := rc.memberReader.ListKeyContactsForMembership(ctx, memberUID, membershipUID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list key contacts for membership",
			"error", err,
			"member_uid", memberUID,
			"membership_uid", membershipUID,
		)
		return nil, err
	}

	slog.DebugContext(ctx, "key contacts for membership retrieved successfully",
		"member_uid", memberUID,
		"membership_uid", membershipUID,
		"contact_count", len(contacts),
	)
	return contacts, nil
}

// NewMemberReaderOrchestrator creates a new member reader orchestrator
func NewMemberReaderOrchestrator(opts ...memberReaderOrchestratorOption) MemberReader {
	rc := &memberReaderOrchestrator{}
	for _, opt := range opts {
		opt(rc)
	}
	if rc.memberReader == nil {
		panic("member reader is required: use WithMemberReader option")
	}
	return rc
}
