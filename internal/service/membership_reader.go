// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// MembershipReader defines the interface for membership read operations
type MembershipReader interface {
	GetMembership(ctx context.Context, uid string) (*model.Membership, uint64, error)
	ListMemberships(ctx context.Context, params model.ListParams) ([]*model.Membership, int, error)
	ListKeyContacts(ctx context.Context, membershipUID string) ([]*model.KeyContact, error)
}

// membershipReaderOrchestratorOption defines a function type for setting options
type membershipReaderOrchestratorOption func(*membershipReaderOrchestrator)

// WithMembershipReader sets the membership reader
func WithMembershipReader(reader port.MembershipReader) membershipReaderOrchestratorOption {
	return func(r *membershipReaderOrchestrator) {
		r.membershipReader = reader
	}
}

// membershipReaderOrchestrator orchestrates the membership reading process
type membershipReaderOrchestrator struct {
	membershipReader port.MembershipReader
}

// GetMembership retrieves a membership by UID
func (rc *membershipReaderOrchestrator) GetMembership(ctx context.Context, uid string) (*model.Membership, uint64, error) {
	slog.DebugContext(ctx, "executing get membership use case", "uid", uid)

	membership, revision, err := rc.membershipReader.GetMembership(ctx, uid)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get membership", "error", err, "uid", uid)
		return nil, 0, err
	}

	slog.DebugContext(ctx, "membership retrieved successfully", "uid", uid, "revision", revision)
	return membership, revision, nil
}

// ListMemberships retrieves memberships with pagination and filtering
func (rc *membershipReaderOrchestrator) ListMemberships(ctx context.Context, params model.ListParams) ([]*model.Membership, int, error) {
	slog.DebugContext(ctx, "executing list memberships use case",
		"page_size", params.PageSize,
		"offset", params.Offset,
	)

	memberships, totalSize, err := rc.membershipReader.ListMemberships(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list memberships", "error", err)
		return nil, 0, err
	}

	slog.DebugContext(ctx, "memberships retrieved successfully", "total_size", totalSize)
	return memberships, totalSize, nil
}

// ListKeyContacts retrieves key contacts for a membership
func (rc *membershipReaderOrchestrator) ListKeyContacts(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	slog.DebugContext(ctx, "executing list key contacts use case", "membership_uid", membershipUID)

	// Verify membership exists first
	_, _, err := rc.membershipReader.GetMembership(ctx, membershipUID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get membership - membership does not exist",
			"error", err,
			"membership_uid", membershipUID,
		)
		return nil, err
	}

	contacts, err := rc.membershipReader.ListKeyContacts(ctx, membershipUID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list key contacts", "error", err, "membership_uid", membershipUID)
		return nil, err
	}

	slog.DebugContext(ctx, "key contacts retrieved successfully",
		"membership_uid", membershipUID,
		"contact_count", len(contacts),
	)
	return contacts, nil
}

// NewMembershipReaderOrchestrator creates a new membership reader orchestrator
func NewMembershipReaderOrchestrator(opts ...membershipReaderOrchestratorOption) MembershipReader {
	rc := &membershipReaderOrchestrator{}
	for _, opt := range opts {
		opt(rc)
	}
	return rc
}
