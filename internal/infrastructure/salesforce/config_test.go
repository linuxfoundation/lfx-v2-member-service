// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigFromEnv_AccountSlugFieldToggle covers the SF_ACCOUNT_SLUG_FIELD_ENABLED
// parser: unset and any true spelling keep Slug__c selected (the zero value of
// Config.AccountSlugFieldDisabled), false spellings disable it, and anything
// strconv.ParseBool rejects fails closed at startup rather than silently
// picking a projection (lfx-self-serve#2570). Serial: t.Setenv forbids
// t.Parallel.
func TestConfigFromEnv_AccountSlugFieldToggle(t *testing.T) {
	// Minimum credentials so ConfigFromEnv reaches the toggle parse.
	t.Setenv("SF_INSTANCE_URL", "https://example.my.salesforce.com")
	t.Setenv("SF_CLIENT_ID", "client-id")
	t.Setenv("SF_CLIENT_SECRET", "client-secret")
	t.Setenv("SF_USERNAME", "")

	tests := []struct {
		name         string
		raw          string
		set          bool
		wantDisabled bool
		wantErr      bool
	}{
		{name: "unset keeps slug enabled", set: false, wantDisabled: false},
		{name: "true keeps slug enabled", set: true, raw: "true", wantDisabled: false},
		{name: "1 keeps slug enabled", set: true, raw: "1", wantDisabled: false},
		{name: "false disables slug", set: true, raw: "false", wantDisabled: true},
		{name: "0 disables slug", set: true, raw: "0", wantDisabled: true},
		{name: "garbage fails closed", set: true, raw: "maybe", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("SF_ACCOUNT_SLUG_FIELD_ENABLED", tt.raw)
			} else {
				t.Setenv("SF_ACCOUNT_SLUG_FIELD_ENABLED", "")
			}

			cfg, err := ConfigFromEnv()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "SF_ACCOUNT_SLUG_FIELD_ENABLED")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantDisabled, cfg.AccountSlugFieldDisabled)
		})
	}
}
