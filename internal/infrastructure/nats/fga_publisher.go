// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// FGAPublisher publishes fga-sync access control messages over core NATS.
type FGAPublisher struct {
	client *NATSClient
}

// NewFGAPublisher creates a new FGAPublisher backed by the given NATS client.
func NewFGAPublisher(client *NATSClient) *FGAPublisher {
	return &FGAPublisher{client: client}
}

// UpdateMemberAccess publishes an update_access message granting the given team's
// members auditor access on the specified membership object.
func (p *FGAPublisher) UpdateMemberAccess(ctx context.Context, membershipUID string, auditorTeamID string) error {
	msg := model.FGASyncMessage{
		ObjectType: "member",
		Operation:  "update_access",
		Data: model.FGASyncData{
			UID:    membershipUID,
			Public: false,
			References: map[string][]string{
				"auditor": {fmt.Sprintf("team:%s#member", auditorTeamID)},
			},
		},
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal fga-sync message: %w", err)
	}

	if err := p.client.conn.Publish(constants.FGASyncUpdateAccessSubject, payload); err != nil {
		return fmt.Errorf("failed to publish fga-sync update_access: %w", err)
	}

	slog.DebugContext(ctx, "published fga-sync update_access",
		"membership_uid", membershipUID,
		"auditor_team", auditorTeamID,
	)
	return nil
}

// Ensure FGAPublisher implements port.FGAPublisher.
var _ port.FGAPublisher = (*FGAPublisher)(nil)
