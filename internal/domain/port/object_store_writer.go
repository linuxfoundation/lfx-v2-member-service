// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// ObjectStoreWriter uploads binary objects (e.g. B2B org logos) to durable,
// publicly-fetchable object storage.
type ObjectStoreWriter interface {
	// Put uploads data at key with the given content type and returns a
	// publicly fetchable, versioned URL for the stored object. Implementations
	// must set a short (~24h) Cache-Control TTL on the object so that any copy
	// of the returned URL propagated elsewhere converges to current bytes
	// within a day, and must embed a cache-busting query parameter (not a path
	// segment or S3 VersionId) so a subsequent Put with the same key resolves
	// to new bytes immediately.
	Put(ctx context.Context, key string, contentType string, data []byte) (url string, err error)

	// VersionedURL returns the same publicly-fetchable, versioned URL shape as
	// Put, for key, without uploading anything. It lets a caller that must not
	// write to a shared key until it has won an external optimistic-concurrency
	// check (e.g. logoUploaderOrchestrator, see LFXV2-2016) obtain the URL it
	// will persist *before* the object at key reflects it — the write to key
	// itself must follow via Put once that check succeeds.
	VersionedURL(key string) string

	// Delete best-effort removes the object at key. Callers use it to clean up
	// scratch objects (e.g. a losing concurrent upload's temp key) and must not
	// treat a failure as fatal to the operation that requested it.
	Delete(ctx context.Context, key string) error

	// Copy server-side copies the object at srcKey to dstKey, preserving its
	// metadata (Content-Type, Cache-Control). It backs the scratch-to-shared-key
	// promotion in logoUploaderOrchestrator (LFXV2-2016): once an optimistic-
	// concurrency check has already committed dstKey's URL elsewhere (e.g. to
	// Salesforce), promoting via Copy — rather than re-uploading the original
	// bytes with Put — avoids re-running the caller's own upload path for a
	// step that must not fail silently into a durably broken reference.
	Copy(ctx context.Context, srcKey, dstKey string) error
}
