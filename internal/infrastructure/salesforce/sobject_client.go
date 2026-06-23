// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package salesforce provides a Salesforce REST API client for querying and
// mutating Salesforce objects via SOQL and the sObject REST endpoints. It wraps
// the github.com/k-capehart/go-salesforce/v3 library.
package salesforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	sf "github.com/k-capehart/go-salesforce/v3"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// sObjectCacher is the storage interface required by SObjectClient. It is
// satisfied by *nats.SObjectCache in production and by an in-memory stub in
// tests, keeping the client decoupled from the NATS infrastructure layer.
type sObjectCacher interface {
	Get(ctx context.Context, key string) (*nats.SObjectCacheEntry, error)
	Put(ctx context.Context, key string, entry *nats.SObjectCacheEntry) error
	Delete(ctx context.Context, key string) error
}

// SObjectClient wraps the Salesforce sObject REST API endpoint
// (GET /services/data/{version}/sobjects/{Type}/{Id}) and integrates HTTP
// conditional GET caching via the NATS member-service-cache KV bucket.
//
// On every fetch, the client:
//  1. Looks up the cached entry for the requested sObject UID key.
//  2. If found, adds If-None-Match (ETag) and/or If-Modified-Since headers to
//     the request so Salesforce can respond with 304 Not Modified when the
//     record is unchanged.
//  3. On 200 OK: updates the cache with the new ETag, Last-Modified, and body.
//  4. On 304 Not Modified: returns the cached body as-is without a cache write.
//  5. On cache miss: fetches without conditional headers and populates the cache.
//
// The client also supports forwarding If-Match / If-Unmodified-Since precondition
// headers from API callers through to Salesforce write endpoints via
// DoConditionalWrite.
type SObjectClient struct {
	sf    *sf.Salesforce
	cache sObjectCacher
}

// NewSObjectClient creates an SObjectClient backed by the given authenticated
// Salesforce client and an sObjectCacher implementation (e.g., NATS cache or
// an in-memory stub for tests).
func NewSObjectClient(sfClient *sf.Salesforce, cache sObjectCacher) *SObjectClient {
	// Wrap the Salesforce library's HTTP transport so outbound requests are
	// captured as OTel HTTP client spans.
	if httpClient := sfClient.GetHTTPClient(); httpClient != nil {
		underlying := httpClient.Transport
		if underlying == nil {
			underlying = http.DefaultTransport
		}
		httpClient.Transport = otelhttp.NewTransport(underlying)
	}
	return &SObjectClient{sf: sfClient, cache: cache}
}

// FetchResult is the outcome of a conditional GET fetch via FetchSObject.
type FetchResult struct {
	// Body is the raw JSON sObject body. Populated on both 200 OK and
	// 304 Not Modified (from cache).
	Body json.RawMessage

	// ETag is the ETag from the 200 OK response, or the cached ETag on 304.
	// Empty when Salesforce did not return an ETag.
	ETag string

	// LastModified is the Last-Modified header from the 200 OK response, or the
	// cached value on 304. Empty when Salesforce did not return the header.
	LastModified string

	// NotModified is true when Salesforce returned 304 Not Modified; the Body is
	// served from the cache unchanged.
	NotModified bool
}

// FetchSObject fetches a single Salesforce sObject record by type and SFID,
// applying HTTP conditional GET caching. cacheKey must be the pre-formed
// "{sobject_type}.{uid}" key used to store this record in the NATS cache.
// fields is the comma-separated list of fields to request (e.g.
// "Id,Name,Status"). Passing an empty fields string omits the ?fields= param.
//
// The caller receives a FetchResult that always has a populated Body (either
// refreshed from Salesforce on 200 or served from the cache on 304/miss).
// A non-nil error indicates an infrastructure failure or an unexpected HTTP
// status code from Salesforce.
//
// Note: this method issues the GET directly via sf.GetHTTPClient() rather than
// sf.DoRequest(). The go-salesforce library's DoRequest discards the response
// pointer whenever the status code is outside 200–300, making it impossible to
// inspect the status code for 304 Not Modified. By using the underlying HTTP
// client directly we retain full control over response handling, context
// propagation, and future OTEL tracing.
//
// Session refresh: on a 401 Unauthorized response, FetchSObject triggers the
// library's re-authentication path via a no-op DoRequest call (which internally
// calls refreshSession and updates the stored access token), then retries the
// original request once with the renewed token.
func (c *SObjectClient) FetchSObject(ctx context.Context, sobjectType, sfid, cacheKey, fields string) (*FetchResult, error) {
	if sobjectType == "" {
		return nil, errs.NewValidation("sobjectType cannot be empty")
	}
	if sfid == "" {
		return nil, errs.NewValidation("sfid cannot be empty")
	}
	if cacheKey == "" {
		return nil, errs.NewValidation("cacheKey cannot be empty")
	}

	// Look up any existing cache entry.
	cached, err := c.cache.Get(ctx, cacheKey)
	if err != nil {
		slog.WarnContext(ctx, "sObject cache get failed; proceeding without conditional headers",
			"sobject_type", sobjectType,
			"sfid", sfid,
			"cache_key", cacheKey,
			"error", err,
		)
		// Non-fatal: proceed without conditional headers.
	}

	// Build the full sObject REST URL. Use net/url to safely encode the
	// fields query parameter so that any special characters are escaped correctly.
	rawURL := fmt.Sprintf("%s/services/data/%s/sobjects/%s/%s",
		c.sf.GetInstanceUrl(), c.sf.GetAPIVersion(), sobjectType, sfid)
	if fields != "" {
		u, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return nil, fmt.Errorf("build sObject URL: %w", parseErr)
		}
		q := url.Values{}
		q.Set("fields", fields)
		u.RawQuery = q.Encode()
		rawURL = u.String()
	}

	resp, err := c.doGet(ctx, rawURL, cached)
	if err != nil {
		return nil, fmt.Errorf("salesforce sObject GET %s/%s: %w", sobjectType, sfid, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// On 401 Unauthorized: delegate to the library for session refresh (it knows
	// the grant type and stored credentials), then retry once.
	if resp.StatusCode == http.StatusUnauthorized {
		// Drain and close the body before discarding the response so the
		// underlying TCP connection can be reused by the HTTP transport.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close() //nolint:errcheck
		if refreshErr := c.refreshSession(); refreshErr != nil {
			return nil, fmt.Errorf("salesforce sObject GET %s/%s: session refresh: %w",
				sobjectType, sfid, refreshErr)
		}
		resp, err = c.doGet(ctx, rawURL, cached)
		if err != nil {
			return nil, fmt.Errorf("salesforce sObject GET %s/%s (retry): %w", sobjectType, sfid, err)
		}
		defer resp.Body.Close() //nolint:errcheck
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return c.handle200(ctx, resp, cacheKey)
	case http.StatusNotModified:
		return c.handle304(ctx, cached, sobjectType, sfid, cacheKey)
	case http.StatusNotFound:
		body, _ := io.ReadAll(resp.Body)
		return nil, errs.NewNotFound(
			fmt.Sprintf("salesforce record not found: %s/%s", sobjectType, sfid),
			fmt.Errorf("status 404: %s", body),
		)
	default:
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("salesforce sObject GET %s/%s: unexpected status %d: %s",
			sobjectType, sfid, resp.StatusCode, body)
	}
}

// doGet issues a single authenticated GET to rawURL, attaching conditional
// headers from cached if present. It returns the raw response; the caller is
// responsible for closing the body.
func (c *SObjectClient) doGet(ctx context.Context, rawURL string, cached *nats.SObjectCacheEntry) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.sf.GetAccessToken())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if cached != nil {
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.LastModified != "" {
			req.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}

	return c.sf.GetHTTPClient().Do(req)
}

// refreshSession triggers the go-salesforce library's internal session-refresh
// path by issuing a DoRequest call that the library will retry with refreshed
// credentials on INVALID_SESSION_ID. The /limits endpoint is used because it is
// lightweight, read-only, and is the same endpoint called during sf.Init.
func (c *SObjectClient) refreshSession() error {
	uri := fmt.Sprintf("/services/data/%s/limits", c.sf.GetAPIVersion())
	_, err := c.sf.DoRequest(http.MethodGet, uri, nil)
	if err != nil {
		return err
	}
	return nil
}

// handle200 processes a 200 OK response: reads the body, extracts ETag and
// Last-Modified, updates the cache, and returns a FetchResult.
//
// When Salesforce does not return a Last-Modified header (common for object
// types such as Asset in some orgs), handle200 falls back to extracting the
// SystemModstamp or LastModifiedDate field from the JSON body and formatting
// it as an RFC 1123 HTTP date. Sub-second precision is truncated to the second
// (floor) so that If-Modified-Since comparisons are conservative: a record
// modified at T+0.500s is represented as T+0s, ensuring Salesforce returns
// 200 (not 304) on the next fetch within that same second.
func (c *SObjectClient) handle200(ctx context.Context, resp *http.Response, cacheKey string) (*FetchResult, error) {
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading sObject response body: %w", err)
	}

	etag := resp.Header.Get("ETag")
	lastModified := resp.Header.Get("Last-Modified")

	// Fall back to body-extracted timestamp when the response header is absent.
	// This allows If-Modified-Since to work for sObject types (e.g. Asset) where
	// Salesforce does not emit a Last-Modified header.
	if lastModified == "" {
		lastModified = extractModstamp(rawBody)
	}

	entry := &nats.SObjectCacheEntry{
		ETag:         etag,
		LastModified: lastModified,
		Body:         json.RawMessage(rawBody),
	}

	if putErr := c.cache.Put(ctx, cacheKey, entry); putErr != nil {
		// Non-fatal: log and continue; the caller still gets a valid response.
		slog.WarnContext(ctx, "failed to update sObject cache after 200 OK",
			"cache_key", cacheKey,
			"error", putErr,
		)
	}

	return &FetchResult{
		Body:         json.RawMessage(rawBody),
		ETag:         etag,
		LastModified: lastModified,
		NotModified:  false,
	}, nil
}

// handle304 processes a 304 Not Modified response: serves the body from the
// existing cache entry and re-writes it to reset the NATS KV TTL. If no cached
// entry is available (should not normally happen — Salesforce should only return
// 304 when we sent conditional headers, which requires a cached entry), an error
// is returned.
//
// Re-writing on every 304 ensures that a frequently-accessed, never-changed
// record is not evicted by the bucket TTL: each successful revalidation extends
// the entry's lifetime by the full TTL window.
func (c *SObjectClient) handle304(ctx context.Context, cached *nats.SObjectCacheEntry, sobjectType, sfid, cacheKey string) (*FetchResult, error) {
	if cached == nil {
		// This is a protocol violation: Salesforce returned 304 without us sending
		// conditional headers. Treat it as an error.
		return nil, fmt.Errorf("salesforce sObject GET %s/%s: received 304 Not Modified but no cached entry exists for key %q",
			sobjectType, sfid, cacheKey)
	}

	slog.DebugContext(ctx, "sObject cache hit (304 Not Modified)",
		"sobject_type", sobjectType,
		"sfid", sfid,
		"cache_key", cacheKey,
	)

	// Re-write the unchanged entry in the background so NATS resets the KV TTL
	// clock. This prevents eviction of frequently-accessed records that are never
	// modified. The goroutine uses a detached context so it is not cancelled when
	// the HTTP response is sent to the caller.
	go func(entry *nats.SObjectCacheEntry) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if putErr := c.cache.Put(ctx, cacheKey, entry); putErr != nil {
			slog.WarnContext(ctx, "failed to refresh sObject cache TTL after 304",
				"cache_key", cacheKey,
				"error", putErr,
			)
		}
	}(cached)

	return &FetchResult{
		Body:         cached.Body,
		ETag:         cached.ETag,
		LastModified: cached.LastModified,
		NotModified:  true,
	}, nil
}

// DoConditionalWrite performs a write (POST/PATCH/DELETE) against a Salesforce
// sObject endpoint, forwarding any If-Match or If-Unmodified-Since precondition
// headers supplied by the API caller. This enables optimistic concurrency
// control: Salesforce returns 412 Precondition Failed when the record has been
// modified since the client last read it.
//
// ifMatch is the value to send as the If-Match request header (typically the
// ETag of the last known version). ifUnmodifiedSince is the value to send as
// If-Unmodified-Since. Both are optional; pass an empty string to omit.
//
// For 2xx and 412 responses the raw *http.Response is returned with a nil
// error; the caller is responsible for closing the body and interpreting the
// status code. For all other non-2xx codes the response body is read (so the
// underlying connection can be reused), the body is closed, and the body text
// is returned as an error — this preserves Salesforce error codes such as
// ENTITY_IS_LOCKED in the error message for retryOnLock to detect.
//
// On 401 Unauthorized a session refresh is attempted and the request is
// retried once, matching the behaviour of FetchSObject.
func (c *SObjectClient) DoConditionalWrite(
	ctx context.Context,
	method, uri string,
	reqBody []byte,
	ifMatch, ifUnmodifiedSince string,
) (*http.Response, error) {
	rawURL := c.sf.GetInstanceUrl() + uri

	newReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("build conditional write request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.sf.GetAccessToken())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		if ifUnmodifiedSince != "" {
			req.Header.Set("If-Unmodified-Since", ifUnmodifiedSince)
		}
		return req, nil
	}

	req, err := newReq()
	if err != nil {
		return nil, err
	}

	resp, err := c.sf.GetHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("salesforce conditional write %s %s: %w", method, uri, err)
	}

	// On 401: refresh session and retry once.
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close() //nolint:errcheck
		if refreshErr := c.refreshSession(); refreshErr != nil {
			return nil, fmt.Errorf("salesforce conditional write %s %s: session refresh: %w",
				method, uri, refreshErr)
		}
		req2, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err = c.sf.GetHTTPClient().Do(req2)
		if err != nil {
			return nil, fmt.Errorf("salesforce conditional write %s %s (retry): %w", method, uri, err)
		}
	}

	// 2xx and 412 are returned to the caller for status-code-level handling.
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusPreconditionFailed {
		return resp, nil
	}

	// For all other non-success codes, surface the body as an error so that
	// Salesforce error codes (e.g. ENTITY_IS_LOCKED) are visible to retryOnLock.
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck
	if readErr != nil {
		return nil, fmt.Errorf("salesforce conditional write %s %s: status %d: read body: %w",
			method, uri, resp.StatusCode, readErr)
	}
	return nil, fmt.Errorf("salesforce conditional write %s %s: status %d: %s",
		method, uri, resp.StatusCode, respBody)
}

// InvalidateCache removes the cached sObject entry for the given cache key.
// Call this after a successful write so the next read fetches fresh data from
// Salesforce. Returns nil if the key does not exist.
func (c *SObjectClient) InvalidateCache(ctx context.Context, cacheKey string) error {
	return c.cache.Delete(ctx, cacheKey)
}

// ── modstamp extraction ───────────────────────────────────────────────────────

// sobjectModstampFields is a minimal struct for extracting the two candidate
// timestamp fields from any Salesforce sObject REST API JSON body.
type sobjectModstampFields struct {
	SystemModstamp   string `json:"SystemModstamp"`
	LastModifiedDate string `json:"LastModifiedDate"`
}

// extractModstamp attempts to extract a conditional-GET timestamp from the raw
// sObject JSON body, for use as an If-Modified-Since header value when
// Salesforce does not return a Last-Modified response header.
//
// It tries SystemModstamp first (updated on any system change, including
// automated processes), falling back to LastModifiedDate (updated only on
// direct user/API edits). The chosen value is parsed from Salesforce's
// ISO 8601 format, truncated to the second (floor), and returned as an RFC
// 1123 HTTP date string.
//
// Floor truncation means a record modified at T+0.500s is represented as T+0s,
// ensuring Salesforce returns 200 (not 304) on the next fetch within that same
// second window — a conservative, safe choice.
//
// Returns "" when neither field is present or parseable.
func extractModstamp(body []byte) string {
	var fields sobjectModstampFields
	if err := json.Unmarshal(body, &fields); err != nil {
		return ""
	}

	raw := fields.SystemModstamp
	if raw == "" {
		raw = fields.LastModifiedDate
	}
	if raw == "" {
		return ""
	}

	t := parseSOQLTime(raw)
	if t.IsZero() {
		return ""
	}

	// Truncate to whole seconds (floor) and format as an HTTP date.
	// Go's time.RFC1123 formats the timezone name as "UTC" but HTTP requires
	// "GMT"; use the equivalent fixed-offset format string instead.
	return t.UTC().Truncate(time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

// ── in-memory cache ───────────────────────────────────────────────────────────

// inMemSObjectCache is a simple in-memory implementation of sObjectCacher
// intended for diagnostic tools and tests that cannot or do not wish to
// connect to NATS. It is safe for concurrent use.
type inMemSObjectCache struct {
	mu      sync.RWMutex
	entries map[string]*nats.SObjectCacheEntry
}

func newInMemSObjectCache() *inMemSObjectCache {
	return &inMemSObjectCache{entries: make(map[string]*nats.SObjectCacheEntry)}
}

func (m *inMemSObjectCache) Get(_ context.Context, key string) (*nats.SObjectCacheEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, nil
	}
	return e, nil
}

func (m *inMemSObjectCache) Put(_ context.Context, key string, entry *nats.SObjectCacheEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = entry
	return nil
}

func (m *inMemSObjectCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	return nil
}

// NewSObjectClientWithMemCache creates an SObjectClient backed by the given
// authenticated Salesforce client and a simple in-memory cache. This is
// intended for diagnostic tools and tests that do not have a NATS connection
// available. Cache entries persist only for the lifetime of the returned
// client.
func NewSObjectClientWithMemCache(sfClient *sf.Salesforce) *SObjectClient {
	return &SObjectClient{sf: sfClient, cache: newInMemSObjectCache()}
}
