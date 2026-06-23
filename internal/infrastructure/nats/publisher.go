// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

const defaultPublishTimeout = 10 * time.Second

type messagePublisher struct {
	client *NATSClient
}

// NewMessagePublisher creates a new messagePublisher backed by the given NATSClient.
func NewMessagePublisher(client *NATSClient) port.MemberPublisher {
	return &messagePublisher{client: client}
}

// Indexer publishes an indexer message to the given NATS subject.
// Publish failures on the write path are logged but never propagated — the
// /admin/reindex backfill will recover missed records.
func (p *messagePublisher) Indexer(ctx context.Context, subject string, msg any, sync bool) error {
	return p.publish(ctx, subject, msg, "indexer", sync)
}

// Access publishes an FGA synchronisation message to the given NATS subject.
// Callers propagate failures for delete operations; writes swallow them.
func (p *messagePublisher) Access(ctx context.Context, subject string, msg any, sync bool) error {
	return p.publish(ctx, subject, msg, "access", sync)
}

func (p *messagePublisher) publish(ctx context.Context, subject string, msg any, msgType string, sync bool) error {
	if err := p.client.IsReady(ctx); err != nil {
		slog.ErrorContext(ctx, "NATS client not ready for publishing",
			"error", err, "subject", subject, "type", msgType)
		return errors.NewServiceUnavailable("NATS client not ready", err)
	}

	var data []byte
	if s, ok := msg.(string); ok {
		data = []byte(s)
	} else {
		var err error
		data, err = json.Marshal(msg)
		if err != nil {
			slog.ErrorContext(ctx, "failed to marshal message",
				"error", err, "subject", subject, "type", msgType)
			return errors.NewUnexpected("failed to marshal message", err)
		}
	}

	if sync {
		return p.request(ctx, subject, data, msgType)
	}
	return p.publishAsync(ctx, subject, data, msgType)
}

func (p *messagePublisher) publishAsync(ctx context.Context, subject string, data []byte, msgType string) error {
	if err := publishWithSpan(ctx, p.client.conn, subject, data); err != nil {
		slog.ErrorContext(ctx, "failed to publish message",
			"error", err, "subject", subject, "type", msgType)
		return errors.NewServiceUnavailable("failed to publish message", err)
	}
	slog.DebugContext(ctx, "message published",
		"subject", subject, "type", msgType, "size", len(data))
	return nil
}

func (p *messagePublisher) request(ctx context.Context, subject string, data []byte, msgType string) error {
	reqCtx, cancel := context.WithTimeout(ctx, defaultPublishTimeout)
	defer cancel()
	resp, err := requestWithSpan(reqCtx, p.client.conn, subject, data)
	if err != nil {
		slog.ErrorContext(ctx, "failed to send sync request",
			"error", err, "subject", subject, "type", msgType, "timeout", defaultPublishTimeout)
		return errors.NewServiceUnavailable("failed to send sync request", err)
	}
	slog.DebugContext(ctx, "sync request sent",
		"subject", subject, "type", msgType, "response_size", len(resp.Data))
	return nil
}
