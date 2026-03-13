// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSClient wraps the NATS connection and provides KV store access
type NATSClient struct {
	conn        *nats.Conn
	config      Config
	kvStore     map[string]jetstream.KeyValue
	v1ObjectsKV jetstream.KeyValue
	timeout     time.Duration
}

// Conn returns the underlying *nats.Conn for callers that need direct NATS access,
// such as the project resolver which uses RPC requests over core NATS.
func (c *NATSClient) Conn() *nats.Conn {
	return c.conn
}

// V1ObjectsKV returns the handle to the v1-objects KV bucket, used by the storage
// layer to resolve v2 project UIDs to B2B Salesforce project SFIDs for inbound
// filter translation. Returns nil if the bucket was not opened successfully.
func (c *NATSClient) V1ObjectsKV() jetstream.KeyValue {
	return c.v1ObjectsKV
}

// Close gracefully closes the NATS connection.
func (c *NATSClient) Close() error {
	if c.conn != nil {
		c.conn.Close()
	}
	return nil
}

// IsReady checks if the NATS client is ready
func (c *NATSClient) IsReady(ctx context.Context) error {
	if c.conn == nil {
		return errors.NewServiceUnavailable("NATS client is not initialized or not connected")
	}
	if !c.conn.IsConnected() || c.conn.IsDraining() {
		return errors.NewServiceUnavailable("NATS client is not ready, connection is not established or is draining")
	}
	return nil
}

// KeyValueStore creates a JetStream client and gets the key-value store for the given bucket.
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
		kvStore, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket: bucketName,
		})
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

// NewClient creates a new NATS client with the given configuration
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
			slog.InfoContext(ctx, "NATS connection closed")
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

	for _, bucketName := range []string{
		constants.KVBucketNameMemberships,
		constants.KVBucketNameMembershipContacts,
		constants.KVBucketNameMembers,
	} {
		if err := client.KeyValueStore(ctx, bucketName); err != nil {
			slog.ErrorContext(ctx, "failed to initialize NATS key-value store",
				"error", err,
				"bucket", bucketName,
			)
			return nil, errors.NewServiceUnavailable("failed to initialize NATS key-value store", err)
		}
		slog.InfoContext(ctx, "NATS key-value store initialized",
			"bucket", bucketName,
		)
	}

	// Open the v1-objects KV bucket for read-only access. This is used by the storage
	// layer to translate v2 project UIDs to B2B Salesforce SFIDs for inbound filter
	// resolution. A failure here is non-fatal — the storage layer degrades gracefully
	// by treating the project_id filter value as a raw SFID when the bucket is absent.
	js, jsErr := jetstream.New(conn)
	if jsErr == nil {
		v1KV, kvErr := js.KeyValue(ctx, constants.V1ObjectsKVBucket)
		if kvErr != nil {
			slog.WarnContext(ctx, "v1-objects KV bucket not available; project_id UID translation will be skipped",
				"bucket", constants.V1ObjectsKVBucket,
				"error", kvErr,
			)
		} else {
			client.v1ObjectsKV = v1KV
			slog.InfoContext(ctx, "v1-objects KV bucket opened for project UID resolution",
				"bucket", constants.V1ObjectsKVBucket,
			)
		}
	} else {
		slog.WarnContext(ctx, "failed to create JetStream context for v1-objects KV; project_id UID translation will be skipped",
			"error", jsErr,
		)
	}

	slog.InfoContext(ctx, "NATS client created successfully",
		"connected_url", conn.ConnectedUrl(),
		"status", conn.Status(),
	)

	return client, nil
}
