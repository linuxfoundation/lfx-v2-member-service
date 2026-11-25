// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// slugMaxLen matches the legacy organization dashboard, whose addresses
// (myorg.lfx.dev/{slug}/…) this rule stays compatible with.
const slugMaxLen = 50

var (
	slugNonAlnumRuns = regexp.MustCompile(`[^a-z0-9]+`)
	// slugSFIDShaped rejects a slug that would be mistaken for the SFID form of
	// the address segment (18-char Salesforce Account id, lowercased).
	slugSFIDShaped = regexp.MustCompile(`^001[a-z0-9]{15}$`)

	// slugFold maps the Latin letters NFKD leaves undecomposed. Without it
	// "Straße" would slug to "stra-e". Both cases are listed for every letter
	// that has one, because the fold runs before lowercasing and a name's
	// capitalization must never move its address (TestSlugify_Idempotent):
	// Go uppercases dotless ı to plain I, so without 'ı' → "i" a case-only
	// rename of "Işık" would relocate the organization.
	slugFold = map[rune]string{
		'ß': "ss", 'ẞ': "ss",
		'ø': "o", 'Ø': "o",
		'æ': "ae", 'Æ': "ae",
		'œ': "oe", 'Œ': "oe",
		'ł': "l", 'Ł': "l",
		'đ': "d", 'Đ': "d",
		'þ': "th", 'Þ': "th",
		'ð': "d", 'Ð': "d",
		'ı': "i",
		'ħ': "h", 'Ħ': "h",
		'ŋ': "ng", 'Ŋ': "ng",
		'ĸ': "k",
	}
)

// Slugify derives an organization's URL slug from its Salesforce Account.Name.
//
// The slug is a pure function of the name — nothing is stored, looked up, or
// disambiguated — so every path that materializes a B2BOrg (SOQL list/search,
// sObject read, CDC, reindex) publishes the same value and a rename simply
// republishes a new one. The rule is the legacy dashboard's, which addresses
// organizations as myorg.lfx.dev/{slug}/… and generates them the same way from
// the same name, plus accent folding and a word-boundary cut for names the
// legacy rule mangles (spec 050, DR-007; lfx-self-serve#2570):
//
//  1. NFKD-decompose and drop combining marks (é → e); fold the letters NFKD
//     does not decompose (ß → ss, ø → o, …).
//  2. Lowercase; replace every run of characters outside [a-z0-9] with "-";
//     trim "-" from both ends.
//  3. If longer than 50: keep the first 50 characters when character 51 is
//     already "-"; otherwise back up to the last "-" inside those 50; hard-cut
//     at 50 when there is none (a single token longer than 50). Trim "-" again.
//  4. An empty result, or one shaped like a Salesforce Account id, yields ""
//     — the organization is then addressed by its SFID.
//
// Changing this function changes existing addresses: bump the contract
// version and reindex every environment.
func Slugify(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range norm.NFKD.String(name) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if folded, ok := slugFold[r]; ok {
			b.WriteString(folded)
			continue
		}
		b.WriteRune(r)
	}

	s := strings.ToLower(b.String())
	s = slugNonAlnumRuns.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	// Only [a-z0-9-] remains, so byte indexes are character indexes. Prefer
	// ending on a whole token: keep the first 50 characters when the next one
	// is already a separator, otherwise back up to the last separator inside
	// the window; a single token longer than 50 is cut hard.
	if len(s) > slugMaxLen {
		cut := slugMaxLen
		if s[slugMaxLen] != '-' {
			if i := strings.LastIndex(s[:slugMaxLen], "-"); i > 0 {
				cut = i
			}
		}
		s = strings.Trim(s[:cut], "-")
	}

	if s == "" || slugSFIDShaped.MatchString(s) {
		return ""
	}
	return s
}
