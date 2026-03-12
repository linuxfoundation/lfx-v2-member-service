// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// tableConsumer holds the JetStream consume context for a single per-table consumer.
type tableConsumer struct {
	// table is the table prefix (e.g. "salesforce_b2b-Account").
	table string

	// consumerName is the durable JetStream consumer name.
	consumerName string

	// consumeCtx is the active consume context; held so Stop() can drain it.
	consumeCtx jetstream.ConsumeContext
}

// Consumer subscribes to the v1-objects KV bucket (filtered to salesforce_b2b table keys)
// using one durable JetStream pull consumer per table and dispatches each entry to the
// appropriate type handler. Per-table consumers ensure that a NAK/backoff on one table
// (e.g. waiting for a missing FK dependency) does not delay processing of unrelated
// tables. It publishes denormalized indexer messages for project_products_b2b,
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

	// pgFallback provides optional PostgreSQL point-lookup queries for dependency
	// records (Account, Product2, Contact, Alternate_Email__c) that may not yet be
	// present in the v1-objects KV bucket during Meltano incremental backfills. When
	// nil, the resolver returns "not found" and the message is ACKed without indexing.
	pgFallback pgFallback

	// lookupSubject is the NATS RPC subject used to translate v1 project SFIDs to v2
	// project UIDs via the v1-sync-helper lookup handler.
	lookupSubject string

	// mu protects tableConsumers during concurrent access.
	mu sync.Mutex

	// tableConsumers holds one per-table JetStream consume context per table.
	tableConsumers []*tableConsumer
}

// Config holds the configuration for the b2b KV consumer.
type Config struct {
	// NATSConn is the established NATS connection. Required.
	NATSConn *nats.Conn

	// LookupSubject is the NATS RPC subject for v1→v2 project UID lookups.
	// Defaults to constants.V1MappingLookupSubject when empty.
	LookupSubject string

	// DB is an optional PostgreSQL connection used as a fallback for dependency
	// lookups when a record is not yet present in the v1-objects KV bucket. When
	// nil, missing KV records result in skipped (ACKed) messages. When set, the
	// consumer falls back to point-lookup queries against the salesforce_b2b schema.
	DB *sqlx.DB
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

	// Wire up the optional PostgreSQL fallback for dependency lookups.
	if cfg.DB != nil {
		c.pgFallback = newPGFallback(cfg.DB)
		slog.InfoContext(ctx, "b2b consumer: PostgreSQL fallback enabled for dependency resolvers")
	} else {
		slog.InfoContext(ctx, "b2b consumer: PostgreSQL fallback not configured — missing KV records will be skipped")
	}

	return c, nil
}

// Run creates (or re-attaches to) one durable JetStream pull consumer per salesforce_b2b
// table and starts consuming messages on each. Per-table consumers ensure that a NAK
// backoff on one table does not block processing of other tables.
//
// If Reload was called beforehand, the durable consumers already exist and are caught up;
// CreateOrUpdateConsumer re-attaches to them without re-delivering any messages. If this
// is a fresh start, new consumers are created with DeliverLastPerSubjectPolicy so that
// only the latest KV revision of each key is delivered.
//
// Run blocks until ctx is cancelled or a fatal consumer error occurs. It is intended to
// be called in a dedicated goroutine.
func (c *Consumer) Run(ctx context.Context) error {
	if err := c.startTableConsumers(ctx); err != nil {
		return err
	}

	tableNames := make([]string, 0, len(c.tableConsumers))
	for _, tc := range c.tableConsumers {
		tableNames = append(tableNames, tc.table)
	}
	slog.InfoContext(ctx, "b2b consumer: all per-table consumers started",
		"stream", constants.B2BConsumerStreamName,
		"tables", tableNames,
	)

	// Block until the context is cancelled.
	<-ctx.Done()

	slog.InfoContext(ctx, "b2b consumer: context cancelled, stopping")

	return nil
}

// Reload destroys all existing per-table JetStream consumers (clearing their ACK state)
// and recreates them with DeliverLastPerSubjectPolicy. Because the ACK state is reset,
// NATS re-delivers the latest value for every KV key — effectively a full re-index.
//
// Tables are processed in dependency-group order so that FK references (e.g. Project__c,
// Account, Product2) are satisfied before the tables that depend on them (Asset,
// Project_Role__c). Each dependency group is drained (caught up) before the next group
// starts.
//
// The dependency order is defined by constants.B2BReloadTableGroups:
//
//	Group 0: Project__c
//	Group 1: Account, Product2
//	Group 2: Asset
//	Group 3: Contact, Alternate_Email__c
//	Group 4: Project_Role__c
//
// After reload completes, the durable consumers are left in place and fully caught up.
// The subsequent call to Run re-attaches to them via CreateOrUpdateConsumer without
// re-delivering any messages.
func (c *Consumer) Reload(ctx context.Context) error {
	slog.InfoContext(ctx, "b2b consumer: reload starting — destroying and recreating all consumers in dependency order")

	// Stop any active consume contexts and delete all durable consumers to reset ACK state.
	c.stopTableConsumers()
	if err := c.deleteAllJetStreamConsumers(ctx); err != nil {
		return fmt.Errorf("b2b consumer reload: failed to delete existing consumers: %w", err)
	}

	// Process each dependency group sequentially.
	for groupIdx, group := range constants.B2BReloadTableGroups {
		slog.InfoContext(ctx, "b2b consumer: reload processing dependency group",
			"group", groupIdx,
			"tables", group,
		)

		if err := c.runReloadGroup(ctx, group); err != nil {
			return fmt.Errorf("b2b consumer reload: group %d failed: %w", groupIdx, err)
		}

		slog.InfoContext(ctx, "b2b consumer: reload dependency group complete",
			"group", groupIdx,
			"tables", group,
		)
	}

	// Stop the consume contexts used during reload but leave the durable consumers on
	// the server. They are fully caught up, so Run will re-attach without re-delivery.
	c.stopTableConsumers()

	slog.InfoContext(ctx, "b2b consumer: reload complete — consumers are caught up and ready for Run")

	return nil
}

// Stop drains all active per-table JetStream consume contexts, allowing in-flight
// message processing to complete before the connection is closed. It is safe to call
// Stop multiple times or when Run has not yet been called.
func (c *Consumer) Stop() {
	c.stopTableConsumers()
}

// startTableConsumers creates (or re-attaches to) one durable JetStream pull consumer per
// table defined in constants.B2BTableConsumerNames. DeliverLastPerSubjectPolicy is always
// used: on a fresh consumer it delivers only the latest KV revision per key; on an
// existing caught-up consumer (e.g. after Reload) it is a no-op since there are no
// unacknowledged messages.
func (c *Consumer) startTableConsumers(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for table, consumerName := range constants.B2BTableConsumerNames {
		filterSubject := constants.B2BTableFilterSubject(table)

		consumer, err := c.js.CreateOrUpdateConsumer(ctx, constants.B2BConsumerStreamName, jetstream.ConsumerConfig{
			Name:           consumerName,
			Durable:        consumerName,
			DeliverPolicy:  jetstream.DeliverLastPerSubjectPolicy,
			AckPolicy:      jetstream.AckExplicitPolicy,
			FilterSubjects: []string{filterSubject},
			MaxDeliver:     3,
			AckWait:        30 * time.Second,
			MaxAckPending:  1000,
			Description:    fmt.Sprintf("per-table b2b KV consumer for %s", table),
		})
		if err != nil {
			slog.ErrorContext(ctx, "b2b consumer: failed to create or update per-table JetStream consumer",
				"consumer", consumerName,
				"table", table,
				"stream", constants.B2BConsumerStreamName,
				"error", err,
			)
			return err
		}

		consumeCtx, err := consumer.Consume(c.handleKVMessage, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, consumeErr error) {
			slog.ErrorContext(ctx, "b2b consumer: JetStream consume error",
				"consumer", consumerName,
				"table", table,
				"error", consumeErr,
			)
		}))
		if err != nil {
			slog.ErrorContext(ctx, "b2b consumer: failed to start consuming for table",
				"consumer", consumerName,
				"table", table,
				"error", err,
			)
			return err
		}

		c.tableConsumers = append(c.tableConsumers, &tableConsumer{
			table:        table,
			consumerName: consumerName,
			consumeCtx:   consumeCtx,
		})

		slog.InfoContext(ctx, "b2b consumer: started per-table consumer",
			"consumer", consumerName,
			"table", table,
			"filter", filterSubject,
		)
	}

	return nil
}

// stopTableConsumers drains all active per-table consume contexts and clears the slice.
// The durable consumers remain on the JetStream server with their ACK state intact.
func (c *Consumer) stopTableConsumers() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, tc := range c.tableConsumers {
		if tc.consumeCtx != nil {
			tc.consumeCtx.Drain()
		}
	}

	c.tableConsumers = nil
}

// deleteAllJetStreamConsumers removes all per-table durable consumers from the JetStream
// server, resetting their ACK state. This is used during reload to force re-delivery of
// the latest value for every KV key.
func (c *Consumer) deleteAllJetStreamConsumers(ctx context.Context) error {
	for table, consumerName := range constants.B2BTableConsumerNames {
		err := c.js.DeleteConsumer(ctx, constants.B2BConsumerStreamName, consumerName)
		if err != nil {
			// Ignore "consumer not found" errors — the consumer may not exist yet.
			slog.DebugContext(ctx, "b2b consumer: delete consumer result (may be expected on first run)",
				"consumer", consumerName,
				"table", table,
				"error", err,
			)
		}
	}
	return nil
}

// runReloadGroup creates per-table consumers for the given tables with
// DeliverLastPerSubjectPolicy, processes all messages until each consumer is caught up
// (no pending messages), then stops the consume contexts but leaves the durable consumers
// on the server. Tables within a group are consumed concurrently; the method blocks until
// all tables in the group are drained.
func (c *Consumer) runReloadGroup(ctx context.Context, tables []string) error {
	type groupConsumer struct {
		table        string
		consumerName string
		jsConsumer   jetstream.Consumer
		consumeCtx   jetstream.ConsumeContext
	}

	consumers := make([]*groupConsumer, 0, len(tables))

	// Create and start consumers for each table in the group.
	for _, table := range tables {
		consumerName, ok := constants.B2BTableConsumerNames[table]
		if !ok {
			return fmt.Errorf("unknown table %q in reload group", table)
		}

		filterSubject := constants.B2BTableFilterSubject(table)

		jsConsumer, err := c.js.CreateOrUpdateConsumer(ctx, constants.B2BConsumerStreamName, jetstream.ConsumerConfig{
			Name:           consumerName,
			Durable:        consumerName,
			DeliverPolicy:  jetstream.DeliverLastPerSubjectPolicy,
			AckPolicy:      jetstream.AckExplicitPolicy,
			FilterSubjects: []string{filterSubject},
			MaxDeliver:     3,
			AckWait:        30 * time.Second,
			MaxAckPending:  1000,
			Description:    fmt.Sprintf("reload b2b KV consumer for %s", table),
		})
		if err != nil {
			return fmt.Errorf("failed to create reload consumer for %s: %w", table, err)
		}

		consumeCtx, err := jsConsumer.Consume(c.handleKVMessage, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, consumeErr error) {
			slog.ErrorContext(ctx, "b2b consumer: reload consume error",
				"consumer", consumerName,
				"table", table,
				"error", consumeErr,
			)
		}))
		if err != nil {
			return fmt.Errorf("failed to start reload consumer for %s: %w", table, err)
		}

		consumers = append(consumers, &groupConsumer{
			table:        table,
			consumerName: consumerName,
			jsConsumer:   jsConsumer,
			consumeCtx:   consumeCtx,
		})

		slog.InfoContext(ctx, "b2b consumer: reload consumer started for table",
			"consumer", consumerName,
			"table", table,
		)
	}

	// Poll until all consumers in this group have no pending messages, or ctx is cancelled.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Stop consume contexts but leave durable consumers on the server.
			for _, gc := range consumers {
				gc.consumeCtx.Drain()
			}
			return ctx.Err()
		case <-ticker.C:
			allCaughtUp := true
			for _, gc := range consumers {
				info, err := gc.jsConsumer.Info(ctx)
				if err != nil {
					slog.WarnContext(ctx, "b2b consumer: failed to get reload consumer info",
						"consumer", gc.consumerName,
						"table", gc.table,
						"error", err,
					)
					allCaughtUp = false
					continue
				}

				pending := info.NumPending + uint64(info.NumAckPending)
				if pending > 0 {
					slog.DebugContext(ctx, "b2b consumer: reload group table still processing",
						"table", gc.table,
						"num_pending", info.NumPending,
						"num_ack_pending", info.NumAckPending,
					)
					allCaughtUp = false
				}
			}

			if allCaughtUp {
				slog.InfoContext(ctx, "b2b consumer: reload group fully caught up",
					"tables", tables,
				)
				// Stop consume contexts but leave durable consumers on the server
				// so the next group (or Run) can re-attach without re-delivery.
				for _, gc := range consumers {
					gc.consumeCtx.Drain()
				}
				return nil
			}
		}
	}
}
