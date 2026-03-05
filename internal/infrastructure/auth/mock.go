// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain"
	"github.com/stretchr/testify/mock"
)

// MockJWTAuth implements domain.Authenticator for testing
type MockJWTAuth struct {
	mock.Mock
}

// ParsePrincipal implements domain.Authenticator.
func (m *MockJWTAuth) ParsePrincipal(ctx context.Context, token string, logger *slog.Logger) (string, error) {
	args := m.Called(ctx, token, logger)
	return args.String(0), args.Error(1)
}

// Ensure MockJWTAuth implements domain.Authenticator interface
var _ domain.Authenticator = (*MockJWTAuth)(nil)
