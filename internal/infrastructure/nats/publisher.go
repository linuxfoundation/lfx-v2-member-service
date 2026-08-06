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

const (
	defaultPublishTimeout = 10 * time.Second
	defaultFlushTimeout   = 10 * time.Second
)

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
//
// FGA messages never use request/reply. They go straight to the publish path so
// no FGA subject can reach p.request, which would otherwise mistake a broker
// storage acknowledgement for an fga-sync completion reply.
func (p *messagePublisher) Access(ctx context.Context, subject string, msg any) error {
	data, err := p.prepare(ctx, subject, msg, "access")
	if err != nil {
		return err
	}
	return p.publishAsync(ctx, subject, data, "access")
}

// Flush blocks until the server has processed everything published earlier on
// this connection. It confirms delivery to the broker, not downstream
// processing.
//
// FlushWithContext requires its context to carry a deadline and returns
// ErrNoDeadlineContext otherwise — an HTTP request context has no deadline
// unless middleware sets one, so Flush enforces its own bound rather than
// depending on that. The caller's context is still respected: WithTimeout
// takes whichever of the two deadlines is sooner.
func (p *messagePublisher) Flush(ctx context.Context) error {
	if err := p.client.IsReady(ctx); err != nil {
		slog.ErrorContext(ctx, "NATS client not ready for flush", "error", err)
		return errors.NewServiceUnavailable("NATS client not ready", err)
	}
	flushCtx, cancel := context.WithTimeout(ctx, defaultFlushTimeout)
	defer cancel()
	if err := p.client.conn.FlushWithContext(flushCtx); err != nil {
		slog.ErrorContext(ctx, "failed to flush NATS connection", "error", err, "timeout", defaultFlushTimeout)
		return errors.NewServiceUnavailable("failed to flush NATS connection", err)
	}
	return nil
}

func (p *messagePublisher) publish(ctx context.Context, subject string, msg any, msgType string, sync bool) error {
	data, err := p.prepare(ctx, subject, msg, msgType)
	if err != nil {
		return err
	}

	if sync {
		return p.request(ctx, subject, data, msgType)
	}
	return p.publishAsync(ctx, subject, data, msgType)
}

// prepare checks connection readiness and renders msg to bytes.
func (p *messagePublisher) prepare(ctx context.Context, subject string, msg any, msgType string) ([]byte, error) {
	if err := p.client.IsReady(ctx); err != nil {
		slog.ErrorContext(ctx, "NATS client not ready for publishing",
			"error", err, "subject", subject, "type", msgType)
		return nil, errors.NewServiceUnavailable("NATS client not ready", err)
	}

	if s, ok := msg.(string); ok {
		return []byte(s), nil
	}
	data, err := json.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal message",
			"error", err, "subject", subject, "type", msgType)
		return nil, errors.NewUnexpected("failed to marshal message", err)
	}
	return data, nil
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
