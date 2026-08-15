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
}
