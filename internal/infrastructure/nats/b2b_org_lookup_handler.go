// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// b2bOrgLookupRequest is the JSON request body for the b2b_org lookup RPC.
type b2bOrgLookupRequest struct {
	ID string `json:"id"`
}

// b2bOrgLookupResponse is the JSON response body for the b2b_org lookup RPC.
type b2bOrgLookupResponse struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// processB2BOrgLookupRequest decodes a raw request body and looks up the org by id.
func processB2BOrgLookupRequest(ctx context.Context, data []byte, reader port.B2BOrgReader) any {
	var req b2bOrgLookupRequest
	if err := json.Unmarshal(data, &req); err != nil {
		slog.WarnContext(ctx, "b2b_org_lookup: failed to decode request", "error", err)
		return b2bOrgLookupResponse{Error: "invalid request body"}
	}

	id := strings.TrimSpace(req.ID)
	if id == "" {
		return b2bOrgLookupResponse{Error: "id is required"}
	}
	if normalized, err := sfuuid.Normalize18(id); err == nil {
		id = normalized
	}

	org, err := reader.GetB2BOrg(ctx, id)
	if err != nil {
		var notFound errs.NotFound
		if errors.As(err, &notFound) {
			slog.DebugContext(ctx, "b2b_org_lookup: org not found", "id", req.ID)
			return b2bOrgLookupResponse{Error: "b2b org not found"}
		}
		slog.WarnContext(ctx, "b2b_org_lookup: lookup failed", "id", req.ID, "error", err)
		return b2bOrgLookupResponse{Error: "b2b org lookup failed"}
	}
	if org == nil || strings.TrimSpace(org.UID) == "" {
		return b2bOrgLookupResponse{Error: "b2b org not found"}
	}

	return b2bOrgLookupResponse{ID: strings.TrimSpace(org.UID)}
}

// SubscribeB2BOrgLookup registers a NATS request/reply subscription on
// constants.B2BOrgLookupSubject.
func SubscribeB2BOrgLookup(conn *nats.Conn, reader port.B2BOrgReader) (*nats.Subscription, error) {
	sub, err := conn.Subscribe(constants.B2BOrgLookupSubject, func(msg *nats.Msg) {
		replyJSON(msg, processB2BOrgLookupRequest(context.Background(), msg.Data, reader))
	})
	if err != nil {
		return nil, err
	}

	slog.Info("subscribed to b2b org lookup RPC",
		"subject", constants.B2BOrgLookupSubject,
	)

	return sub, nil
}
