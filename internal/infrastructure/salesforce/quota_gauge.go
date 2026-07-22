// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"net/http"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// APIUsageGauge implements port.SalesforceQuotaGauge over the coherent quota
// observation recorded by rateLimitTransport on every Salesforce HTTP response.
//
// Active refresh is delegated to refreshFn, which issues one lightweight
// /limits GET through the same rate-limited client so the passive transport
// records a fresh observation. refreshFn may be nil (active refresh disabled),
// in which case Refresh returns an error.
type APIUsageGauge struct {
	refreshFn func(ctx context.Context) error
}

// NewAPIUsageGauge creates an APIUsageGauge backed by the package-level quota
// observation. refreshFn issues an active /limits GET through the rate-limited
// client; pass nil to disable active refresh (Refresh then errors).
func NewAPIUsageGauge(refreshFn func(ctx context.Context) error) *APIUsageGauge {
	return &APIUsageGauge{refreshFn: refreshFn}
}

// NewLimitsRefreshFunc returns a refresh function that issues one lightweight
// GET /limits through the go-salesforce client. The client is wired with
// rateLimitTransport (see Config.Init), so the response's Sforce-Limit-Info
// header updates the shared quota observation via the passive write path — the
// header remains the single usage source (the /limits JSON body is discarded).
//
// DoRequest already prefixes the uri with "/services/data/{apiVersion}"
// internally (github.com/k-capehart/go-salesforce/v3 requests.go), so the uri
// passed here must be relative — passing the full "/services/data/.../limits"
// path would double the prefix and 404.
//
// The go-salesforce DoRequest API does not accept a context; cancellation is
// best-effort at the caller.
func NewLimitsRefreshFunc(client *sf.Salesforce) func(ctx context.Context) error {
	return func(_ context.Context) error {
		_, err := client.DoRequest(http.MethodGet, "/limits", nil)
		return err
	}
}

// Ensure APIUsageGauge satisfies the port at compile time.
var _ port.SalesforceQuotaGauge = (*APIUsageGauge)(nil)

// APIUsage returns the most-recently observed Salesforce API usage counters.
// Returns (-1, -1) when no response has been received yet.
func (g *APIUsageGauge) APIUsage() (current, limit int64) {
	c, l, _, _ := loadQuotaObservation()
	return c, l
}

// Snapshot returns the coherent most-recent observation.
func (g *APIUsageGauge) Snapshot() port.QuotaSnapshot {
	c, l, at, gen := loadQuotaObservation()
	return port.QuotaSnapshot{Current: c, Limit: l, ObservedAt: at, Generation: gen}
}

// Refresh issues one active /limits GET through the rate-limited client and
// returns the resulting snapshot only after a newer valid observation is
// recorded by the passive transport. See port.SalesforceQuotaGauge.Refresh.
func (g *APIUsageGauge) Refresh(ctx context.Context) (port.QuotaSnapshot, error) {
	before := g.Snapshot()
	if g.refreshFn == nil {
		return before, fmt.Errorf("salesforce: active quota refresh not configured")
	}
	if err := g.refreshFn(ctx); err != nil {
		return before, fmt.Errorf("salesforce: quota refresh request failed: %w", err)
	}
	after := g.Snapshot()
	if after.Generation <= before.Generation {
		// No newer observation — missing/malformed header or a response that did
		// not carry Sforce-Limit-Info. Do not trust the stale reading as fresh.
		return after, fmt.Errorf("salesforce: quota refresh produced no newer observation")
	}
	if after.Limit <= 0 || after.Current < 0 {
		return after, fmt.Errorf("salesforce: quota refresh produced invalid observation (current=%d limit=%d)", after.Current, after.Limit)
	}
	return after, nil
}
