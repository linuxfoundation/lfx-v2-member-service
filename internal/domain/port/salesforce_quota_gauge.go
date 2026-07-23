// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"time"
)

// QuotaSnapshot is a coherent reading of the most-recently observed Salesforce
// REST API usage for the current 24-hour rolling window. All fields are
// captured together so a caller never sees a torn tuple.
type QuotaSnapshot struct {
	// Current is the observed API call count; -1 when never observed.
	Current int64
	// Limit is the observed API call limit; -1 when never observed.
	Limit int64
	// ObservedAt is the wall-clock time of the observation; zero when never
	// observed.
	ObservedAt time.Time
	// Generation is a monotonically increasing counter incremented on every
	// successful usage-header parse (passive or active). 0 means never observed.
	Generation uint64
}

// Observed reports whether a valid usage reading has ever been captured.
func (s QuotaSnapshot) Observed() bool {
	return s.Generation > 0 && s.Limit > 0 && s.Current >= 0
}

// Ratio returns Current/Limit, or 0 when no valid limit has been observed.
func (s QuotaSnapshot) Ratio() float64 {
	if s.Limit <= 0 {
		return 0
	}
	return float64(s.Current) / float64(s.Limit)
}

// SalesforceQuotaGauge reports the most-recently observed Salesforce REST API
// usage for the current 24-hour rolling window. The gauge is updated by
// rateLimitTransport on every HTTP response; values of -1 indicate the signal
// has not yet been observed (e.g. no response received yet).
type SalesforceQuotaGauge interface {
	// Snapshot returns the coherent most-recent observation, including its
	// observation time and generation. Generation 0 means never observed.
	Snapshot() QuotaSnapshot

	// Refresh issues one lightweight active /limits request through the
	// rate-limited client so the passive transport records a fresh observation,
	// then returns the resulting snapshot. It returns an error when the request
	// fails, when it produces no newer valid observation (missing/malformed
	// header, unchanged generation), or when the resulting values are invalid
	// (limit <= 0 or current < 0). A newer observation produced by a concurrent
	// Salesforce response is acceptable — it represents the same org-wide quota.
	Refresh(ctx context.Context) (QuotaSnapshot, error)
}
