// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSClient wraps the NATS connection and provides KV store access.
type NATSClient struct {
	conn    *nats.Conn
	config  Config
	kvStore map[string]jetstream.KeyValue
	timeout time.Duration
}

// Conn returns the underlying *nats.Conn for callers that need direct
// NATS request/reply access (e.g., the project resolver).
func (c *NATSClient) Conn() *nats.Conn {
	return c.conn
}

// PubSubStateKV returns the jetstream.KeyValue handle for the pubsub-state
// bucket. Must be called after KeyValueStore(ctx, constants.KVBucketNamePubSubState).
// Returns nil if the bucket has not been initialized (API mode does not init it).
func (c *NATSClient) PubSubStateKV() jetstream.KeyValue {
	if c.kvStore == nil {
		return nil
	}
	return c.kvStore[constants.KVBucketNamePubSubState]
}

// Close gracefully closes the NATS connection.
func (c *NATSClient) Close() error {
	if c.conn != nil {
		c.conn.Close()
	}
	return nil
}

// IsReady checks if the NATS client is ready.
func (c *NATSClient) IsReady(ctx context.Context) error {
	if c.conn == nil {
		return errors.NewServiceUnavailable("NATS client is not initialized or not connected")
	}
	if !c.conn.IsConnected() || c.conn.IsDraining() {
		return errors.NewServiceUnavailable("NATS client is not ready, connection is not established or is draining")
	}
	return nil
}

// KeyValueStore creates a JetStream client and gets the key-value store for the
// given bucket. When creating a new bucket, membership-cache is given a 24-hour
// MaxAge TTL (aligned with its per-entry soft-TTL envelopes) and
// member-service-cache is given a 7-day TTL (entries are revalidated via HTTP
// conditional GET on every fetch and their TTL is reset on each 304, so a
// longer backstop reduces cold fetches after quiet periods). If the bucket
// already exists, the existing configuration is used as-is — NATS does not
// allow in-place TTL updates on live buckets; the Helm chart is responsible for
// pre-creating it with the correct config.
func (c *NATSClient) KeyValueStore(ctx context.Context, bucketName string) error {
	js, err := jetstream.New(c.conn)
	if err != nil {
		slog.ErrorContext(ctx, "error creating NATS JetStream client",
			"error", err,
			"nats_url", c.conn.ConnectedUrl(),
		)
		return err
	}
	kvStore, err := js.KeyValue(ctx, bucketName)
	if err != nil {
		slog.InfoContext(ctx, "KV bucket not found, creating it",
			"bucket", bucketName,
		)
		cfg := jetstream.KeyValueConfig{
			Bucket: bucketName,
		}
		// Cache buckets use different hard-eviction TTLs:
		//   membership-cache: 24h — soft-TTL envelopes (6h stale / 23h expire) live
		//     inside each entry, so 24h is a tight backstop aligned with those values.
		//   member-service-cache: 7 days — freshness is governed by HTTP conditional
		//     GET (ETag / Last-Modified); entries are revalidated on every fetch and
		//     their TTL is reset on each 304, so a longer backstop means fewer cold
		//     fetches after quiet periods.
		//   org-settings: no TTL — authoritative state, never silently evicted.
		//   org-workspaces: no TTL — authoritative state, never silently evicted.
		//   org_workspace_projects: no TTL — authoritative state, never silently evicted.
		//   pubsub-state: no TTL — replay cursors must survive indefinitely; a
		//     silent eviction would cause a gap (fallback to LATEST, missing events).
		if bucketName == constants.KVBucketNameCache {
			cfg.TTL = 24 * time.Hour
		}
		if bucketName == constants.KVBucketNameSObjectCache {
			cfg.TTL = 7 * 24 * time.Hour
		}
		kvStore, err = js.CreateKeyValue(ctx, cfg)
		if err != nil {
			slog.ErrorContext(ctx, "error creating NATS JetStream key-value store",
				"error", err,
				"nats_url", c.conn.ConnectedUrl(),
				"bucket", bucketName,
			)
			return err
		}
	}

	if c.kvStore == nil {
		c.kvStore = make(map[string]jetstream.KeyValue)
	}
	c.kvStore[bucketName] = kvStore
	return nil
}

// NewClient creates a new NATS client with the given configuration.
func NewClient(ctx context.Context, config Config) (*NATSClient, error) {
	slog.InfoContext(ctx, "creating NATS client",
		"url", config.URL,
		"timeout", config.Timeout,
	)

	if config.URL == "" {
		return nil, errors.NewUnexpected("NATS URL is required")
	}

	opts := []nats.Option{
		nats.Name(constants.ServiceName),
		nats.Timeout(config.Timeout),
		nats.MaxReconnects(config.MaxReconnect),
		nats.ReconnectWait(config.ReconnectWait),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			slog.WarnContext(ctx, "NATS disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.InfoContext(ctx, "NATS reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ErrorHandler(func(_ *nats.Conn, s *nats.Subscription, err error) {
			if s != nil {
				slog.With("error", err, "subject", s.Subject, "queue", s.Queue).Error("async NATS error")
			} else {
				slog.With("error", err).Error("async NATS error outside subscription")
			}
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			// If the context is already done, this is an intentional shutdown;
			// do nothing.
			if ctx.Err() != nil {
				slog.InfoContext(ctx, "NATS connection closed during shutdown")
				return
			}
			slog.Error("NATS max-reconnects exhausted; connection permanently closed")
			if config.OnClosed != nil {
				config.OnClosed()
			} else {
				// Default behaviour: give the logger a moment to flush, then exit
				// so Kubernetes restarts the pod rather than leaving it running
				// with a dead NATS connection.
				time.Sleep(5 * time.Second)
				os.Exit(1)
			}
		}),
	}

	conn, err := nats.Connect(config.URL, opts...)
	if err != nil {
		return nil, errors.NewServiceUnavailable("failed to connect to NATS", err)
	}

	client := &NATSClient{
		conn:    conn,
		config:  config,
		timeout: config.Timeout,
	}

	if err := client.KeyValueStore(ctx, constants.KVBucketNameCache); err != nil {
		slog.ErrorContext(ctx, "failed to initialize NATS key-value store",
			"error", err,
			"bucket", constants.KVBucketNameCache,
		)
		return nil, errors.NewServiceUnavailable("failed to initialize NATS key-value store", err)
	}
	slog.InfoContext(ctx, "NATS key-value store initialized",
		"bucket", constants.KVBucketNameCache,
	)

	if err := client.KeyValueStore(ctx, constants.KVBucketNameSObjectCache); err != nil {
		slog.ErrorContext(ctx, "failed to initialize NATS sObject key-value store",
			"error", err,
			"bucket", constants.KVBucketNameSObjectCache,
		)
		return nil, errors.NewServiceUnavailable("failed to initialize NATS sObject key-value store", err)
	}
	slog.InfoContext(ctx, "NATS key-value store initialized",
		"bucket", constants.KVBucketNameSObjectCache,
	)

	if err := client.KeyValueStore(ctx, constants.KVBucketNameOrgSettings); err != nil {
		slog.ErrorContext(ctx, "failed to initialize org-settings key-value store",
			"error", err,
			"bucket", constants.KVBucketNameOrgSettings,
		)
		return nil, errors.NewServiceUnavailable("failed to initialize org-settings key-value store", err)
	}
	slog.InfoContext(ctx, "NATS key-value store initialized",
		"bucket", constants.KVBucketNameOrgSettings,
	)

	if err := client.KeyValueStore(ctx, constants.KVBucketNameOrgWorkspaces); err != nil {
		slog.ErrorContext(ctx, "failed to initialize org-workspaces key-value store",
			"error", err,
			"bucket", constants.KVBucketNameOrgWorkspaces,
		)
		return nil, errors.NewServiceUnavailable("failed to initialize org-workspaces key-value store", err)
	}
	slog.InfoContext(ctx, "NATS key-value store initialized",
		"bucket", constants.KVBucketNameOrgWorkspaces,
	)

	if err := client.KeyValueStore(ctx, constants.KVBucketNameWorkspaceProjects); err != nil {
		slog.ErrorContext(ctx, "failed to initialize org_workspace_projects key-value store",
			"error", err,
			"bucket", constants.KVBucketNameWorkspaceProjects,
		)
		return nil, errors.NewServiceUnavailable("failed to initialize org_workspace_projects key-value store", err)
	}
	slog.InfoContext(ctx, "NATS key-value store initialized",
		"bucket", constants.KVBucketNameWorkspaceProjects,
	)

	// cdc-repair: durable CDC quota-repair queue. No MaxAge TTL (a pending
	// marker must survive until drained) and history 1 (a pending set — old
	// revisions are never needed). Both defaults are provided by KeyValueStore
	// (no TTL branch) and jetstream.KeyValueConfig (History defaults to 1).
	// Initialised in both consumer and API modes: the consumer writes markers
	// on skip; the API lists/deletes them during a cdc_repair drain.
	if err := client.KeyValueStore(ctx, constants.KVBucketNameCDCRepair); err != nil {
		slog.ErrorContext(ctx, "failed to initialize cdc-repair key-value store",
			"error", err,
			"bucket", constants.KVBucketNameCDCRepair,
		)
		return nil, errors.NewServiceUnavailable("failed to initialize cdc-repair key-value store", err)
	}
	slog.InfoContext(ctx, "NATS key-value store initialized",
		"bucket", constants.KVBucketNameCDCRepair,
	)

	slog.InfoContext(ctx, "NATS client created successfully",
		"connected_url", conn.ConnectedUrl(),
		"status", conn.Status(),
	)

	return client, nil
}
