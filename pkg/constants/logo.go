// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// B2B org logo upload constraints (LFXV2-2016). SVG is allowed, but only
// after passing through pkg/svgsanitize — see ORG-LOGO-UPLOAD-PLAN-LFXV2-2016.md
// (Open Item 3 / Decision #11) for why a hand-rolled allow-list sanitizer was
// chosen over both a blocklist and the (nonexistent) maintained Go library.
const (
	// MaxB2BOrgLogoSizeBytes is the maximum accepted upload size for a B2B org
	// logo (2MB, per the ticket spec — smaller than avatar's 20MB).
	MaxB2BOrgLogoSizeBytes = 2 * 1024 * 1024

	// LogoCacheControl is the Cache-Control value stored on every uploaded
	// logo object. A short TTL is load-bearing: it is what makes a copy of a
	// versioned logo URL propagated elsewhere (e.g. a stale index entry)
	// converge to current bytes within a day instead of staying wrong for a
	// year. Do not lengthen this without also changing the "?v=" cache-busting
	// contract it depends on.
	LogoCacheControl = "public, max-age=86400"
)

// AllowedB2BOrgLogoContentTypes is the content-type allow-list for B2B org
// logo uploads.
var AllowedB2BOrgLogoContentTypes = map[string]string{
	"image/png":     ".png",
	"image/jpeg":    ".jpg",
	"image/svg+xml": ".svg",
}
