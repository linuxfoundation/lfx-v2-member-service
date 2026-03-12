// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
)

// indexer publishes upsert and delete messages to the LFX indexer over core NATS.
type indexer struct {
	conn *nats.Conn
}

// newIndexer creates an indexer backed by the given NATS connection.
func newIndexer(conn *nats.Conn) *indexer {
	return &indexer{conn: conn}
}

// publishUpsert sends a document upsert message to the indexer on the given subject.
// The data argument is the typed indexed document struct (e.g. IndexedProjectMembershipTier).
// cfg is the pre-computed indexing_config that bypasses server-side enrichers.
func (idx *indexer) publishUpsert(ctx context.Context, subject string, data any, cfg *IndexingConfig) error {
	return idx.publish(ctx, subject, MessageActionUpserted, data, cfg)
}

// publishDelete sends a document delete message to the indexer on the given subject.
// uid is the v2 UID of the document to remove from the index.
func (idx *indexer) publishDelete(ctx context.Context, subject, uid string) error {
	return idx.publish(ctx, subject, MessageActionDeleted, DeleteRequest{UID: uid}, nil)
}

// publish marshals and sends an IndexerMessage to the given NATS subject.
func (idx *indexer) publish(ctx context.Context, subject string, action MessageAction, data any, cfg *IndexingConfig) error {
	msg := IndexerMessage{
		Action:         action,
		Headers:        systemHeaders(),
		Data:           data,
		Tags:           []string{},
		IndexingConfig: cfg,
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("indexer marshal for subject %q: %w", subject, err)
	}

	if err := idx.conn.Publish(subject, payload); err != nil {
		return fmt.Errorf("indexer publish to subject %q: %w", subject, err)
	}

	slog.DebugContext(ctx, "published indexer message",
		"subject", subject,
		"action", action,
	)

	return nil
}

// systemHeaders returns the minimal set of headers included with indexer messages
// originating from a system-generated (non-user) event path. Currently empty because
// the indexer expects a Heimdall-signed authorization header, which this service does
// not have when triggered by incoming v1-objects data.
func systemHeaders() map[string]string {
	return map[string]string{}
}
