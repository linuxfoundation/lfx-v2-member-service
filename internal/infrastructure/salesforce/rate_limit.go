// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const (
	sforceLimitInfoHeader = "Sforce-Limit-Info"
	apiUsagePrefix        = "api-usage="
)

// SforceAPIUsageCurrent is the most-recently observed Salesforce API call count
// for the current 24-hour rolling window, parsed from the Sforce-Limit-Info
// response header (e.g. "api-usage=150/15000"). Initialised to -1 to indicate
// "not yet observed" — distinct from a legitimate zero usage count.
var SforceAPIUsageCurrent = func() *expvar.Int {
	v := expvar.NewInt("sfdc_api_usage_current")
	v.Set(-1)
	return v
}()

// SforceAPIUsageLimit is the maximum Salesforce API calls allowed in the
// current 24-hour rolling window, parsed from the Sforce-Limit-Info response
// header (e.g. "api-usage=150/15000"). Initialised to -1 to indicate
// "not yet observed".
var SforceAPIUsageLimit = func() *expvar.Int {
	v := expvar.NewInt("sfdc_api_usage_limit")
	v.Set(-1)
	return v
}()

// SforceAPIUsageObservedAt is the Unix-nanosecond timestamp of the most-recent
// successful Sforce-Limit-Info header parse. Initialised to 0 ("never
// observed"). Exposed for observability alongside the coherent observation
// snapshot read via loadQuotaObservation.
var SforceAPIUsageObservedAt = expvar.NewInt("sfdc_api_usage_observed_at_unixnano")

// SforceAPIUsageGeneration is a monotonically increasing counter incremented on
// every successful Sforce-Limit-Info header parse (passive traffic or an active
// /limits refresh). It lets a caller prove that a refresh produced a new sample
// without relying on wall-clock comparison. Initialised to 0 ("never observed").
var SforceAPIUsageGeneration = expvar.NewInt("sfdc_api_usage_generation")

// quotaObservation holds a coherent snapshot of the most-recently observed
// Salesforce API usage. All four fields are updated together under quotaObsMu so
// a reader never sees a torn (current, limit, observedAt, generation) tuple.
var (
	quotaObsMu      sync.Mutex
	quotaObsCurrent int64 = -1
	quotaObsLimit   int64 = -1
	quotaObsAt      time.Time
	quotaObsGen     uint64
)

// recordQuotaObservation atomically records a successful usage-header parse:
// it advances the coherent observation snapshot (current, limit, observedAt,
// generation) and mirrors the values into the exported expvars for
// observability. Called at the single passive write point in RoundTrip on every
// successful parse, whether triggered by normal traffic or an active refresh.
func recordQuotaObservation(current, limit int64) {
	quotaObsMu.Lock()
	now := time.Now()
	quotaObsCurrent = current
	quotaObsLimit = limit
	quotaObsAt = now
	quotaObsGen++
	quotaObsMu.Unlock()

	SforceAPIUsageCurrent.Set(current)
	SforceAPIUsageLimit.Set(limit)
	SforceAPIUsageObservedAt.Set(now.UnixNano())
	SforceAPIUsageGeneration.Add(1)
}

// loadQuotaObservation returns the coherent most-recent observation. A
// generation of 0 means no valid usage header has ever been parsed.
func loadQuotaObservation() (current, limit int64, observedAt time.Time, generation uint64) {
	quotaObsMu.Lock()
	defer quotaObsMu.Unlock()
	return quotaObsCurrent, quotaObsLimit, quotaObsAt, quotaObsGen
}

// rateLimitTransport is an http.RoundTripper that wraps an inner transport,
// inspects every Salesforce response for the Sforce-Limit-Info header, and
// keeps the SforceAPIUsageCurrent / SforceAPIUsageLimit expvar counters
// up-to-date. All requests are delegated transparently to the inner transport.
type rateLimitTransport struct {
	inner http.RoundTripper
}

// NewRateLimitTransport returns an http.RoundTripper that wraps inner and
// updates the Salesforce API-usage expvar counters on every response.
// If inner is nil, http.DefaultTransport is used.
func NewRateLimitTransport(inner http.RoundTripper) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &rateLimitTransport{inner: inner}
}

// RoundTrip delegates to the inner transport and then parses the
// Sforce-Limit-Info header from the response, if present.
func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}

	if header := resp.Header.Get(sforceLimitInfoHeader); header != "" {
		current, limit, parseErr := parseAPIUsage(header)
		if parseErr != nil {
			slog.Warn("failed to parse Sforce-Limit-Info header",
				"error", parseErr,
				"header", header,
			)
		} else {
			recordQuotaObservation(current, limit)
		}
	}

	return resp, nil
}

// RegisterOTelMetrics registers observable gauges for the Salesforce API usage
// counters with the global OTEL meter provider. The SDK calls the callback on
// its own schedule (configured via metric.NewPeriodicReader) and pushes the
// current expvar values. If the meter provider is a no-op (i.e.
// OTEL_METRICS_EXPORTER is unset), observations are silently discarded.
//
// Call this once after the OTel SDK has been initialised.
func RegisterOTelMetrics() error {
	meter := otel.GetMeterProvider().Meter("github.com/linuxfoundation/lfx-v2-member-service")

	usageCurrent, err := meter.Int64ObservableGauge("sfdc.api.usage.current",
		metric.WithDescription("Most-recently observed Salesforce API call count for the current 24-hour rolling window"),
		metric.WithUnit("{call}"),
	)
	if err != nil {
		return fmt.Errorf("registering sfdc.api.usage.current gauge: %w", err)
	}

	usageLimit, err := meter.Int64ObservableGauge("sfdc.api.usage.limit",
		metric.WithDescription("Salesforce API call limit for the current 24-hour rolling window"),
		metric.WithUnit("{call}"),
	)
	if err != nil {
		return fmt.Errorf("registering sfdc.api.usage.limit gauge: %w", err)
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		current := SforceAPIUsageCurrent.Value()
		limit := SforceAPIUsageLimit.Value()
		// Only report once we have observed at least one response.
		if current >= 0 {
			o.ObserveInt64(usageCurrent, current)
		}
		if limit >= 0 {
			o.ObserveInt64(usageLimit, limit)
		}
		return nil
	}, usageCurrent, usageLimit)
	if err != nil {
		return fmt.Errorf("registering sfdc api usage callback: %w", err)
	}

	return nil
}

// parseAPIUsage extracts the current and limit integers from a
// Sforce-Limit-Info header value. The expected format contains a segment of
// the form "api-usage=<current>/<limit>", e.g.:
//
//	"api-usage=150/15000"
//	"api-usage=150/15000; per-app-api-usage=17/250(appName=MyConnectedApp)"
//
// Returns an error if the segment is missing or malformed.
func parseAPIUsage(header string) (current, limit int64, err error) {
	// The header may contain multiple semicolon-separated segments.
	for segment := range strings.SplitSeq(header, ";") {
		segment = strings.TrimSpace(segment)
		if !strings.HasPrefix(segment, apiUsagePrefix) {
			continue
		}

		value := strings.TrimPrefix(segment, apiUsagePrefix)
		before, after, ok := strings.Cut(value, "/")
		if !ok {
			return 0, 0, fmt.Errorf("salesforce: malformed api-usage segment %q", segment)
		}

		current, err = strconv.ParseInt(strings.TrimSpace(before), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("salesforce: parsing api-usage current %q: %w", before, err)
		}

		limit, err = strconv.ParseInt(strings.TrimSpace(after), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("salesforce: parsing api-usage limit %q: %w", after, err)
		}

		return current, limit, nil
	}

	return 0, 0, fmt.Errorf("salesforce: %q segment not found in Sforce-Limit-Info header %q", apiUsagePrefix, header)
}
