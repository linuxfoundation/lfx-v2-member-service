// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// b2bOrgLookupByWebsiteHandlerTimeout bounds downstream FindByNameOrWebsite work
// (Salesforce SOQL) so a hung dependency cannot pin the subscription callback
// indefinitely.
const b2bOrgLookupByWebsiteHandlerTimeout = 30 * time.Second

// b2bOrgLookupByWebsiteRequest is the JSON request body for the b2b_org
// name/website lookup RPC.
type b2bOrgLookupByWebsiteRequest struct {
	Name    string `json:"name"`
	Website string `json:"website"`
}

// b2bOrgLookupByWebsiteResponse is the JSON response body for the b2b_org
// name/website lookup RPC.
type b2bOrgLookupByWebsiteResponse struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// processB2BOrgLookupByWebsiteRequest decodes a raw request body and looks up
// the org by name/website.
func processB2BOrgLookupByWebsiteRequest(ctx context.Context, data []byte, reader port.B2BOrgReader) any {
	var req b2bOrgLookupByWebsiteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		slog.WarnContext(ctx, "b2b_org_lookup_by_website: failed to decode request", "error", err)
		return b2bOrgLookupByWebsiteResponse{Error: "invalid request body"}
	}

	name := strings.TrimSpace(req.Name)
	website := strings.TrimSpace(req.Website)
	if name == "" && website == "" {
		return b2bOrgLookupByWebsiteResponse{Error: "name or website is required"}
	}

	org, ok, err := reader.FindByNameOrWebsite(ctx, name, website)
	if err != nil {
		slog.WarnContext(ctx, "b2b_org_lookup_by_website: lookup failed", "name", name, "website", website, "error", err)
		return b2bOrgLookupByWebsiteResponse{Error: "b2b org lookup failed"}
	}
	if !ok || org == nil || strings.TrimSpace(org.UID) == "" {
		slog.DebugContext(ctx, "b2b_org_lookup_by_website: org not found", "name", name, "website", website)
		return b2bOrgLookupByWebsiteResponse{Error: "b2b org not found"}
	}

	return b2bOrgLookupByWebsiteResponse{ID: strings.TrimSpace(org.UID)}
}

// SubscribeB2BOrgLookupByWebsite registers a NATS request/reply subscription on
// constants.B2BOrgLookupByWebsiteSubject. Uses queue group constants.ServiceName
// so exactly one member-api replica handles each request when horizontally
// scaled.
func SubscribeB2BOrgLookupByWebsite(conn *nats.Conn, reader port.B2BOrgReader) (*nats.Subscription, error) {
	sub, err := conn.QueueSubscribe(constants.B2BOrgLookupByWebsiteSubject, constants.ServiceName, func(msg *nats.Msg) {
		msgCtx := otel.GetTextMapPropagator().Extract(context.Background(), natsHeaderCarrier(msg.Header))
		msgCtx, span := tracer.Start(msgCtx, "nats.process",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "nats"),
				attribute.String("messaging.destination.name", constants.B2BOrgLookupByWebsiteSubject),
				attribute.String("messaging.operation.type", "process"),
				attribute.Int("messaging.message.body.size", len(msg.Data)),
			),
		)
		defer span.End()

		msgCtx, cancel := context.WithTimeout(msgCtx, b2bOrgLookupByWebsiteHandlerTimeout)
		defer cancel()

		replyJSON(msg, processB2BOrgLookupByWebsiteRequest(msgCtx, msg.Data, reader))
	})
	if err != nil {
		return nil, err
	}

	slog.Info("subscribed to b2b org lookup by website RPC",
		"subject", constants.B2BOrgLookupByWebsiteSubject,
		"queue_group", constants.ServiceName,
	)

	return sub, nil
}
