// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"

	sf "github.com/k-capehart/go-salesforce/v3"
)

// sfQueryBatchSize is the number of records requested from Salesforce per
// initial query call. Salesforce enforces a hard cap of 2000 records per
// response; 1000 balances first-page latency (smaller payload to wait for)
// against the number of round-trips needed to cover a full result set. The
// remaining records are fetched eagerly in a background goroutine so
// subsequent logical pages are served from the cache rather than live SOQL.
const sfQueryBatchSize = 1000

// ValidPageSizes is the set of logical page sizes accepted by the API.
// All values must divide evenly into sfQueryBatchSize so that intra-batch
// slicing never crosses a batch boundary mid-record.
var ValidPageSizes = map[int]bool{
	10:   true,
	50:   true,
	100:  true,
	200:  true,
	500:  true,
	1000: true,
}

// NormalizePageSize rounds an arbitrary page size up to the nearest valid
// page size. Values exceeding 1000 are capped at 1000; values ≤ 0 default
// to 200.
func NormalizePageSize(n int) int {
	if n <= 0 {
		return 200
	}
	for _, s := range []int{10, 50, 100, 200, 500, 1000} {
		if n <= s {
			return s
		}
	}
	return 1000
}

// PageCursor is the internal representation of the opaque page token returned
// to API consumers. It encodes all state needed to resume pagination without
// retaining any live Salesforce locator:
//
//   - BatchIndex: which SF batch (0-based). Batch 0 is always keyed by the
//     stable query parameters; batches 1+ are keyed by the iterator embedded
//     in the previous batch's cache entry.
//   - BatchOffset: the index within the identified batch where the next
//     logical page starts. Always < sfQueryBatchSize.
//   - PageSize: the logical page size agreed at the start of the sequence.
//     Encoded in the cursor so mid-stream size changes are rejected.
//   - NextBatchIterator: the random token stored in the current batch's cache
//     entry that keys the next batch's cache entry. This is only set when the
//     cursor points to the first logical page of a new batch (BatchOffset==0
//     and there is a subsequent batch). Consumers treat it as opaque.
//
// Consumers never see this struct — they only see the base64url-encoded JSON
// produced by EncodeCursor / consumed by DecodeCursor.
type PageCursor struct {
	BatchIndex        int    `json:"b"`
	BatchOffset       int    `json:"o,omitempty"`
	PageSize          int    `json:"p"`
	NextBatchIterator string `json:"i,omitempty"`
}

// EncodeCursor serialises a PageCursor to an opaque, URL-safe base64url token.
func EncodeCursor(c PageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor reverses EncodeCursor. Returns an error if the token is not
// valid base64url-encoded JSON. An empty token decodes to a zero PageCursor
// (first page of batch 0).
func DecodeCursor(encoded string) (PageCursor, error) {
	if encoded == "" {
		return PageCursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return PageCursor{}, fmt.Errorf("invalid page token: %w", err)
	}
	var c PageCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return PageCursor{}, fmt.Errorf("invalid page token: %w", err)
	}
	return c, nil
}

// soqlPageResponse is the raw Salesforce REST query response for a single page.
type soqlPageResponse struct {
	// TotalSize is the total number of records matching the query across all
	// pages. Present on initial queries; continuation pages may repeat it or
	// omit it depending on the API version.
	TotalSize int `json:"totalSize"`
	// Done is true when this response contains the last page of results.
	Done bool `json:"done"`
	// NextRecordsUrl is the relative URL to fetch the next page. Present when
	// Done is false. Example: "/services/data/v63.0/query/01gXXX-2000".
	NextRecordsUrl string `json:"nextRecordsUrl"`
	// Records contains the raw JSON objects for this page.
	Records []json.RawMessage `json:"records"`
}

// PageResult carries the decoded records for a single SOQL page together with
// the continuation token for the next page (if any).
type PageResult[T any] struct {
	// Records is the decoded slice of domain objects for this page.
	Records []T
	// NextPageToken is the raw nextRecordsUrl value from Salesforce (the full
	// relative path, e.g. "/services/data/v63.0/query/01gXXX-2000"). Empty
	// when Done is true (last page). Callers should treat this as opaque and
	// pass it unchanged to the next QueryPage call.
	NextPageToken string
	// TotalSize is the total record count reported by Salesforce. Only
	// populated on first-page responses; may be 0 on continuation pages.
	TotalSize int
}

// QueryPage executes a single-page SOQL query against the Salesforce REST API
// using sf.DoRequest, which automatically handles INVALID_SESSION_ID responses
// by re-authenticating and retrying the request once. Unlike a raw HTTP call,
// this ensures that an expired session mid-operation does not cause a permanent
// 401 error — the SDK refreshes the access token and transparently retries.
//
// Pass an empty pageToken to execute a new query. Pass a non-empty pageToken
// (the NextPageToken value from a previous PageResult) to fetch the next page;
// in this case the query string is ignored by Salesforce.
//
// The SF batch size is always sfQueryBatchSize. Logical page slicing (the
// client-facing page size) is handled by the caller after records are returned.
func QueryPage[T any](ctx context.Context, client *sf.Salesforce, query string, pageToken string) (PageResult[T], error) {
	return queryPage[T](client, "/query", query, pageToken)
}

// queryPage is QueryPage against endpoint: "/query", or "/queryAll", which also
// returns records in the recycle bin.
func queryPage[T any](client *sf.Salesforce, endpoint, query, pageToken string) (PageResult[T], error) {
	apiVer := client.GetAPIVersion()

	// Build the URI relative to the versioned base path that sf.DoRequest
	// prepends. DoRequest constructs the full endpoint as:
	//   instanceURL + "/services/data/" + apiVersion + uri
	// so uri must start with "/" and must NOT include the version prefix.
	var uri string
	if pageToken == "" {
		uri = endpoint + "?q=" + url.QueryEscape(query)
	} else {
		// Continuation page: the token is the raw nextRecordsUrl returned by
		// Salesforce, which already includes the full versioned path like
		// "/services/data/v63.0/query/<locator>". Strip the version prefix so
		// that DoRequest does not double it.
		uri = strings.TrimPrefix(pageToken, "/services/data/"+apiVer)
	}

	// Sforce-Query-Options is honoured by Salesforce only on the initial
	// /query?q=... call; locator fetches return up to 2000 records regardless.
	// Always request sfQueryBatchSize records — logical page slicing is the
	// caller's responsibility and is completely independent of the SF batch size.
	var opts []sf.RequestOption
	if pageToken == "" {
		opts = append(opts, sf.WithHeader("Sforce-Query-Options", fmt.Sprintf("batchSize=%d", sfQueryBatchSize)))
	}

	// DoRequest routes through the SDK's internal doRequest function, which
	// detects INVALID_SESSION_ID in a non-2xx response, calls refreshSession
	// to obtain a fresh access token, and retries the request exactly once.
	// This is the key difference from the previous raw-http implementation,
	// which bypassed the SDK entirely and never triggered session renewal.
	resp, err := client.DoRequest("GET", uri, nil, opts...)
	if err != nil {
		return PageResult[T]{}, fmt.Errorf("SOQL page request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close errors are not actionable.

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PageResult[T]{}, fmt.Errorf("reading SOQL page response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PageResult[T]{}, fmt.Errorf("SOQL page request returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 512))
	}

	var raw soqlPageResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return PageResult[T]{}, fmt.Errorf("unmarshalling SOQL page response: %w", err)
	}

	records := make([]T, 0, len(raw.Records))
	for i, rec := range raw.Records {
		var item T
		if err := json.Unmarshal(rec, &item); err != nil {
			return PageResult[T]{}, fmt.Errorf("unmarshalling SOQL record %d: %w", i, err)
		}
		records = append(records, item)
	}

	var nextToken string
	if !raw.Done && raw.NextRecordsUrl != "" {
		nextToken = raw.NextRecordsUrl
	}

	return PageResult[T]{
		Records:       records,
		NextPageToken: nextToken,
		TotalSize:     raw.TotalSize,
	}, nil
}

// QueryAllPages fetches all remaining records starting from the given locator
// token, following nextRecordsUrl until done. Pass an empty locator to
// re-execute from the initial query. The accumulated records and the total
// size reported by Salesforce (from the first response) are returned.
//
// This is intended for use in background goroutines that eagerly consume a
// locator immediately after it is received, so the locator expiry window
// (typically 15 minutes) is never a concern for cached pages.
func QueryAllPages[T any](ctx context.Context, client *sf.Salesforce, query string, locator string) ([]T, int, error) {
	return queryPages[T](ctx, client, "/query", query, locator)
}

// QueryDeletedAndLivePages is QueryAllPages against the queryAll endpoint, so
// records in the recycle bin (IsDeleted = true) are returned as well.
func QueryDeletedAndLivePages[T any](ctx context.Context, client *sf.Salesforce, query string) ([]T, error) {
	records, _, err := queryPages[T](ctx, client, "/queryAll", query, "")
	return records, err
}

func queryPages[T any](ctx context.Context, client *sf.Salesforce, endpoint, query, locator string) ([]T, int, error) {
	var all []T
	totalSize := 0
	token := locator

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("QueryAllPages aborted by context: %w", err)
		}
		result, err := queryPage[T](client, endpoint, query, token)
		if err != nil {
			return nil, 0, fmt.Errorf("fetching page (token=%q): %w", token, err)
		}
		if totalSize == 0 && result.TotalSize > 0 {
			totalSize = result.TotalSize
		}
		all = append(all, result.Records...)
		if result.NextPageToken == "" {
			break
		}
		token = result.NextPageToken
	}

	return all, totalSize, nil
}

// IterPages fetches pages of SOQL query results, converts each record from Raw
// to Out via the converter function, and passes batches of converted records to
// the callback function fn. Memory is bounded to one page at a time.
//
// Conversion errors are logged as warnings and the offending record is skipped.
// If fn returns an error, iteration stops immediately and that error is returned.
func IterPages[Raw any, Out any](
	ctx context.Context,
	client *sf.Salesforce,
	query string,
	converter func(Raw) (Out, error),
	fn func([]Out) error,
) error {
	token := ""

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("IterPages aborted by context: %w", err)
		}
		result, err := QueryPage[Raw](ctx, client, query, token)
		if err != nil {
			return fmt.Errorf("fetching page (token=%q): %w", token, err)
		}

		converted := make([]Out, 0, len(result.Records))
		for _, raw := range result.Records {
			out, err := converter(raw)
			if err != nil {
				slog.WarnContext(ctx, "skipping record due to conversion error", "error", err)
				continue
			}
			converted = append(converted, out)
		}

		if err := fn(converted); err != nil {
			return err
		}

		if result.NextPageToken == "" {
			break
		}
		token = result.NextPageToken
	}

	return nil
}

// truncate shortens s to at most n bytes, appending "…" if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
