// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package svgsanitize

import (
	"fmt"
	"regexp"
	"strings"
)

// This file resolves an SVG's embedded <style> sheet into presentation
// attributes so that stripping <style> (which stays mandatory — CSS can carry
// url()/expression() payloads) no longer silently destroys the artwork.
//
// The bug it fixes: Illustrator exports every colour as a class rule
// (.st212{fill:#003764;}) referenced by class="st212". The sanitizer dropped
// <style> but kept class, leaving selectors pointing at rules that no longer
// existed, so every path fell back to the SVG default fill of black. Output
// was a valid, correctly-sized, entirely black logo with no error raised.
//
// Only simple single-class selectors are understood, which is all Adobe
// Illustrator emits. Anything else fails closed rather than rendering wrong:
// a rejected upload is recoverable, a silently miscoloured logo is not.

// classSelectorPattern matches one simple class selector and captures its
// name. Illustrator emits ".st0"..".stN" plus graphic-style names carrying
// its "_x0020_" space encoding (".Graphic_x0020_Style_x0020_2"), so the
// character class has to allow underscores and digits, not just letters.
var classSelectorPattern = regexp.MustCompile(`^\.([A-Za-z_][-\w]*)$`)

// cssCommentPattern matches a /* ... */ comment, including across newlines.
var cssCommentPattern = regexp.MustCompile(`(?s)/\*.*?\*/`)

// inertProperties are declarations that reach no SVG presentation attribute
// yet cannot change rendering, so dropping them is lossless and must not
// trip the fail-closed check. enable-background was removed from the SVG 2
// specification and is implemented by no current browser; Illustrator still
// emits it out of habit.
var inertProperties = map[string]bool{
	"enable-background": true,
}

// styleRule is one parsed "selector { ... }" block. Declarations keep source
// order because a later declaration for the same property wins within a rule.
type styleRule struct {
	class string
	decls []cssDeclaration
}

// cssDeclaration is a single "property: value" pair.
type cssDeclaration struct {
	property string
	value    string
}

// stylesheet is every rule parsed from a document's <style> elements, in
// source order — which is also cascade order for the equal-specificity,
// single-class selectors this parser accepts.
type stylesheet struct {
	rules []styleRule
}

// empty reports whether there is nothing to inline, letting the caller skip
// the resolve step entirely for the common no-<style> document.
func (s *stylesheet) empty() bool {
	return s == nil || len(s.rules) == 0
}

// parseStylesheet parses the concatenated text of a document's <style>
// elements into rules.
//
// It rejects — rather than ignores — any selector it does not understand.
// Ignoring one would mean emitting an SVG whose styling silently differs from
// the author's intent, which is the exact failure mode this file exists to
// prevent. Unknown *properties* get the opposite treatment: they are carried
// through to resolveClasses, which only rejects them if the rule they belong
// to is actually referenced by an element (see resolveClasses).
func parseStylesheet(css string) (*stylesheet, error) {
	css = cssCommentPattern.ReplaceAllString(css, " ")

	sheet := &stylesheet{}
	for rest := css; ; {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			// Trailing text after the last rule is only acceptable if it is
			// whitespace; anything else means a truncated or unparsable sheet.
			if strings.TrimSpace(rest) != "" {
				return nil, fmt.Errorf("svgsanitize: unsupported CSS in <style>: trailing content %q", truncateForError(rest))
			}
			return sheet, nil
		}
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			return nil, fmt.Errorf("svgsanitize: unsupported CSS in <style>: unterminated rule")
		}
		close += open

		selector := strings.TrimSpace(rest[:open])
		decls := parseDeclarations(rest[open+1 : close])
		rest = rest[close+1:]

		// An at-rule (@media, @import, @font-face, ...) changes which
		// declarations apply, or pulls in an external sheet. Neither can be
		// represented as a presentation attribute.
		if strings.HasPrefix(selector, "@") {
			return nil, fmt.Errorf("svgsanitize: unsupported CSS in <style>: at-rule %q", truncateForError(selector))
		}

		for _, one := range strings.Split(selector, ",") {
			one = strings.TrimSpace(one)
			m := classSelectorPattern.FindStringSubmatch(one)
			if m == nil {
				return nil, fmt.Errorf("svgsanitize: unsupported CSS selector %q in <style>: only simple class selectors are supported", truncateForError(one))
			}
			sheet.rules = append(sheet.rules, styleRule{class: m[1], decls: decls})
		}
	}
}

// parseDeclarations splits a rule body into property/value pairs. Malformed
// fragments (no colon, empty property, empty value) are skipped rather than
// rejected: they contribute no styling, so dropping them changes nothing a
// browser would have rendered.
func parseDeclarations(body string) []cssDeclaration {
	var out []cssDeclaration
	for _, part := range strings.Split(body, ";") {
		colon := strings.IndexByte(part, ':')
		if colon < 0 {
			continue
		}
		property := strings.ToLower(strings.TrimSpace(part[:colon]))
		value := strings.TrimSpace(part[colon+1:])
		if property == "" || value == "" {
			continue
		}
		out = append(out, cssDeclaration{property: property, value: value})
	}
	return out
}

// resolveClasses computes the presentation attributes contributed by the
// stylesheet to an element carrying the given class attribute value.
//
// Only rules whose class is actually referenced are inspected. That
// distinction matters in practice: a real Illustrator export carries hundreds
// of dead rules copied from a shared master document (the Linux Foundation
// logo defines 304 classes and uses 2), and those routinely mention
// properties with no attribute equivalent. Failing on them would reject
// nearly every genuine logo while changing nothing about how the file renders.
//
// Returned attributes are keyed by property name; the caller decides how they
// merge with the element's own attributes.
func (s *stylesheet) resolveClasses(classAttr string) ([]cssDeclaration, error) {
	if s.empty() || strings.TrimSpace(classAttr) == "" {
		return nil, nil
	}

	referenced := make(map[string]bool)
	for _, name := range strings.Fields(classAttr) {
		referenced[name] = true
	}

	// Later rules override earlier ones for the same property, so walking in
	// source order and overwriting yields the cascade result. Insertion order
	// is tracked separately so output is deterministic across runs.
	winner := make(map[string]string)
	var order []string
	for _, rule := range s.rules {
		if !referenced[rule.class] {
			continue
		}
		for _, d := range rule.decls {
			switch {
			case inertProperties[d.property]:
				continue
			case !allowedAttributes[d.property]:
				return nil, fmt.Errorf("svgsanitize: unsupported CSS property %q in rule .%s: cannot be represented as an SVG presentation attribute", d.property, rule.class)
			}
			if _, seen := winner[d.property]; !seen {
				order = append(order, d.property)
			}
			winner[d.property] = d.value
		}
	}

	out := make([]cssDeclaration, 0, len(order))
	for _, property := range order {
		out = append(out, cssDeclaration{property: property, value: winner[property]})
	}
	return out, nil
}

// truncateForError bounds a fragment of attacker-controlled CSS before it is
// interpolated into an error that surfaces to the uploader, so a hostile file
// cannot use the message as an amplification channel.
func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	const max = 40
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
