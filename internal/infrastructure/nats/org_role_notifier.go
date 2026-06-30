// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	natsgo "github.com/nats-io/nats.go"

	emailapi "github.com/linuxfoundation/lfx-v2-email-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	infraemail "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/email"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

type orgRoleNotifier struct {
	client      *NATSClient
	platformURL string
}

// NewOrgRoleNotifier creates a NATS-backed OrgRoleNotifier. It renders the
// role-assignment email locally and publishes it to the email-service subject.
// platformURL is injected at construction so the use case never touches URLs.
func NewOrgRoleNotifier(client *NATSClient, platformURL string) port.OrgRoleNotifier {
	return &orgRoleNotifier{client: client, platformURL: platformURL}
}

// NotifyRoleAssigned renders the v5 role-assignment email and sends it via the
// email service. Errors should be treated as best-effort by the caller.
func (n *orgRoleNotifier) NotifyRoleAssigned(ctx context.Context, notif port.OrgRoleAssignedNotification) error {
	if n.client == nil || n.client.conn == nil {
		return pkgerrors.NewServiceUnavailable("org role notifier is not configured", nil)
	}

	subject, htmlBody, textBody, err := infraemail.RenderOrgRoleAssigned(notif.OrgName, n.platformURL, notif.Role)
	if err != nil {
		return fmt.Errorf("render role-assignment email: %w", err)
	}

	payload := emailapi.SendEmailRequest{
		To:      notif.RecipientEmail,
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return pkgerrors.NewUnexpected("marshal role-assignment email request", err)
	}

	ctx, cancel := context.WithTimeout(ctx, defaultPublishTimeout)
	defer cancel()

	reply, err := requestMsgWithSpan(ctx, n.client.conn, &natsgo.Msg{
		Subject: emailapi.SendEmailSubject,
		Data:    data,
	})
	if err != nil {
		return pkgerrors.NewServiceUnavailable("email service unavailable", err)
	}

	var errResp emailapi.SendEmailErrorResponse
	if len(reply.Data) > 0 {
		if jsonErr := json.Unmarshal(reply.Data, &errResp); jsonErr == nil && errResp.Error != "" {
			slog.WarnContext(ctx, "email service returned error for role-assignment email",
				"recipient", redaction.RedactEmail(notif.RecipientEmail), "error", errResp.Error)
			return pkgerrors.NewUnexpected("email service rejected role-assignment email", fmt.Errorf("%s", errResp.Error))
		}
	}
	return nil
}
