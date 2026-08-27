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
// marker-start/mid/end are deliberately absent. Their value is a FuncIRI
// pointing at a <marker>, which is not in allowedElements and cannot cheaply
// be added — its geometry attributes (markerWidth, refX, orient, ...) are not
// allow-listed either, so admitting the element alone would render markers at
// the wrong size, anchor and angle, which is worse than rendering none.
// Keeping the properties unsupported means a referenced marker rule fails
// closed instead of emitting a reference whose target has been stripped.
//
// The reverse containment is a hard requirement and is asserted by init()
// below, not maintained by hand: a property that is inlinable but not
// emittable passes the fail-closed gate here and is then silently deleted by
// filterAttrs, which is the exact silent-corruption outcome this package
// exists to prevent.
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
	"pointer-events": true,
	"font-family":    true, "font-size": true, "font-weight": true,
	"font-style": true, "font-variant": true, "font-stretch": true,
	"text-anchor": true, "dominant-baseline": true, "letter-spacing": true,
	"text-decoration": true, "baseline-shift": true, "writing-mode": true,
}

// maxStylesheetRules bounds how many rules a document's stylesheets may expand
// to. A selector list makes each extra rule three bytes (".a,.a,.a{...}"), so
// without a cap a 2 MiB upload can define ~350k rules; combined with tens of
// thousands of elements referencing them, resolution reaches ~10^10 index
// visits. The Linux Foundation logo — the worked example throughout this
// package — defines 304, so four thousand leaves two orders of magnitude of
// headroom for anything a design tool legitimately emits.
const maxStylesheetRules = 4096

// maxResolveWork bounds the total work spent resolving classes across the
// whole document: every rule index looked up and every declaration visited is
// charged against it.
//
// Both dimensions are needed. Capping rules alone leaves the element count
// free, and capping index lookups alone leaves the declaration count free — a
// single rule with a very long body, referenced from many elements, reached
// hundreds of millions of declaration visits while spending 0.4% of the index
// budget. Memoization cannot substitute for either, since an attacker can give
// every element a distinct class attribute so no cache key ever repeats. One
// global budget bounds the product however the input distributes it.
const maxResolveWork = 1 << 20

// maxDeclarationValueBytes bounds one declaration's value. A single valid
// value of ~1 MiB referenced from tens of thousands of elements is copied onto
// each of them, so this is the cheapest place to stop that amplification at
// its source. No legitimate font-family or stroke-dasharray approaches it.
const maxDeclarationValueBytes = 4096

// init asserts that every inlinable property is also emittable. The two sets
// answer different questions — allowedAttributes gates what may reach the CDN,
// inlinableProperties gates what CSS may set — so neither derives from the
// other. But a property that is inlinable and not emittable passes the
// fail-closed gate and is then silently deleted by filterAttrs, which is the
// exact silent-corruption outcome this package exists to prevent. Panicking at
// process start turns that class of drift into an immediate, un-missable
// failure rather than a wrong logo discovered in production.
func init() {
	for property := range inlinableProperties {
		if !allowedAttributes[property] {
			panic("svgsanitize: inlinable property " + property + " is missing from allowedAttributes")
		}
	}
}

// inertProperties reach no SVG presentation attribute yet cannot change how a
// document renders, so dropping them is lossless and must not trip the
// fail-closed check. enable-background was removed from SVG 2 and is
// implemented by no current browser; cursor affects only interaction, which a
// static CDN image has none of.
//
// white-space is deliberately absent. SVG 2 makes it the whitespace-handling
// mechanism for <text> (§11.10.3.1, §11.6.1), so pre and pre-line change what
// renders. It cannot be inlined either: browsers do not honour white-space as
// a presentation attribute, so emitting one would pass every gate here and
// still do nothing — silent corruption relocated into the user agent rather
// than fixed. It therefore falls through to the unsupported branch and fails
// closed.
var inertProperties = map[string]bool{
	"enable-background": true,
	"cursor":            true,
}

// inertPropertyPrefixes are vendor prefixes belonging to design tools whose
// properties no browser implements, so discarding them cannot change
// rendering.
//
// This is an allow-list rather than a blanket "starts with a hyphen" test,
// because that test was wrong: Blink honours -webkit-clip-path,
// -webkit-mask-image, -webkit-opacity, -webkit-transform and -webkit-filter on
// SVG content, so treating every prefixed property as inert silently dropped
// declarations that visibly change the artwork. An unrecognised prefix now
// fails closed, and the error names the property, so a rejection is
// self-diagnosing and a re-export fixes it.
var inertPropertyPrefixes = []string{
	"-inkscape-",
	"-sodipodi-",
}

// isInertProperty reports whether a declaration can be discarded without
// changing what renders.
//
// A custom property ("--brand") is emphatically not inert: browsers honour it,
// and discarding one while keeping a var() that reads it leaves an
// unresolvable reference, which computes to the initial value — black for
// fill. That is this package's original bug wearing a different hat, so custom
// properties fail closed like any other unsupported declaration.
func isInertProperty(property string) bool {
	if strings.HasPrefix(property, "--") {
		return false
	}
	if inertProperties[property] {
		return true
	}
	for _, prefix := range inertPropertyPrefixes {
		if strings.HasPrefix(property, prefix) {
			return true
		}
	}
	return false
}

// knownValueKeywords are the bare identifiers a declaration value may legally
// be: the CSS-wide keywords, the SVG presentation-attribute keywords, and the
// named colours. It exists only to answer "could this identifier plausibly be
// valid", not to validate a value against its property's grammar — see
// isPlausibleValue.
var knownValueKeywords = map[string]bool{
	"aliceblue": true, "all": true, "alpha": true, "alphabetic": true, "antiquewhite": true,
	"aqua": true, "aquamarine": true, "arcs": true, "auto": true, "auto-sense": true,
	"auto-start-reverse": true, "azure": true, "baseline": true, "beige": true, "bevel": true,
	"bidi-override": true, "bisque": true, "black": true, "blanchedalmond": true, "blink": true,
	"block": true, "blue": true, "blueviolet": true, "bold": true, "bolder": true,
	"border-box": true, "bounding-box": true, "break-spaces": true, "brown": true,
	"burlywood": true, "butt": true, "cadetblue": true, "central": true, "chartreuse": true,
	"chocolate": true, "clip": true, "collapse": true, "color": true, "color-burn": true,
	"color-dodge": true, "condensed": true, "content-box": true, "context-fill": true,
	"context-stroke": true, "coral": true, "cornflowerblue": true, "cornsilk": true,
	"crimson": true, "crispedges": true, "currentcolor": true, "cursive": true, "cyan": true,
	"darkblue": true, "darkcyan": true, "darken": true, "darkgoldenrod": true, "darkgray": true,
	"darkgreen": true, "darkgrey": true, "darkkhaki": true, "darkmagenta": true,
	"darkolivegreen": true, "darkorange": true, "darkorchid": true, "darkred": true,
	"darksalmon": true, "darkseagreen": true, "darkslateblue": true, "darkslategray": true,
	"darkslategrey": true, "darkturquoise": true, "darkviolet": true, "deeppink": true,
	"deepskyblue": true, "difference": true, "dimgray": true, "dimgrey": true, "dodgerblue": true,
	"embed": true, "end": true, "evenodd": true, "exclusion": true, "expanded": true,
	"extra-condensed": true, "extra-expanded": true, "fantasy": true, "fill": true,
	"fill-box": true, "firebrick": true, "fixed-position": true, "floralwhite": true,
	"forestgreen": true, "fuchsia": true, "gainsboro": true, "geometricprecision": true,
	"ghostwhite": true, "gold": true, "goldenrod": true, "gray": true, "green": true,
	"greenyellow": true, "grey": true, "hanging": true, "hard-light": true, "hidden": true,
	"honeydew": true, "horizontal-tb": true, "hotpink": true, "hue": true, "ideographic": true,
	"indianred": true, "indigo": true, "inherit": true, "initial": true, "inline": true,
	"isolate": true, "isolate-override": true, "italic": true, "ivory": true, "khaki": true,
	"large": true, "larger": true, "lavender": true, "lavenderblush": true, "lawngreen": true,
	"lemonchiffon": true, "lightblue": true, "lightcoral": true, "lightcyan": true,
	"lighten": true, "lighter": true, "lightgoldenrodyellow": true, "lightgray": true,
	"lightgreen": true, "lightgrey": true, "lightpink": true, "lightsalmon": true,
	"lightseagreen": true, "lightskyblue": true, "lightslategray": true, "lightslategrey": true,
	"lightsteelblue": true, "lightyellow": true, "lime": true, "limegreen": true,
	"line-through": true, "linen": true, "lr": true, "lr-tb": true, "luminance": true,
	"luminosity": true, "magenta": true, "margin-box": true, "markers": true, "maroon": true,
	"mathematical": true, "medium": true, "mediumaquamarine": true, "mediumblue": true,
	"mediumorchid": true, "mediumpurple": true, "mediumseagreen": true, "mediumslateblue": true,
	"mediumspringgreen": true, "mediumturquoise": true, "mediumvioletred": true, "middle": true,
	"middle-anchor": true, "midnightblue": true, "mintcream": true, "mistyrose": true,
	"miter": true, "miter-clip": true, "moccasin": true, "monospace": true, "multiply": true,
	"navajowhite": true, "navy": true, "no-change": true, "non-rotation": true,
	"non-scaling-size": true, "non-scaling-stroke": true, "none": true, "nonzero": true,
	"normal": true, "nowrap": true, "oblique": true, "oldlace": true, "olive": true,
	"olivedrab": true, "optimizequality": true, "optimizespeed": true, "orange": true,
	"orangered": true, "orchid": true, "overlay": true, "overline": true, "padding-box": true,
	"painted": true, "palegoldenrod": true, "palegreen": true, "paleturquoise": true,
	"palevioletred": true, "papayawhip": true, "peachpuff": true, "peru": true, "pink": true,
	"plaintext": true, "plum": true, "powderblue": true, "pre": true, "pre-line": true,
	"pre-wrap": true, "purple": true, "rebeccapurple": true, "red": true, "reset-size": true,
	"revert": true, "rl": true, "rl-tb": true, "rosybrown": true, "round": true,
	"royalblue": true, "saddlebrown": true, "salmon": true, "sandybrown": true,
	"sans-serif": true, "saturation": true, "screen": true, "scroll": true, "seagreen": true,
	"seashell": true, "semi-condensed": true, "semi-expanded": true, "serif": true,
	"sienna": true, "silver": true, "skyblue": true, "slateblue": true, "slategray": true,
	"slategrey": true, "small": true, "small-caps": true, "smaller": true, "snow": true,
	"soft-light": true, "springgreen": true, "square": true, "start": true, "steelblue": true,
	"stroke": true, "stroke-box": true, "sub": true, "super": true, "system-ui": true,
	"tan": true, "tb": true, "tb-rl": true, "teal": true, "text-after-edge": true,
	"text-before-edge": true, "thistle": true, "tomato": true, "transparent": true,
	"turquoise": true, "ultra-condensed": true, "ultra-expanded": true, "underline": true,
	"unset": true, "use-script": true, "vertical-lr": true, "vertical-rl": true, "view-box": true,
	"violet": true, "visible": true, "visiblefill": true, "visiblepainted": true,
	"visiblestroke": true, "wheat": true, "white": true, "whitesmoke": true, "x-large": true,
	"x-small": true, "xx-large": true, "xx-small": true, "yellow": true, "yellowgreen": true,
}

// isPlausibleValue reports whether a value could legally apply.
//
// It is deliberately permissive: anything carrying a digit, a hash, a
// function, a percentage or a comma is accepted without inspection, so var(),
// color-mix(), oklch(), #009ADE, 2rem and space-separated rgb() all pass
// untouched. Only a bare identifier is checked, against the keyword table
// above. A conservative answer here costs nothing — the caller only consults
// it to decide whether a *displacing* declaration should be trusted — whereas
// validating values properly would mean rejecting every CSS addition this
// package has not been taught about.
func isPlausibleValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, "#(),%\"'/") {
		return true
	}
	for i := 0; i < len(value); i++ {
		if value[i] >= '0' && value[i] <= '9' {
			return true
		}
	}
	if strings.ContainsAny(value, " \t") {
		// A multi-token value (e.g. "underline solid") is beyond what the
		// keyword table describes; leave it to the browser.
		return true
	}
	return knownValueKeywords[strings.ToLower(value)]
}

// displaces reports whether next should override prev for the same property.
//
// A browser discards a declaration whose value is invalid, so an earlier valid
// declaration keeps winning: for .a{fill:#009ADE;fill:not-a-color} it renders
// blue, and for .a{display:none;display:bogus} the element stays hidden.
// Emitting the later value instead produced a black logo in the first case and
// revealed artwork the author had hidden in the second. Priority is checked
// first, since !important outranks source order either way.
func displaces(prev, next cssDeclaration) bool {
	if prev.important && !next.important {
		return false
	}
	if !isPlausibleValue(next.value) && isPlausibleValue(prev.value) {
		return false
	}
	return true
}

// styleRule is one parsed "selector { ... }" block. Declarations keep source
// order because a later declaration for the same property wins within a rule.
type styleRule struct {
	class string
	decls []cssDeclaration
}

// cssDeclaration is a single "property: value" pair. important records a
// !important priority, which the cascade needs even though the marker itself
// can never be emitted: a presentation attribute has no way to express it.
type cssDeclaration struct {
	property  string
	value     string
	important bool
}

// stylesheet is every rule parsed from a document's <style> elements, indexed
// by the class each rule selects. rules keeps source order, which is also
// cascade order for the equal-specificity selectors this parser accepts.
//
// resolved memoizes resolveClasses by class-attribute value. Without it, a
// document where many elements share a class that many rules select costs
// rules x elements — and a selector list makes each extra rule three bytes
// (".a,.a,.a{...}"), so a small upload can buy minutes of CPU on a pod with no
// request timeout. Keying on the attribute string bounds the work to the
// number of distinct class attributes in the document.
type stylesheet struct {
	rules    []styleRule
	byClass  map[string][]int
	resolved map[string][]cssDeclaration
	budget   int
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
	sheet := &stylesheet{byClass: make(map[string][]int), budget: maxResolveWork}
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
			if len(s.rules) >= maxStylesheetRules {
				return fmt.Errorf("svgsanitize: stylesheet defines more than %d rules", maxStylesheetRules)
			}
			s.byClass[m[1]] = append(s.byClass[m[1]], len(s.rules))
			s.rules = append(s.rules, styleRule{class: m[1], decls: decls})
		}
	}
}

// parseDeclarations splits a declaration list into property/value pairs. It
// serves both a rule body and a style attribute, so comment stripping lives
// here rather than in the block parser — comments are legal in either, and
// handling them on only one path would reject valid input on the other.
//
// Malformed fragments (no colon, empty property, empty value) are skipped
// rather than rejected: they contribute no styling, so dropping them changes
// nothing a browser would have rendered.
//
// Values are not validated against their property's grammar, because doing so
// would mean rejecting anything this package has not been taught about —
// var(), color-mix(), oklch(), space-separated rgb() and rem units all
// round-trip correctly today and would be casualties. What is checked is
// narrower: whether a value could plausibly apply at all, used only to stop an
// unrecognised identifier from displacing a valid earlier declaration for the
// same property. See isPlausibleValue and displaces.
//
// An invalid value that displaces nothing is still emitted verbatim, which is
// correct: the browser discards it and falls back to the initial value, which
// is exactly what it would have done with the original document.
func parseDeclarations(body string) ([]cssDeclaration, error) {
	parts, err := splitDeclarations(cssCommentPattern.ReplaceAllString(body, " "))
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
		// default black this package exists to prevent. The priority itself is
		// kept on the declaration, because the cascade still needs it.
		raw := part[colon+1:]
		stripped := importantPattern.ReplaceAllString(raw, "")
		value := strings.TrimSpace(stripped)
		if property == "" || value == "" {
			continue
		}
		if len(value) > maxDeclarationValueBytes {
			return nil, fmt.Errorf("svgsanitize: CSS value for %q exceeds %d bytes",
				truncateForError(property), maxDeclarationValueBytes)
		}
		out = append(out, cssDeclaration{
			property:  property,
			value:     value,
			important: len(stripped) != len(raw),
		})
	}
	return out, nil
}

// splitDeclarations splits a declaration list on semicolons that actually
// terminate a declaration, ignoring any inside a quoted string or a url(...)
// token. A naive split truncates font-family:"Foo;Bar" into a broken value.
//
// An unterminated quote or parenthesis is an error rather than a best-effort
// recovery: the remainder would otherwise be swallowed into one nonsensical
// value, silently dropping every declaration after it.
func splitDeclarations(body string) ([]string, error) {
	var (
		out    []string
		start  int
		quote  byte
		parens int
	)
	for i := 0; i < len(body); i++ {
		c := body[i]
		// A backslash escapes the next byte, so an apostrophe in a font name
		// (font-family:'It\'s') does not read as the end of the string.
		if c == '\\' {
			i++
			continue
		}
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
		return nil, fmt.Errorf("svgsanitize: unsupported CSS: unterminated string in %q", truncateForError(body))
	}
	if parens > 0 {
		return nil, fmt.Errorf("svgsanitize: unsupported CSS: unbalanced parentheses in %q", truncateForError(body))
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
	if cached, ok := s.resolved[classAttr]; ok {
		return cached, nil
	}

	// Collect matching rule indices, then apply them in source order so the
	// cascade does not depend on the order classes happen to appear in the
	// attribute. Every index visited is charged against a document-wide
	// budget: memoization alone cannot bound this, because an attacker can
	// give each element a distinct class attribute — varying a dead class name
	// is enough — so that no key ever repeats.
	var matched []int
	seen := make(map[int]bool)
	for _, name := range strings.Fields(classAttr) {
		indices := s.byClass[name]
		s.budget -= len(indices)
		if s.budget < 0 {
			return nil, fmt.Errorf("svgsanitize: stylesheet is too expensive to resolve: exceeded %d rule lookups", maxResolveWork)
		}
		for _, i := range indices {
			if !seen[i] {
				seen[i] = true
				matched = append(matched, i)
			}
		}
	}
	if len(matched) == 0 {
		s.memoize(classAttr, nil)
		return nil, nil
	}
	sort.Ints(matched)

	// Later declarations override earlier ones for the same property, except
	// that an !important one is not displaced by a normal one.
	winner := make(map[string]cssDeclaration)
	var order []string
	for _, i := range matched {
		rule := s.rules[i]
		// Charge the declarations too, not just the index lookup. Selectors
		// and declarations are independent dimensions — one rule with a very
		// long body reached hundreds of millions of visits while staying far
		// under the index budget — so the bound has to cover the product of
		// elements and total declarations, however the input distributes it.
		s.budget -= len(rule.decls)
		if s.budget < 0 {
			return nil, fmt.Errorf("svgsanitize: stylesheet is too expensive to resolve: exceeded %d lookups", maxResolveWork)
		}
		for _, d := range rule.decls {
			switch {
			case isInertProperty(d.property):
				continue
			case !inlinableProperties[d.property]:
				return nil, fmt.Errorf("svgsanitize: unsupported CSS property %q in rule %q: cannot be represented as an SVG presentation attribute",
					truncateForError(d.property), truncateForError(rule.class))
			}
			prev, dup := winner[d.property]
			if !dup {
				order = append(order, d.property)
			} else if !displaces(prev, d) {
				continue
			}
			winner[d.property] = d
		}
	}

	out := make([]cssDeclaration, 0, len(order))
	for _, property := range order {
		out = append(out, winner[property])
	}
	s.memoize(classAttr, out)
	return out, nil
}

// memoize records a resolved class attribute. Only successful resolutions are
// cached; a failure aborts the whole document, so there is nothing to reuse.
func (s *stylesheet) memoize(classAttr string, decls []cssDeclaration) {
	if s.resolved == nil {
		s.resolved = make(map[string][]cssDeclaration)
	}
	s.resolved[classAttr] = decls
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
