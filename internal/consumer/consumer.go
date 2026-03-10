// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Consumer subscribes to the v1-objects KV bucket (filtered to salesforce_b2b-* keys)
// using a durable JetStream pull consumer and dispatches each entry to the appropriate
// type handler. It publishes denormalized indexer messages for project_products_b2b,
// project_members_b2b, and key_contact resource types.
type Consumer struct {
	// natsConn is the underlying core NATS connection used for request-reply RPC
	// (v1-sync-helper project UID lookup) and indexer message publishing.
	natsConn *nats.Conn

	// js is the JetStream context derived from natsConn.
	js jetstream.JetStream

	// v1ObjectsKV is the handle to the v1-objects KV bucket used for resolving linked
	// Salesforce records (Account, Product2, Contact, Project__c, Asset).
	v1ObjectsKV jetstream.KeyValue

	// mapping maintains the forward-lookup indexes in the project-membership-mapping KV
	// bucket (account→assets, product2→assets, contact→project_roles, asset→project_roles).
	mapping *mappingStore

	// indexer publishes upsert/delete messages to the LFX indexer over core NATS.
	indexer *indexer

	// projectCache is an in-memory cache of resolved project info (UID, name, slug)
	// keyed by Salesforce project SFID, with a 10-minute TTL.
	projectCache *projectCache

	// lookupSubject is the NATS RPC subject used to translate v1 project SFIDs to v2
	// project UIDs via the v1-sync-helper lookup handler.
	lookupSubject string

	// consumerCtx is the active JetStream consume context; held so Stop() can drain it.
	consumerCtx jetstream.ConsumeContext
}

// Config holds the configuration for the b2b KV consumer.
type Config struct {
	// NATSConn is the established NATS connection. Required.
	NATSConn *nats.Conn

	// LookupSubject is the NATS RPC subject for v1→v2 project UID lookups.
	// Defaults to constants.V1MappingLookupSubject when empty.
	LookupSubject string
}

// New creates and initialises a Consumer, opening all required KV bucket handles.
// The caller must invoke Run to start message processing, and Stop to shut down cleanly.
func New(ctx context.Context, cfg Config) (*Consumer, error) {
	lookupSubject := cfg.LookupSubject
	if lookupSubject == "" {
		lookupSubject = constants.V1MappingLookupSubject
	}

	js, err := jetstream.New(cfg.NATSConn)
	if err != nil {
		return nil, err
	}

	// Open the v1-objects KV bucket for reading source records.
	v1ObjectsKV, err := js.KeyValue(ctx, constants.V1ObjectsKVBucket)
	if err != nil {
		slog.ErrorContext(ctx, "b2b consumer: failed to open v1-objects KV bucket",
			"bucket", constants.V1ObjectsKVBucket,
			"error", err,
		)
		return nil, err
	}

	// Open the project-membership-mapping KV bucket for forward-lookup indexes.
	// The bucket must already exist (created by the Helm chart); the app does not
	// attempt to create it, so that operators can control bucket settings via Helm values.
	mappingKV, err := js.KeyValue(ctx, constants.KVBucketNameB2BMapping)
	if err != nil {
		slog.ErrorContext(ctx, "b2b consumer: failed to open project-membership-mapping KV bucket (must be pre-created via Helm chart)",
			"bucket", constants.KVBucketNameB2BMapping,
			"error", err,
		)
		return nil, err
	}

	c := &Consumer{
		natsConn:      cfg.NATSConn,
		js:            js,
		v1ObjectsKV:   v1ObjectsKV,
		mapping:       newMappingStore(mappingKV),
		indexer:       newIndexer(cfg.NATSConn),
		projectCache:  newProjectCache(),
		lookupSubject: lookupSubject,
	}

	return c, nil
}

// Run creates (or re-attaches to) the durable b2b JetStream pull consumer and starts
// consuming messages. It blocks until ctx is cancelled or a fatal consumer error occurs.
// Run is intended to be called in a dedicated goroutine.
func (c *Consumer) Run(ctx context.Context) error {
	// DeliverLastPerSubject ensures we only consume the most recent revision of any
	// item in the KV bucket, even if the consumer is destroyed and recreated. This
	// avoids replaying stale intermediate versions on cold start.
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, constants.B2BConsumerStreamName, jetstream.ConsumerConfig{
		Name:          constants.B2BConsumerName,
		Durable:       constants.B2BConsumerName,
		DeliverPolicy: jetstream.DeliverLastPerSubjectPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: constants.B2BConsumerFilterSubject,
		MaxDeliver:    3,
		AckWait:       30 * time.Second,
		MaxAckPending: 1000,
		Description:   "durable b2b KV consumer for lfx-v2-member-service",
	})
	if err != nil {
		slog.ErrorContext(ctx, "b2b consumer: failed to create or update JetStream consumer",
			"consumer", constants.B2BConsumerName,
			"stream", constants.B2BConsumerStreamName,
			"error", err,
		)
		return err
	}

	consumeCtx, err := consumer.Consume(c.handleKVMessage, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		slog.ErrorContext(ctx, "b2b consumer: JetStream consume error",
			"error", err,
		)
	}))
	if err != nil {
		slog.ErrorContext(ctx, "b2b consumer: failed to start consuming",
			"consumer", constants.B2BConsumerName,
			"error", err,
		)
		return err
	}

	c.consumerCtx = consumeCtx

	slog.InfoContext(ctx, "b2b consumer: started",
		"consumer", constants.B2BConsumerName,
		"stream", constants.B2BConsumerStreamName,
		"filter", constants.B2BConsumerFilterSubject,
	)

	// Block until the context is cancelled.
	<-ctx.Done()

	slog.InfoContext(ctx, "b2b consumer: context cancelled, stopping")

	return nil
}

// Stop drains the active JetStream consume context, allowing in-flight message
// processing to complete before the connection is closed. It is safe to call
// Stop multiple times or when Run has not yet been called.
func (c *Consumer) Stop() {
	if c.consumerCtx != nil {
		c.consumerCtx.Drain()
		c.consumerCtx = nil
	}
}
