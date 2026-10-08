// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"net/http"
	"testing"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/stretchr/testify/require"
)

// TestConfig_Init_SetsHTTPTimeout exercises the exact option set Config.Init
// passes to sf.Init (access-token flow substituted for the real auth flow, and
// a fake transport in place of NewRateLimitTransport's inner http.DefaultTransport,
// so no network call is needed). go-salesforce's QueryPage/DoRequest path has no
// context parameter, so the handler's own request deadline never bounds a
// Salesforce call — only http.Client.Timeout does. This guards against that
// timeout option being dropped again.
func TestConfig_Init_SetsHTTPTimeout(t *testing.T) {
	t.Parallel()

	client, err := sf.Init(
		sf.Creds{
			Domain:      "https://test.salesforce.com",
			AccessToken: "fake-token-for-tests",
		},
		sf.WithAPIVersion(defaultAPIVersion),
		sf.WithRoundTripper(NewRateLimitTransport(fakeLimitsRoundTripper{})),
		sf.WithHTTPTimeout(httpTimeout),
	)
	require.NoError(t, err)
	require.NotNil(t, client.GetHTTPClient(), "sf.Init must configure an http.Client")
	require.Equal(t, httpTimeout, client.GetHTTPClient().Timeout,
		"Salesforce http.Client.Timeout must be set explicitly, since the SDK's DoRequest has no context parameter to bind a caller deadline through")
}

// fakeLimitsRoundTripper answers the GET /limits request sf.Init issues to
// validate the access token, without making a real network call.
type fakeLimitsRoundTripper struct{}

func (fakeLimitsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fakeResponse(http.StatusOK, `{}`, nil), nil
}
