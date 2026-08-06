// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// MemberPublisher publishes domain events to the LFX event bus. It is used by
// write-path use cases to trigger downstream indexing and FGA synchronisation
// after successful mutations.
//
// Indexer accepts a sync flag: when true the call blocks until the remote
// acknowledges the message (used for synchronous write paths); when false the
// message is fire-and-forget. Write handlers pass sync=false so that publish
// failures never block or fail an HTTP response.
//
// Access has no such flag. FGA synchronisation is always asynchronous, because
// the fga-sync service consumes membership subjects through durable JetStream
// delivery that cannot answer a request inbox. A reply on those subjects comes
// from the broker on storage rather than from fga-sync on completion, so
// waiting for one would report a convergence that has not happened.
//
// Publish-failure policy:
//   - Creates and updates: swallow the error and log at warn with
//     publish_failed_for_backfill_repair=true so the /admin/reindex backfill
//     can recover the record.
//   - Deletes: propagate the error to the caller; a delete without FGA/index
//     cleanup leaves dangling permissions.
type MemberPublisher interface {
	// Indexer publishes an indexer message to the given NATS subject.
	// msg must be a pre-built, JSON-serialisable value (e.g. *model.MemberIndexerMessage);
	// the publisher marshals and forwards it as-is.
	Indexer(ctx context.Context, subject string, msg any, sync bool) error

	// Access publishes an FGA synchronisation message to the given NATS subject.
	// msg must be JSON-serialisable (typically a fgatypes.GenericFGAMessage).
	// A nil return means the message was handed to NATS, not that OpenFGA
	// converged. Callers needing proof of delivery must call Flush.
	Access(ctx context.Context, subject string, msg any) error

	// Flush blocks until the server has processed everything published earlier
	// on this connection, closing the window in which a crash could discard a
	// message still buffered locally. It confirms delivery only: it does not
	// wait for fga-sync to process the message or for OpenFGA to converge.
	Flush(ctx context.Context) error
}
