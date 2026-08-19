// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// UserReader provides access to user identity information from auth-service.
type UserReader interface {
	// UsernameByEmail resolves the registered LFID username for the given primary email address.
	// Returns NotFound only for a definitive "no registered account" reply from auth-service. A
	// transport-level failure (timeout, no responders, connection error — no reply received at all)
	// returns a different error type; callers MUST NOT treat it as evidence the email is unregistered.
	UsernameByEmail(ctx context.Context, email string) (string, error)
	// UserMetadataByPrincipal resolves profile metadata for a principal (username/sub). Returns NotFound
	// when no metadata is resolvable.
	UserMetadataByPrincipal(ctx context.Context, principal string) (UserMetadata, error)
}

// UserMetadata is the subset of auth-service profile metadata the member-service denormalizes.
type UserMetadata struct {
	// Picture is the avatar URL; empty when the user has no photo.
	Picture string
}
