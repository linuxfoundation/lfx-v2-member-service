// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	sf "github.com/k-capehart/go-salesforce/v3"
)

// soslReservedChars are the characters SOSL requires to be escaped with a
// backslash inside a FIND search term.
const soslReservedChars = `?&|!{}[]()^~*:\"'+-`

// soslMaxResponseBytes caps a search response body. A search returns at most
// aliasSearchLimit Ids, a few KiB, so a larger body is an error.
const soslMaxResponseBytes = 1 << 20

// quoteSOSLPhrase escapes term for a SOSL FIND clause and wraps it in double
// quotes, so Salesforce matches it as one phrase instead of OR-ing its
// tokens. Escaping matters for domains: an unescaped "-" is SOSL's NOT
// operator and an unescaped "}" ends the FIND clause.
func quoteSOSLPhrase(term string) string {
	var b strings.Builder
	b.Grow(len(term) + 2)
	b.WriteByte('"')
	for _, r := range term {
		if strings.ContainsRune(soslReservedChars, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// searchIDs runs a SOSL search and returns the Id of every record found.
// Like QueryPage, it goes through sf.DoRequest so an expired session is
// renewed and retried once, and like QueryPage it cannot be cancelled once
// sent: go-salesforce does not bind a context to the request.
func searchIDs(client *sf.Salesforce, sosl string) ([]string, error) {
	resp, err := client.DoRequest("GET", "/search?q="+url.QueryEscape(sosl), nil)
	if err != nil {
		return nil, fmt.Errorf("SOSL search request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Response body close errors are not actionable.

	body, err := io.ReadAll(io.LimitReader(resp.Body, soslMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading SOSL search response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("SOSL search returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 512))
	}
	if len(body) > soslMaxResponseBytes {
		return nil, fmt.Errorf("SOSL search response exceeds %d bytes", soslMaxResponseBytes)
	}

	var result struct {
		SearchRecords []struct {
			ID string `json:"Id"`
		} `json:"searchRecords"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshalling SOSL search response: %w", err)
	}
	// A missing or null searchRecords, or a record without an Id, is an error:
	// reading it as fewer candidates could send the lookup on to the website
	// and name tiers, or let the re-read drop an unknown candidate and turn
	// an ambiguous result into a unique match.
	if result.SearchRecords == nil {
		return nil, fmt.Errorf("SOSL search response has no searchRecords array: %s", truncate(string(body), 512))
	}
	ids := make([]string, len(result.SearchRecords))
	for i, rec := range result.SearchRecords {
		if rec.ID == "" {
			return nil, fmt.Errorf("SOSL search record %d has no Id: %s", i, truncate(string(body), 512))
		}
		ids[i] = rec.ID
	}
	return ids, nil
}
