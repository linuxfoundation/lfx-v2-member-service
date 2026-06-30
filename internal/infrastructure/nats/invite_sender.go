// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"log/slog"

	inviteapi "github.com/linuxfoundation/lfx-v2-invite-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/nats-io/nats.go"
)

type inviteSender struct {
	client *NATSClient
}

// SendInvite sends a request/reply to the invite service for a user who does not
// yet have an LFID and returns the invite metadata from the response.
func (s *inviteSender) SendInvite(ctx context.Context, req inviteapi.SendInviteRequest) (port.InviteResult, error) {
	if s.client == nil || s.client.conn == nil {
		return port.InviteResult{}, errors.NewServiceUnavailable("invite sender is not configured", nil)
	}

	if err := ctx.Err(); err != nil {
		return port.InviteResult{}, errors.NewUnexpected("context cancelled before sending invite", err)
	}

	ctx, cancel := context.WithTimeout(ctx, defaultPublishTimeout)
	defer cancel()

	data, err := json.Marshal(req)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal invite request", "error", err)
		return port.InviteResult{}, errors.NewUnexpected("failed to marshal invite request", err)
	}

	reply, err := requestMsgWithSpan(ctx, s.client.conn, &nats.Msg{
		Subject: inviteapi.SendInviteSubject,
		Data:    data,
	})
	if err != nil {
		slog.ErrorContext(ctx, "invite service request failed", "error", err)
		return port.InviteResult{}, errors.NewServiceUnavailable("invite service unavailable", err)
	}

	var resp inviteapi.SendInviteResponse
	if len(reply.Data) > 0 {
		if jsonErr := json.Unmarshal(reply.Data, &resp); jsonErr != nil {
			slog.ErrorContext(ctx, "error unmarshalling invite response", "error", jsonErr)
			return port.InviteResult{}, errors.NewUnexpected("failed to parse invite service response", jsonErr)
		}
		if resp.Error != "" {
			return port.InviteResult{}, errors.NewUnexpected("invite service returned an error", stderrors.New(resp.Error))
		}
	}

	var result port.InviteResult
	if resp.InviteData != nil {
		result.InviteUID = resp.UID
		result.RecipientEmail = resp.Email
		result.ExpiresAt = resp.ExpiresAt
	}
	slog.DebugContext(ctx, "invite service replied", "invite_uid", result.InviteUID, "expires_at", result.ExpiresAt)
	return result, nil
}

// NewInviteSender creates a NATS-backed InviteSender.
func NewInviteSender(client *NATSClient) port.InviteSender {
	return &inviteSender{client: client}
}
