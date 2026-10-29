// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package pubsub implements the CDCSubscriber port using Salesforce Pub/Sub
// gRPC API v1. It owns all gRPC, proto, and Avro types; nothing from those
// layers crosses into the domain.
package pubsub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	pbproto "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce/pubsub/proto"
)

const (
	// defaultBatchSize is the number of events requested per FetchRequest.
	// Salesforce recommends 100 as a reasonable upper bound.
	defaultBatchSize = 100

	// reconnectDelay is the backoff between Subscribe stream reconnects.
	reconnectDelay = 5 * time.Second

	// replayIDCorruptedErrorCode is the Pub/Sub error-code trailer for a replay
	// ID Salesforce cannot serve, most often one outside the 72-hour retention
	// window. Retrying the same cursor can never succeed.
	replayIDCorruptedErrorCode = "sfdc.platform.eventbus.grpc.subscription.fetch.replayid.corrupted"

	// streamDownThreshold is the number of consecutive reconnects with no
	// response (about a minute at reconnectDelay) after which reconnect logs
	// escalate from WARN to ERROR so a persistent outage is not silent.
	streamDownThreshold = 12

	// reindexLookback is subtracted from last_saved_at to get a safe reindex
	// since. Salesforce delivers an event at most 72 hours after its change,
	// and the cursor is saved after delivery; the extra hour covers the wait
	// in the event buffer. An event held longer, by an authorization retry,
	// needs an earlier since (see docs/cdc-consumer.md).
	reindexLookback = 73 * time.Hour
)

var errReplayCursorExpired = errors.New("pubsub: replay cursor expired or invalid")

// isReplayCursorExpired matches the status code and the error-code trailer,
// not the message text, which Salesforce does not document as stable.
func isReplayCursorExpired(err error, trailer metadata.MD) bool {
	if status.Code(err) != codes.InvalidArgument {
		return false
	}
	return slices.Contains(trailer.Get("error-code"), replayIDCorruptedErrorCode)
}

// Ensure Client satisfies port.CDCSubscriber at compile time.
var _ port.CDCSubscriber = (*Client)(nil)

// tokenProvider is a function that returns the current Salesforce access token
// and instance URL. Called once per gRPC stream to refresh credentials.
type tokenProvider func() (accessToken, instanceURL, orgID string)

// Client is the CDCSubscriber adapter. It wraps the Salesforce Pub/Sub gRPC
// service, handles Avro schema caching, and emits normalized CDCEvents.
// Implements port.CDCSubscriber.
type Client struct {
	grpcConn    *grpc.ClientConn
	stub        pbproto.PubSubClient
	tokenFn     tokenProvider
	schemaCache sync.Map // map[schemaID string]string       — raw Avro JSON from GetSchema
	codecCache  sync.Map // map[schemaID string]*goavro.Codec — compiled codec, reused across events
	batchSize   int32

	reconnectDelay time.Duration
}

// NewClient dials the Salesforce Pub/Sub gRPC endpoint and returns a Client.
// The tokenFn is called each time a new stream is opened so that a refreshed
// access token is always used (Salesforce sessions expire).
//
// endpoint example: "api.pubsub.salesforce.com:7443"
func NewClient(endpoint string, tokenFn tokenProvider) (*Client, error) {
	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("pubsub: dial %s: %w", endpoint, err)
	}

	return &Client{
		grpcConn:  conn,
		stub:      pbproto.NewPubSubClient(conn),
		tokenFn:   tokenFn,
		batchSize: defaultBatchSize,

		reconnectDelay: reconnectDelay,
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.grpcConn.Close()
}

// Subscribe implements port.CDCSubscriber. It opens a bidirectional gRPC
// stream, requests events in batches, decodes Avro payloads, and emits
// CDCEvents on the returned channel.
//
// On transient errors (network blip, stream EOF) it reconnects automatically
// with a short delay. Before each reconnect attempt it reloads the cursor from
// replayStore.Load — this is the same store the consumer commits to after each
// event. Reloading on reconnect ensures that if an event was delivered to the
// channel but the consumer had not yet committed it before the disconnect, it
// is re-delivered rather than silently skipped.
//
// replayID nil means start at LATEST (skip historical events). After an
// expired cursor is reset, the store holds replayFromEarliest, so streams
// start at EARLIEST until a cursor is saved again, across restarts too.
func (c *Client) Subscribe(ctx context.Context, channel string, replayID []byte, replayStore port.ReplayStore) (<-chan model.CDCEvent, error) {
	out := make(chan model.CDCEvent, defaultBatchSize)

	go func() {
		defer close(out)
		first := true
		failures := 0
		for {
			// On first attempt use the caller-supplied cursor (which was loaded
			// from the store by CDCConsumer.Run before calling Subscribe).
			// On reconnects reload from the store so we resume from the last
			// committed position, not from the last in-flight delivery.
			var (
				current []byte
				err     error
			)
			if first {
				current = replayID
				first = false
			} else {
				current, err = replayStore.Load(ctx, channel)
				if err != nil {
					slog.ErrorContext(ctx, "pubsub: failed to reload replay cursor, stopping",
						"channel", channel, "error", err)
					return
				}
			}

			received, streamErr := c.runStream(ctx, channel, current, out)
			if streamErr == nil || ctx.Err() != nil {
				return
			}
			if errors.Is(streamErr, errReplayCursorExpired) && c.resetExpiredCursor(ctx, channel, current, streamErr, replayStore) {
				continue
			}
			if received {
				failures = 0
			} else {
				failures++
			}
			logReconnect(ctx, channel, streamErr, failures, c.reconnectDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.reconnectDelay):
			}
		}
	}()

	return out, nil
}

// resetExpiredCursor replaces a cursor Salesforce rejected so the next
// attempt replays from EARLIEST. Events older than the retention window are
// already gone, so they are skipped; the ERROR tells the operator which
// window to reindex. Returns true when the caller should reconnect now: after
// a reset, or when the consumer saved a newer cursor that is worth trying.
// Returns false when the reset failed, and the caller backs off and retries.
func (c *Client) resetExpiredCursor(ctx context.Context, channel string, rejected []byte, streamErr error, replayStore port.ReplayStore) bool {
	lastSavedAt, err := replayStore.Reset(ctx, channel, rejected)
	if errors.Is(err, port.ErrReplayCursorChanged) {
		slog.WarnContext(ctx, "pubsub: replay cursor rejected but a newer one was saved, reconnecting with it",
			"channel", channel, "error", streamErr)
		return true
	}
	if err != nil {
		slog.ErrorContext(ctx, "pubsub: failed to reset expired replay cursor, retrying",
			"channel", channel, "error", err)
		return false
	}

	savedAt, reindexSince := "unknown", "full"
	if !lastSavedAt.IsZero() {
		savedAt = lastSavedAt.UTC().Format(time.RFC3339)
		reindexSince = lastSavedAt.Add(-reindexLookback).UTC().Format(time.RFC3339)
	}
	slog.ErrorContext(ctx, "pubsub: replay cursor expired, replaying from EARLIEST; "+
		"events past Salesforce retention were skipped, run POST /admin/reindex with since=reindex_since "+
		"(a full reindex when reindex_since is full; neither removes records deleted in that window, "+
		"see docs/cdc-consumer.md)",
		"channel", channel,
		"error", streamErr,
		"last_saved_at", savedAt,
		"reindex_since", reindexSince,
		"cdc_replay_cursor_expired", true,
	)
	return true
}

func logReconnect(ctx context.Context, channel string, streamErr error, failures int, delay time.Duration) {
	if failures >= streamDownThreshold {
		slog.ErrorContext(ctx, "pubsub: stream has not recovered, reconnecting",
			"channel", channel,
			"error", streamErr,
			"consecutive_failures", failures,
			"delay", delay,
			"cdc_stream_down", true,
		)
		return
	}
	slog.WarnContext(ctx, "pubsub: stream ended, reconnecting",
		"channel", channel,
		"error", streamErr,
		"delay", delay,
	)
}

// runStream opens one Subscribe stream and forwards events to out. The bool
// reports whether at least one response (events or keepalive) arrived. Returns
// a nil error only when ctx is cancelled; any other failure is reconnectable.
// Subscribe reloads the cursor from the persistent store before each reconnect,
// so there is no need to track the in-flight cursor here.
func (c *Client) runStream(
	ctx context.Context,
	channel string,
	replayID []byte,
	out chan<- model.CDCEvent,
) (bool, error) {
	accessToken, instanceURL, orgID := c.tokenFn()

	// Inject Salesforce auth metadata required by the gRPC server.
	md := metadata.Pairs(
		"accesstoken", accessToken,
		"instanceurl", instanceURL,
		"tenantid", orgID,
	)
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := c.stub.Subscribe(streamCtx)
	if err != nil {
		return false, fmt.Errorf("pubsub: open stream: %w", err)
	}

	// Send initial FetchRequest.
	req := &pbproto.FetchRequest{
		TopicName:    channel,
		NumRequested: c.batchSize,
	}
	switch {
	case bytes.Equal(replayID, replayFromEarliest):
		req.ReplayPreset = pbproto.ReplayPreset_EARLIEST
	case len(replayID) > 0:
		req.ReplayPreset = pbproto.ReplayPreset_CUSTOM
		req.ReplayId = replayID
	default:
		req.ReplayPreset = pbproto.ReplayPreset_LATEST
	}

	if err := stream.Send(req); err != nil {
		return false, fmt.Errorf("pubsub: send fetch request: %w", err)
	}

	received := false
	for {
		resp, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return received, nil
			}
			if isReplayCursorExpired(err, stream.Trailer()) {
				return received, fmt.Errorf("%w: %w", errReplayCursorExpired, err)
			}
			return received, fmt.Errorf("pubsub: recv: %w", err)
		}
		received = true

		for _, event := range resp.GetEvents() {
			cdcEvent, decErr := c.decodeEvent(ctx, event)
			if decErr != nil {
				slog.ErrorContext(ctx, "pubsub: decode event, skipping",
					"schema_id", event.GetEvent().GetSchemaId(),
					"error", decErr,
				)
				continue
			}

			select {
			case <-ctx.Done():
				return received, nil
			case out <- cdcEvent:
			}
		}

		// Salesforce sends an empty keepalive within 270s when no events are
		// available. Its cursor goes through out, not straight to the store, so
		// it is saved only after every earlier event has been processed.
		if len(resp.GetEvents()) == 0 && len(resp.GetLatestReplayId()) > 0 {
			select {
			case <-ctx.Done():
				return received, nil
			case out <- model.CDCEvent{ReplayID: resp.GetLatestReplayId(), CursorOnly: true}:
			}
		}

		// Request next batch to keep the stream flowing.
		if err := stream.Send(&pbproto.FetchRequest{
			TopicName:    channel,
			NumRequested: c.batchSize,
		}); err != nil {
			if ctx.Err() != nil {
				return received, nil
			}
			return received, fmt.Errorf("pubsub: send next fetch: %w", err)
		}
	}
}

// fetchSchema retrieves and caches the Avro schema for the given schema ID.
// Cached per the Salesforce recommendation: schemas change rarely; only re-fetch
// when a new schema_id appears in a FetchResponse.
func (c *Client) fetchSchema(ctx context.Context, schemaID string) (string, error) {
	if v, ok := c.schemaCache.Load(schemaID); ok {
		return v.(string), nil
	}

	accessToken, instanceURL, orgID := c.tokenFn()
	md := metadata.Pairs(
		"accesstoken", accessToken,
		"instanceurl", instanceURL,
		"tenantid", orgID,
	)
	schemaCtx := metadata.NewOutgoingContext(ctx, md)

	resp, err := c.stub.GetSchema(schemaCtx, &pbproto.SchemaRequest{SchemaId: schemaID})
	if err != nil {
		return "", fmt.Errorf("pubsub: GetSchema %s: %w", schemaID, err)
	}

	schemaJSON := resp.GetSchemaJson()
	c.schemaCache.Store(schemaID, schemaJSON)

	slog.DebugContext(ctx, "pubsub: schema cached", "schema_id", schemaID)
	return schemaJSON, nil
}
