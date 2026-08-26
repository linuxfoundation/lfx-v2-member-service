// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package svgsanitize

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// This file resolves an SVG's embedded <style> sheet into presentation
// attributes so that stripping <style> (which stays mandatory — CSS can carry
// url()/expression() payloads) no longer silently destroys the artwork.
//
// The bug it fixes: Illustrator expresses every colour as a class rule
// (.st212{fill:#003764;}) applied via class="st212". The sanitizer dropped
// <style> but kept class, leaving selectors pointing at rules that no longer
// existed, so every path fell back to the SVG default fill of black. Output
// was a valid, correctly-sized, entirely black logo with no error raised.
//
// Only simple single-class selectors are understood, which is all Adobe
// Illustrator emits. Anything that cannot be reproduced faithfully fails
// closed rather than rendering wrong: a rejected upload is recoverable, a
// silently miscoloured logo is not. That principle is why several paths here
// return an error where merely dropping the declaration would be easier.

// classSelectorPattern matches one simple class selector and captures its
// name. Illustrator emits ".st0"..".stN" plus graphic-style names carrying
// its "_x0020_" space encoding (".Graphic_x0020_Style_x0020_2"), so the
// character class has to allow underscores and digits, not just letters.
var classSelectorPattern = regexp.MustCompile(`^\.([A-Za-z_][-\w]*)$`)

// cssCommentPattern matches a /* ... */ comment, including across newlines.
var cssCommentPattern = regexp.MustCompile(`(?s)/\*.*?\*/`)

// importantPattern matches a trailing CSS "!important" priority marker.
var importantPattern = regexp.MustCompile(`(?i)\s*!\s*important\s*$`)

// inlinableProperties are the CSS properties that may be rewritten into an SVG
// presentation attribute of the same name.
//
// This is deliberately its own set rather than a reuse of allowedAttributes.
// That map answers "may this XML attribute appear in the output", which is a
// strictly larger question: it also admits geometry and identity attributes
// (d, points, x, width, id, class, transform, viewBox). Those are not CSS
// properties, so no browser would ever apply them from a stylesheet — letting
// CSS set them would corrupt artwork (.a{d:none} erases a path) and let a
// crafted sheet rewrite the id that a url(#...) reference resolves against.
var inlinableProperties = map[string]bool{
	"fill": true, "fill-rule": true, "fill-opacity": true,
	"stroke": true, "stroke-width": true, "stroke-linecap": true,
	"stroke-linejoin": true, "stroke-dasharray": true, "stroke-dashoffset": true,
	"stroke-opacity": true, "stroke-miterlimit": true,
	"opacity": true, "stop-color": true, "stop-opacity": true,
	"clip-path": true, "clip-rule": true, "mask": true,
	"color": true, "display": true, "visibility": true, "overflow": true,
	"paint-order": true, "vector-effect": true, "isolation": true,
	"mix-blend-mode": true,
	"font-family":    true, "font-size": true, "font-weight": true,
	"font-style": true, "font-variant": true,
	"text-anchor": true, "dominant-baseline": true, "letter-spacing": true,
}

// inertProperties reach no SVG presentation attribute yet cannot change how a
// document renders, so dropping them is lossless and must not trip the
// fail-closed check. enable-background was removed from SVG 2 and is
// implemented by no current browser; Illustrator still emits it out of habit.
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

// stylesheet is every rule parsed from a document's <style> elements, indexed
// by the class each rule selects. rules keeps source order, which is also
// cascade order for the equal-specificity selectors this parser accepts.
type stylesheet struct {
	rules   []styleRule
	byClass map[string][]int
}

// empty reports whether there is nothing to inline, letting the caller skip
// the resolve step entirely for the common no-<style> document.
func (s *stylesheet) empty() bool {
	return s == nil || len(s.rules) == 0
}

// parseStylesheet parses the concatenated text of a document's <style>
// elements into rules indexed by class.
//
// It rejects — rather than ignores — any selector it does not understand.
// Ignoring one would mean emitting an SVG whose styling silently differs from
// the author's intent, which is the exact failure mode this file exists to
// prevent. Unknown *properties* get the opposite treatment: they are carried
// through to resolveClasses, which only rejects them if the rule they belong
// to is actually referenced by an element (see resolveClasses).
func parseStylesheet(css string) (*stylesheet, error) {
	css = cssCommentPattern.ReplaceAllString(css, " ")

	sheet := &stylesheet{byClass: make(map[string][]int)}
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
		closeIdx := strings.IndexByte(rest[open:], '}')
		if closeIdx < 0 {
			return nil, fmt.Errorf("svgsanitize: unsupported CSS in <style>: unterminated rule")
		}
		closeIdx += open

		selector := strings.TrimSpace(rest[:open])
		decls := parseDeclarations(rest[open+1 : closeIdx])
		rest = rest[closeIdx+1:]

		// An at-rule (@media, @import, @font-face, ...) changes which
		// declarations apply, or pulls in an external sheet. Neither can be
		// represented as a presentation attribute.
		if strings.HasPrefix(selector, "@") {
			return nil, fmt.Errorf("svgsanitize: unsupported CSS at-rule %q in <style>", truncateForError(selector))
		}

		for _, one := range strings.Split(selector, ",") {
			one = strings.TrimSpace(one)
			m := classSelectorPattern.FindStringSubmatch(one)
			if m == nil {
				return nil, fmt.Errorf("svgsanitize: unsupported CSS selector %q in <style>: only simple class selectors are supported", truncateForError(one))
			}
			sheet.byClass[m[1]] = append(sheet.byClass[m[1]], len(sheet.rules))
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
	for _, part := range splitDeclarations(body) {
		colon := strings.IndexByte(part, ':')
		if colon < 0 {
			continue
		}
		property := strings.ToLower(strings.TrimSpace(part[:colon]))
		// A presentation attribute has no notion of priority, so the marker
		// has to go — carrying it through would emit fill="#003764 !important",
		// which every browser discards, putting the element right back to the
		// default black this package exists to prevent.
		value := strings.TrimSpace(importantPattern.ReplaceAllString(part[colon+1:], ""))
		if property == "" || value == "" {
			continue
		}
		out = append(out, cssDeclaration{property: property, value: value})
	}
	return out
}

// splitDeclarations splits a rule body on semicolons that actually terminate a
// declaration, ignoring any inside a quoted string or a url(...) token. A
// naive split truncates font-family:"Foo;Bar" into a broken value.
func splitDeclarations(body string) []string {
	var (
		out    []string
		start  int
		quote  byte
		parens int
	)
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			parens++
		case c == ')':
			if parens > 0 {
				parens--
			}
		case c == ';' && parens == 0:
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	return append(out, body[start:])
}

// resolveClasses computes the presentation attributes contributed by the
// stylesheet to an element carrying the given class attribute value.
//
// Only rules whose class is actually referenced are inspected, via the byClass
// index. That matters twice over. Correctness: a real Illustrator export
// carries hundreds of dead rules copied from a shared master document (the
// Linux Foundation logo defines 304 classes and uses 2), and those routinely
// mention properties with no attribute equivalent — failing on them would
// reject nearly every genuine logo while changing nothing about how the file
// renders. Cost: scanning every rule for every element is quadratic in two
// attacker-controlled dimensions, which a 2 MB upload can turn into minutes of
// CPU on a pod with no request timeout.
func (s *stylesheet) resolveClasses(classAttr string) ([]cssDeclaration, error) {
	if s.empty() || strings.TrimSpace(classAttr) == "" {
		return nil, nil
	}

	// Collect matching rule indices, then apply them in source order so the
	// cascade does not depend on the order classes happen to appear in the
	// attribute.
	var matched []int
	seen := make(map[int]bool)
	for _, name := range strings.Fields(classAttr) {
		for _, i := range s.byClass[name] {
			if !seen[i] {
				seen[i] = true
				matched = append(matched, i)
			}
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	sort.Ints(matched)

	// Later declarations override earlier ones for the same property.
	// Insertion order is tracked separately so output is deterministic —
	// ranging a map would not be.
	winner := make(map[string]string)
	var order []string
	for _, i := range matched {
		rule := s.rules[i]
		for _, d := range rule.decls {
			switch {
			case inertProperties[d.property]:
				continue
			case !inlinableProperties[d.property]:
				return nil, fmt.Errorf("svgsanitize: unsupported CSS property %q in rule %q: cannot be represented as an SVG presentation attribute",
					truncateForError(d.property), truncateForError(rule.class))
			}
			if _, dup := winner[d.property]; !dup {
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
// interpolated into an error that surfaces to the uploader and into the
// request log, so a hostile file cannot use the message as an amplification
// channel. It cuts on a rune boundary so the result stays valid UTF-8.
func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	const max = 40
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
