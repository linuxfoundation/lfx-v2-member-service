// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// lfxSalesforceNS is a stable, platform-specific UUID v5 namespace derived from a
// well-known URL. All deterministic UIDs for Salesforce objects are generated under
// this namespace. Because Salesforce IDs are globally unique (they embed object-type
// information), no additional prefix is needed.
var lfxSalesforceNS = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://lfx.dev/salesforce"))

// generateDeterministicUID generates a deterministic UUID v5 for a Salesforce SFID
// using the platform-specific namespace. Salesforce IDs are globally unique, so no
// type prefix is required.
func generateDeterministicUID(sfid string) string {
	return uuid.NewSHA1(lfxSalesforceNS, []byte(sfid)).String()
}

// buildMembershipName constructs the display name for a project_members_b2b document
// in the format "{company_name} - {product_name}". Falls back gracefully when either
// part is empty.
func buildMembershipName(companyName, productName string) string {
	switch {
	case companyName != "" && productName != "":
		return fmt.Sprintf("%s - %s", companyName, productName)
	case companyName != "":
		return companyName
	case productName != "":
		return productName
	default:
		return ""
	}
}

// coalesceDate returns the first non-empty string from the provided values.
func coalesceDate(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseTimestampOrNow parses an ISO 8601 timestamp string and returns the time.Time value.
// Returns time.Now() when the string is empty or cannot be parsed.
func parseTimestampOrNow(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	// Try common Salesforce timestamp formats.
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05.000Z0700",
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05.000+0000",
		"2006-01-02T15:04:05+0000",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}
