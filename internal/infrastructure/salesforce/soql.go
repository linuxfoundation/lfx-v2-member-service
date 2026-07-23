// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"strings"
	"time"
)

// quoteSOQL escapes a string value for safe interpolation into a SOQL query.
// Per the Salesforce SOQL reserved-character rules, both the backslash (\) and
// single-quote (') characters must be escaped with a leading backslash. The
// backslash is escaped first so that the escape sequences introduced for single
// quotes are not themselves re-escaped. The result is wrapped in single quotes,
// producing a value suitable for use in a SOQL WHERE clause (e.g. WHERE Id =
// 'abc123').
//
// SOQL does not support parameterized queries, so all external values must be
// escaped before interpolation. Use this function for every user-supplied or
// externally-sourced string that is substituted into a SOQL query string.
func quoteSOQL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", `\'`)
	return "'" + s + "'"
}

// quoteLikeSOQL builds a quoted SOQL LIKE pattern that matches any string
// containing term as a substring (i.e. a contains-style search). All escaping
// is done in a single pass so that no character is escaped twice:
//
//  1. Backslash is escaped to \\ first (it is both the SOQL string-literal
//     escape character and the SOQL LIKE escape character, so it must be
//     handled before any other substitution).
//  2. Single-quote is escaped to \' (SOQL string-literal escaping).
//  3. Percent is escaped to \% (LIKE wildcard → literal).
//  4. Underscore is escaped to \_ (LIKE wildcard → literal).
//  5. The result is wrapped in '% … %' so the surrounding wildcards remain
//     as pattern metacharacters.
//
// The returned string is a complete, single-quoted SOQL literal ready for
// direct interpolation into a LIKE predicate, e.g.:
//
//	fmt.Fprintf(&b, "AND Account.Name LIKE %s", quoteLikeSOQL(term))
//
// Do not pass the result through quoteSOQL; that would re-escape the
// backslashes introduced in steps 1–4, producing a broken pattern.
func quoteLikeSOQL(term string) string {
	term = strings.ReplaceAll(term, `\`, `\\`)
	term = strings.ReplaceAll(term, "'", `\'`)
	term = strings.ReplaceAll(term, "%", `\%`)
	term = strings.ReplaceAll(term, "_", `\_`)
	return "'%" + term + "%'"
}

// buildSOQLInClause builds a comma-separated list of quoted, escaped values
// suitable for a SOQL IN clause (e.g. 'a','b','c'). Each value is passed
// through quoteSOQL so that embedded single quotes are safely escaped.
//
// Returns "”" (an empty quoted string) when values is empty, which produces a
// syntactically valid but always-false IN predicate.
func buildSOQLInClause(values []string) string {
	if len(values) == 0 {
		return "''"
	}

	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = quoteSOQL(v)
	}

	return strings.Join(parts, ",")
}

// soqlDateTime formats t as an unquoted RFC3339 literal for use in SOQL
// dateTime predicates (e.g. WHERE LastModifiedDate >= 2024-06-01T00:00:00Z).
// Unlike string fields, SOQL dateTime literals must NOT be enclosed in single
// quotes — passing the result through quoteSOQL would cause Salesforce to
// reject the query with INVALID_FIELD.
func soqlDateTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// lastModifiedWindowClause returns the SOQL predicate fragment(s) that bound
// LastModifiedDate to the inclusive [since, until] window. Either bound may be
// nil for an open end. The returned string is prefixed with "\n    AND " for
// each present bound so it can be appended directly to a WHERE clause.
func lastModifiedWindowClause(since, until *time.Time) string {
	var clause string
	if since != nil {
		clause += "\n    AND LastModifiedDate >= " + soqlDateTime(*since)
	}
	if until != nil {
		clause += "\n    AND LastModifiedDate <= " + soqlDateTime(*until)
	}
	return clause
}
