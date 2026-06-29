// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/sha512"
	"regexp"
	"strings"

	"github.com/akamensky/base58"
)

var (
	// Detect username compatibility with Auth0-generated user IDs.
	safeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,58}[A-Za-z0-9]$`)
	hexUserRE  = regexp.MustCompile(`^[0-9a-f]{24,60}$`)
)

// MapUsernameToAuthSub converts an LFX username to the Auth0 user_id format used by the
// Auth0 Management API (auth0|{userID}). Not used for v2 service writes or JWT impersonation.
//
// The mapping logic:
//   - Safe usernames (matching safeNameRE and not hexUserRE): use directly as userID
//   - Unsafe usernames: hash with SHA512 and encode to base58 (~88 chars) for legacy usernames
//     longer than 60 characters, with non-standard chars, or that might collide with future
//     24+ character Auth0 native DB hexadecimal hash
//
// Returns: "auth0|{userID}" format string.
func MapUsernameToAuthSub(username string) string {
	if username == "" {
		return ""
	}

	var userID string
	if safeNameRE.MatchString(username) && !hexUserRE.MatchString(username) {
		// Safe and forward-compatible to use the username as the unique ID.
		userID = username
	} else {
		// Uses a sha512 hash encoded to base58 (~88 chars) for legacy usernames
		// longer than 60 characters, with non-standard chars, or that might
		// collide with a future 24+ character Auth0 native DB hexadecimal hash.
		hash := sha512.Sum512([]byte(username))
		userID = base58.Encode(hash[:])
	}

	return "auth0|" + userID
}

// AuthSubLookupKey returns the best key for an auth-service user_metadata lookup: a principal that
// is already provider-qualified (contains "|", e.g. "auth0|abc", "oidc|…") is returned unchanged,
// while a bare LFID username is mapped to its deterministic "auth0|" sub. Resolving by sub lets
// auth-service do a cheap get-by-id instead of a rate-limited Auth0 user *search*.
func AuthSubLookupKey(principal string) string {
	principal = strings.TrimSpace(principal)
	if principal == "" || strings.Contains(principal, "|") {
		return principal
	}
	return MapUsernameToAuthSub(principal)
}
