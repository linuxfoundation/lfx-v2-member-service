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
//
// The reverse containment is a hard requirement and is established by init()
// below, not by keeping two lists in sync by hand: a property that is
// inlinable but not emittable passes the fail-closed gate here and is then
// silently deleted by filterAttrs, which is the exact silent-corruption
// outcome this package exists to prevent.
var inlinableProperties = map[string]bool{
	"fill": true, "fill-rule": true, "fill-opacity": true,
	"stroke": true, "stroke-width": true, "stroke-linecap": true,
	"stroke-linejoin": true, "stroke-dasharray": true, "stroke-dashoffset": true,
	"stroke-opacity": true, "stroke-miterlimit": true,
	"opacity": true, "stop-color": true, "stop-opacity": true,
	"clip-path": true, "clip-rule": true, "mask": true,
	"color": true, "display": true, "visibility": true, "overflow": true,
	"paint-order": true, "vector-effect": true, "isolation": true,
	"mix-blend-mode": true, "shape-rendering": true, "color-interpolation": true,
	"pointer-events": true, "marker-start": true, "marker-mid": true,
	"marker-end":  true,
	"font-family": true, "font-size": true, "font-weight": true,
	"font-style": true, "font-variant": true, "font-stretch": true,
	"text-anchor": true, "dominant-baseline": true, "letter-spacing": true,
	"text-decoration": true, "baseline-shift": true, "writing-mode": true,
}

// init makes allowedAttributes a superset of inlinableProperties by
// construction, so the two sets cannot drift apart in a later edit. Every
// entry is a genuine SVG presentation attribute carrying an inert
// keyword/number/colour value, or a paint attribute already gated by
// isSafePaintValue, so admitting them as authored attributes too is safe.
func init() {
	for property := range inlinableProperties {
		allowedAttributes[property] = true
	}
}

// inertProperties reach no SVG presentation attribute yet cannot change how a
// document renders, so dropping them is lossless and must not trip the
// fail-closed check. enable-background was removed from SVG 2 and is
// implemented by no current browser; the others are editor hints or affect
// only interactive presentation, and Inkscape and Illustrator emit them
// routinely alongside the colour rules.
var inertProperties = map[string]bool{
	"enable-background": true,
	"cursor":            true,
	"white-space":       true,
}

// isInertProperty reports whether a declaration can be discarded without
// changing what renders. Vendor-prefixed properties qualify because a browser
// that does not recognise the prefix ignores them too — and rejecting an
// upload over one would turn a fidelity fix into an availability regression.
func isInertProperty(property string) bool {
	return inertProperties[property] || strings.HasPrefix(property, "-")
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

// parseStylesheet parses each <style> element's text into rules indexed by
// class, in document order.
//
// Blocks are parsed independently rather than concatenated, because a browser
// treats every <style> element as its own stylesheet. Joining them first lets
// an unterminated /* in one block comment out the next, and lets a dangling
// selector in one splice onto a body in another — both inventing a rendering
// no renderer would produce, in the silent direction.
//
// It rejects — rather than ignores — any selector it does not understand.
// Ignoring one would mean emitting an SVG whose styling silently differs from
// the author's intent, which is the exact failure mode this file exists to
// prevent. Unknown *properties* get the opposite treatment: they are carried
// through to resolveClasses, which only rejects them if the rule they belong
// to is actually referenced by an element (see resolveClasses).
func parseStylesheet(blocks []string) (*stylesheet, error) {
	sheet := &stylesheet{byClass: make(map[string][]int)}
	for _, block := range blocks {
		if err := sheet.parseBlock(block); err != nil {
			return nil, err
		}
	}
	return sheet, nil
}

// parseBlock parses one <style> element's text into the sheet.
func (s *stylesheet) parseBlock(css string) error {
	css = cssCommentPattern.ReplaceAllString(css, " ")

	for rest := css; ; {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			// Trailing text after the last rule is only acceptable if it is
			// whitespace; anything else means a truncated or unparsable sheet.
			if strings.TrimSpace(rest) != "" {
				return fmt.Errorf("svgsanitize: unsupported CSS in <style>: trailing content %q", truncateForError(rest))
			}
			return nil
		}
		closeIdx := strings.IndexByte(rest[open:], '}')
		if closeIdx < 0 {
			return fmt.Errorf("svgsanitize: unsupported CSS in <style>: unterminated rule")
		}
		closeIdx += open

		selector := strings.TrimSpace(rest[:open])
		decls, err := parseDeclarations(rest[open+1 : closeIdx])
		if err != nil {
			return err
		}
		rest = rest[closeIdx+1:]

		// An at-rule (@media, @import, @font-face, ...) changes which
		// declarations apply, or pulls in an external sheet. Neither can be
		// represented as a presentation attribute.
		if strings.HasPrefix(selector, "@") {
			return fmt.Errorf("svgsanitize: unsupported CSS at-rule %q in <style>", truncateForError(selector))
		}

		for _, one := range strings.Split(selector, ",") {
			one = strings.TrimSpace(one)
			m := classSelectorPattern.FindStringSubmatch(one)
			if m == nil {
				return fmt.Errorf("svgsanitize: unsupported CSS selector %q in <style>: only simple class selectors are supported", truncateForError(one))
			}
			s.byClass[m[1]] = append(s.byClass[m[1]], len(s.rules))
			s.rules = append(s.rules, styleRule{class: m[1], decls: decls})
		}
	}
}

// parseDeclarations splits a rule body into property/value pairs. Malformed
// fragments (no colon, empty property, empty value) are skipped rather than
// rejected: they contribute no styling, so dropping them changes nothing a
// browser would have rendered.
func parseDeclarations(body string) ([]cssDeclaration, error) {
	parts, err := splitDeclarations(body)
	if err != nil {
		return nil, err
	}

	var out []cssDeclaration
	for _, part := range parts {
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
	return out, nil
}

// splitDeclarations splits a rule body on semicolons that actually terminate a
// declaration, ignoring any inside a quoted string or a url(...) token. A
// naive split truncates font-family:"Foo;Bar" into a broken value.
//
// An unterminated quote or parenthesis is an error rather than a best-effort
// recovery: the remainder of the rule would otherwise be swallowed into one
// nonsensical value, silently dropping every declaration after it.
func splitDeclarations(body string) ([]string, error) {
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
	if quote != 0 {
		return nil, fmt.Errorf("svgsanitize: unsupported CSS in <style>: unterminated string in %q", truncateForError(body))
	}
	if parens > 0 {
		return nil, fmt.Errorf("svgsanitize: unsupported CSS in <style>: unbalanced parentheses in %q", truncateForError(body))
	}
	return append(out, body[start:]), nil
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
			case isInertProperty(d.property):
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

// truncateForError bounds a fragment of attacker-controlled input before it is
// interpolated into an error that surfaces to the uploader and into the
// request log, so a hostile file cannot use the message as an amplification
// channel. It applies to decoder errors too, since those embed element and
// entity names taken straight from the document. It cuts on a rune boundary so
// the result stays valid UTF-8.
func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	const max = 80
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
