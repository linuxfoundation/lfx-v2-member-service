// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats provides NATS JetStream KV-backed implementations of the domain
// storage ports.
package nats

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// projectIDMapRequest is the JSON request body for the project-id-map lookup RPC.
type projectIDMapRequest struct {
	// ProjectUID is the v2 project UID to resolve.
	ProjectUID string `json:"project_uid"`
}

// projectIDMapResponse is the JSON response body for the project-id-map lookup
// RPC. On success, ProjectSFID is populated. On failure, Error is populated.
type projectIDMapResponse struct {
	// ProjectSFID is the Salesforce Project__c.Id resolved from the UID.
	ProjectSFID string `json:"project_sfid,omitempty"`
	// Error is a human-readable error message returned when resolution fails.
	Error string `json:"error,omitempty"`
}

// SubscribeProjectIDMap registers a NATS core request/reply subscription on
// constants.ProjectIDMapLookupSubject. On each request it resolves the v2
// project UID in the JSON body to a Salesforce Project__c.Id using the supplied
// resolver and replies with a JSON response. Uses queue group
// constants.ServiceName so exactly one member-api replica handles each request
// when horizontally scaled.
//
// The returned *nats.Subscription can be used to drain or unsubscribe on
// shutdown. A non-nil error means the subscription could not be established.
// processProjectIDMapRequest decodes a raw request body and calls the resolver,
// returning the response value to be marshalled. Extracted for testability.
func processProjectIDMapRequest(ctx context.Context, data []byte, resolver port.ProjectResolver) any {
	var req projectIDMapRequest
	if err := json.Unmarshal(data, &req); err != nil {
		slog.WarnContext(ctx, "project-id-map: failed to decode request", "error", err)
		return map[string]string{"error": "invalid request body"}
	}
	if req.ProjectUID == "" {
		return map[string]string{"error": "project_uid is required"}
	}
	sfid, err := resolver.SFIDFromUID(ctx, req.ProjectUID)
	if err != nil {
		slog.DebugContext(ctx, "project-id-map: resolution failed",
			"project_uid", req.ProjectUID, "error", err)
		return map[string]string{"error": "project not found"}
	}
	return projectIDMapResponse{ProjectSFID: sfid}
}

func SubscribeProjectIDMap(conn *nats.Conn, resolver port.ProjectResolver) (*nats.Subscription, error) {
	sub, err := conn.QueueSubscribe(constants.ProjectIDMapLookupSubject, constants.ServiceName, func(msg *nats.Msg) {
		msgCtx := otel.GetTextMapPropagator().Extract(context.Background(), natsHeaderCarrier(msg.Header))
		msgCtx, span := tracer.Start(msgCtx, "nats.process",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "nats"),
				attribute.String("messaging.destination.name", constants.ProjectIDMapLookupSubject),
				attribute.String("messaging.operation.type", "process"),
				attribute.Int("messaging.message.body.size", len(msg.Data)),
			),
		)
		defer span.End()
		replyJSON(msg, processProjectIDMapRequest(msgCtx, msg.Data, resolver))
	})
	if err != nil {
		return nil, err
	}

	slog.Info("subscribed to project-id-map lookup RPC",
		"subject", constants.ProjectIDMapLookupSubject,
		"queue_group", constants.ServiceName,
	)

	return sub, nil
}

// replyJSON marshals payload and publishes it to msg.Reply. Marshalling errors
// are logged and a JSON error response is sent instead so the caller never
// hangs waiting for a reply that never arrives. Shared by all id-map handlers
// in this package.
func replyJSON(msg *nats.Msg, payload any) {
	if msg.Reply == "" {
		// Fire-and-forget message; nothing to reply to.
		return
	}

	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("id-map: failed to marshal response", "error", err)
		_ = msg.Respond([]byte(`{"error":"internal serialisation error"}`))
		return
	}

	if err := msg.Respond(data); err != nil {
		slog.Error("id-map: failed to send reply", "error", err)
	}
}
