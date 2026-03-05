// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHeimdallClaims_Validate(t *testing.T) {
	tests := []struct {
		name    string
		claims  HeimdallClaims
		wantErr bool
	}{
		{
			name: "valid claims with principal",
			claims: HeimdallClaims{
				Principal: "user123",
				Email:     "test@example.com",
			},
			wantErr: false,
		},
		{
			name: "valid claims with principal only",
			claims: HeimdallClaims{
				Principal: "user456",
			},
			wantErr: false,
		},
		{
			name: "invalid claims without principal",
			claims: HeimdallClaims{
				Email: "test@example.com",
			},
			wantErr: true,
		},
		{
			name: "invalid claims with empty principal",
			claims: HeimdallClaims{
				Principal: "",
				Email:     "test@example.com",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.claims.Validate(context.Background())
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "principal must be provided")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewJWTAuth(t *testing.T) {
	tests := []struct {
		name      string
		config    JWTAuthConfig
		wantErr   bool
		expectNil bool
	}{
		{
			name:      "successful initialization with defaults",
			config:    JWTAuthConfig{},
			wantErr:   false,
			expectNil: false,
		},
		{
			name: "successful initialization with custom values",
			config: JWTAuthConfig{
				JWKSURL:  "http://custom-jwks:4457/.well-known/jwks",
				Audience: "custom-audience",
			},
			wantErr:   false,
			expectNil: false,
		},
		{
			name: "invalid JWKS URL",
			config: JWTAuthConfig{
				JWKSURL: "://invalid-url",
			},
			wantErr:   true,
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, err := NewJWTAuth(tt.config)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectNil {
				assert.Nil(t, auth)
			} else {
				assert.NotNil(t, auth)
				if auth != nil {
					assert.NotNil(t, auth.validator)
					assert.Equal(t, tt.config, auth.config)
				}
			}
		})
	}
}

func TestJWTAuth_ParsePrincipal_MockMode(t *testing.T) {
	tests := []struct {
		name               string
		mockLocalPrincipal string
		token              string
		expected           string
		wantErr            bool
	}{
		{
			name:               "mock mode with valid principal",
			mockLocalPrincipal: "test-user-123",
			token:              "any-token",
			expected:           "test-user-123",
			wantErr:            false,
		},
		{
			name:               "production mode without mock",
			mockLocalPrincipal: "",
			token:              "invalid-token",
			expected:           "",
			wantErr:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &JWTAuth{
				validator: nil,
				config: JWTAuthConfig{
					MockLocalPrincipal: tt.mockLocalPrincipal,
				},
			}

			logger := slog.Default()
			principal, err := auth.ParsePrincipal(context.Background(), tt.token, logger)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, principal)
			}
		})
	}
}

func TestJWTAuth_ParsePrincipal_NilValidator(t *testing.T) {
	t.Run("nil validator returns error", func(t *testing.T) {
		auth := &JWTAuth{
			validator: nil,
			config:    JWTAuthConfig{},
		}

		logger := slog.Default()
		principal, err := auth.ParsePrincipal(context.Background(), "some-token", logger)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "JWT validator is not set up")
		assert.Empty(t, principal)
	})
}

func TestJWTAuth_Constants(t *testing.T) {
	t.Run("verify constants", func(t *testing.T) {
		assert.Equal(t, "heimdall", defaultIssuer)
		assert.Equal(t, "lfx-v2-member-service", defaultAudience)
		assert.Equal(t, "http://heimdall:4457/.well-known/jwks", defaultJWKSURL)
		assert.NotNil(t, signatureAlgorithm)
	})
}

func TestJWTAuth_CustomClaimsFactory(t *testing.T) {
	t.Run("custom claims factory creates HeimdallClaims", func(t *testing.T) {
		claims := customClaims()
		assert.NotNil(t, claims)

		heimdallClaims, ok := claims.(*HeimdallClaims)
		assert.True(t, ok)
		assert.NotNil(t, heimdallClaims)

		err := heimdallClaims.Validate(context.Background())
		assert.Error(t, err)

		heimdallClaims.Principal = "test-principal"
		err = heimdallClaims.Validate(context.Background())
		assert.NoError(t, err)
	})
}

func TestJWTAuth_Integration_MockMode(t *testing.T) {
	t.Run("end to end mock authentication", func(t *testing.T) {
		auth := &JWTAuth{
			validator: nil,
			config: JWTAuthConfig{
				MockLocalPrincipal: "integration-test-user",
			},
		}

		ctx := context.Background()
		logger := slog.Default()
		principal, err := auth.ParsePrincipal(ctx, "fake-token", logger)

		assert.NoError(t, err)
		assert.Equal(t, "integration-test-user", principal)
	})
}
