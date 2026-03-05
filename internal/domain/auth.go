// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"log/slog"
)

// Authenticator defines the authentication interface for the domain layer.
// This interface allows the service layer to authenticate users without depending
// on specific authentication implementations (JWT, OAuth, etc.).
type Authenticator interface {
	// ParsePrincipal extracts the principal (user identifier) from an authentication token.
	// Returns the principal string and any error that occurred during parsing.
	ParsePrincipal(ctx context.Context, token string, logger *slog.Logger) (string, error)
}
