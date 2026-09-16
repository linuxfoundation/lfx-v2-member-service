// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"errors"
	"fmt"
)

// ErrStalePromotion is returned by ObjectStoreWriter.CopyIfNewer when dstKey
// already reflects a promotion with a generation at least as new as the one
// requested — either because a HeadObject pre-check saw it, or because a
// concurrent writer won the underlying conditional CopyObject. It means "some
// other attempt's bytes currently own dstKey", not "that attempt is the
// authoritative winner": generations are derived from a coarse, shared clock,
// so two attempts can legitimately carry the same one. Callers that hold an
// external arbiter for ownership (e.g. logoUploaderOrchestrator, for which the
// conditional Salesforce commit is the single serialization point) must
// consult it rather than treating this error as a definitive loss.
var ErrStalePromotion = errors.New("a newer promotion already committed to this key")

// StalePromotionError carries the generation currently stamped on dstKey so a
// caller that the arbiter confirms as the real winner can re-attempt strictly
// above it. It satisfies errors.Is(err, ErrStalePromotion).
type StalePromotionError struct {
	// ExistingGeneration is the generation stamped on dstKey that beat this
	// attempt. Zero when the loss came from a conditional-copy conflict rather
	// than an observed stamp.
	ExistingGeneration int64
}

func (e *StalePromotionError) Error() string {
	return fmt.Sprintf("%s (existing generation %d)", ErrStalePromotion, e.ExistingGeneration)
}

func (e *StalePromotionError) Is(target error) bool { return target == ErrStalePromotion }

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
	// will persist *before* the object at key reflects it. Once that check
	// succeeds, key must be made to hold those bytes — for the shared logo key
	// that is CopyIfNewer promoting an already-written scratch object, which
	// preserves the ordering guarantees described there; a direct Put to key is
	// only safe where no concurrent writer can target it.
	VersionedURL(key string) string

	// Delete best-effort removes the object at key. Callers use it to clean up
	// scratch objects (e.g. a losing concurrent upload's temp key) and must not
	// treat a failure as fatal to the operation that requested it.
	Delete(ctx context.Context, key string) error

	// CopyIfNewer server-side copies the object at srcKey to dstKey, preserving
	// its metadata (Content-Type, Cache-Control), but only if dstKey does not
	// already reflect a promotion at least as new as generation (a caller-
	// assigned, monotonically increasing per-attempt token, e.g. a nanosecond
	// timestamp). It backs the scratch-to-shared-key promotion in
	// logoUploaderOrchestrator (LFXV2-2016): once an optimistic-concurrency
	// check has already committed dstKey's URL elsewhere (e.g. to Salesforce),
	// promoting via CopyIfNewer — rather than re-uploading the original bytes
	// with Put, or an unconditional Copy — closes the remaining race where a
	// slower, older promotion attempt's delayed copy could otherwise land
	// after a faster, newer attempt's and physically overwrite dstKey's bytes
	// with stale content, even though the newer attempt's own record update
	// already succeeded (LFXV2-2016 lfx-reviewer finding on PR #87). Returns
	// a *StalePromotionError (which satisfies errors.Is(err,
	// ErrStalePromotion)) if dstKey already carries a generation at least as
	// new; callers must resolve that against their own arbiter rather than
	// retrying blindly.
	CopyIfNewer(ctx context.Context, srcKey, dstKey string, generation int64) error
}
