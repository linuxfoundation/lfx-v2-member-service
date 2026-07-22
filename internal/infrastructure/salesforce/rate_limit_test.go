// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAPIUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		header        string
		wantCurrent   int64
		wantLimit     int64
		wantErrSubstr string
	}{
		{
			name:        "simple",
			header:      "api-usage=150/15000",
			wantCurrent: 150,
			wantLimit:   15000,
		},
		{
			name:        "with per-app segment",
			header:      "api-usage=150/15000; per-app-api-usage=17/250(appName=MyConnectedApp)",
			wantCurrent: 150,
			wantLimit:   15000,
		},
		{
			name:        "per-app segment first",
			header:      "per-app-api-usage=17/250(appName=MyConnectedApp); api-usage=42/10000",
			wantCurrent: 42,
			wantLimit:   10000,
		},
		{
			name:        "zero current",
			header:      "api-usage=0/15000",
			wantCurrent: 0,
			wantLimit:   15000,
		},
		{
			name:        "whitespace around values",
			header:      "api-usage= 99 / 20000 ",
			wantCurrent: 99,
			wantLimit:   20000,
		},
		{
			name:          "missing api-usage segment",
			header:        "per-app-api-usage=17/250(appName=MyConnectedApp)",
			wantErrSubstr: "segment not found",
		},
		{
			name:          "empty header",
			header:        "",
			wantErrSubstr: "segment not found",
		},
		{
			name:          "missing slash separator",
			header:        "api-usage=15000",
			wantErrSubstr: "malformed api-usage segment",
		},
		{
			name:          "non-numeric current",
			header:        "api-usage=abc/15000",
			wantErrSubstr: "parsing api-usage current",
		},
		{
			name:          "non-numeric limit",
			header:        "api-usage=150/xyz",
			wantErrSubstr: "parsing api-usage limit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			current, limit, err := parseAPIUsage(tc.header)

			if tc.wantErrSubstr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErrSubstr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantCurrent, current)
			assert.Equal(t, tc.wantLimit, limit)
		})
	}
}

func TestRateLimitTransport_UpdatesExpvars(t *testing.T) {
	// Reset the package-level expvars so this test is not affected by order.
	SforceAPIUsageCurrent.Set(-1)
	SforceAPIUsageLimit.Set(-1)

	transport := NewRateLimitTransport(&headerInjectingTransport{
		header: sforceLimitInfoHeader,
		value:  "api-usage=300/15000",
	})

	req, err := newTestRequest()
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, int64(300), SforceAPIUsageCurrent.Value())
	assert.Equal(t, int64(15000), SforceAPIUsageLimit.Value())
}

func TestRateLimitTransport_NoHeader(t *testing.T) {
	SforceAPIUsageCurrent.Set(-1)
	SforceAPIUsageLimit.Set(-1)
	_, _, _, genBefore := loadQuotaObservation()

	transport := NewRateLimitTransport(&headerInjectingTransport{})

	req, err := newTestRequest()
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Values should remain at their initial sentinel — no header means no update.
	assert.Equal(t, int64(-1), SforceAPIUsageCurrent.Value())
	assert.Equal(t, int64(-1), SforceAPIUsageLimit.Value())

	// No header means no observation — the coherent generation must not advance.
	_, _, _, genAfter := loadQuotaObservation()
	assert.Equal(t, genBefore, genAfter, "missing header must not advance the observation generation")
}

func TestRateLimitTransport_MalformedHeader(t *testing.T) {
	SforceAPIUsageCurrent.Set(7)
	SforceAPIUsageLimit.Set(777)
	_, _, _, genBefore := loadQuotaObservation()

	transport := NewRateLimitTransport(&headerInjectingTransport{
		header: sforceLimitInfoHeader,
		value:  "api-usage=not-a-number/15000",
	})

	req, err := newTestRequest()
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Malformed header: existing values must not be overwritten.
	assert.Equal(t, int64(7), SforceAPIUsageCurrent.Value())
	assert.Equal(t, int64(777), SforceAPIUsageLimit.Value())

	// Malformed header means no valid parse — the coherent generation must not advance.
	_, _, _, genAfter := loadQuotaObservation()
	assert.Equal(t, genBefore, genAfter, "malformed header must not advance the observation generation")
}

func TestRecordQuotaObservation_AdvancesGenerationAndTimestamp(t *testing.T) {
	_, _, atBefore, genBefore := loadQuotaObservation()

	recordQuotaObservation(321, 20000)

	current, limit, at, gen := loadQuotaObservation()
	assert.Equal(t, int64(321), current)
	assert.Equal(t, int64(20000), limit)
	assert.Equal(t, genBefore+1, gen, "generation must advance by exactly 1 per successful observation")
	assert.True(t, at.After(atBefore) || at.Equal(atBefore), "observed-at must not move backwards")
	assert.False(t, at.IsZero(), "observed-at must be set on a successful observation")

	// The exported expvars must mirror the coherent snapshot.
	assert.Equal(t, int64(321), SforceAPIUsageCurrent.Value())
	assert.Equal(t, int64(20000), SforceAPIUsageLimit.Value())
	assert.Equal(t, at.UnixNano(), SforceAPIUsageObservedAt.Value())
	assert.Equal(t, int64(gen), SforceAPIUsageGeneration.Value()) //nolint:gosec // gen is a small monotonic counter
}

func TestRateLimitTransport_ValidHeader_AdvancesGeneration(t *testing.T) {
	_, _, _, genBefore := loadQuotaObservation()

	transport := NewRateLimitTransport(&headerInjectingTransport{
		header: sforceLimitInfoHeader,
		value:  "api-usage=500/15000",
	})

	req, err := newTestRequest()
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	current, limit, _, genAfter := loadQuotaObservation()
	assert.Equal(t, int64(500), current)
	assert.Equal(t, int64(15000), limit)
	assert.Greater(t, genAfter, genBefore, "a valid header parse must advance the observation generation")
}

// ── test helpers ─────────────────────────────────────────────────────────────

// headerInjectingTransport is a minimal http.RoundTripper that returns a 200
// response containing a single configurable response header. Used to exercise
// rateLimitTransport without a real network.
type headerInjectingTransport struct {
	header string
	value  string
}

func (t *headerInjectingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}
	if t.header != "" {
		resp.Header.Set(t.header, t.value)
	}
	return resp, nil
}

func newTestRequest() (*http.Request, error) {
	return http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"https://example.salesforce.com/services/data/v63.0/query",
		nil,
	)
}
