// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package svgsanitize implements a strict allow-list SVG sanitizer for
// untrusted uploads (LFXV2-2016). No actively maintained, purpose-built Go
// SVG sanitizer exists — see ORG-LOGO-UPLOAD-PLAN-LFXV2-2016.md, Engineering
// Reference #3 — so this is a small one modeled on the allow-list design
// proven by darylldoyle/svg-sanitizer and its enshrined/svg-sanitize fork
// (both PHP), reimplemented on encoding/xml rather than ported line-for-line.
//
// The approach is default-deny: only elements and attributes on an explicit
// allow-list survive. <script>, <foreignObject>, <style>, <image>, every
// "on*" event handler, every href/xlink:href or fill/stroke/clip-path/mask
// url(...) reference that isn't a same-document "#fragment", comments,
// processing instructions, and DOCTYPE/ENTITY declarations are all dropped
// or rejected outright — not pattern-matched against a blocklist that could
// miss a variant.
package svgsanitize

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// allowedElements is the complete set of SVG elements a company logo needs:
// shapes, paths, grouping/reuse, gradients/clipping, and text. Nothing that
// can execute script or load an external resource is on this list — in
// particular, <script>, <foreignObject>, and <image> are deliberately
// excluded, not just their dangerous attributes.
var allowedElements = map[string]bool{
	"svg": true, "g": true, "defs": true, "symbol": true, "use": true,
	"path": true, "rect": true, "circle": true, "ellipse": true, "line": true,
	"polyline": true, "polygon": true,
	"text": true, "tspan": true, "textPath": true,
	"linearGradient": true, "radialGradient": true, "stop": true,
	"clipPath": true, "mask": true, "pattern": true,
	"title": true, "desc": true,
}

// textBearingElements may keep their CharData content; every other
// element's text content (typically just whitespace indentation between
// tags) is silently dropped.
var textBearingElements = map[string]bool{
	"text": true, "tspan": true, "textPath": true, "title": true, "desc": true,
}

// allowedAttributes is a presentation/geometry allow-list. Notably absent:
// "style" (CSS can carry url()/expression() payloads) and every "on*" event
// handler — excluded by construction here, not by name-matching a blocklist.
// href/xlink:href is handled separately by filterAttrs, since it's allowed
// only in its same-document "#fragment" form; fill/stroke/clip-path/mask are
// listed here (so a plain color or keyword value passes) but are also
// checked by isSafePaintValue, since their value can alternatively be a
// url(...) paint-server/filter reference that must be fragment-only too.
//
// stroke-miterlimit, clip-rule and display are present because an inlined
// stylesheet (see css.go) can carry them and silently dropping any of them
// changes what renders — display:none in particular hides artwork, so
// discarding it would reveal layers the author meant to stay hidden.
var allowedAttributes = map[string]bool{
	"id": true, "class": true, "transform": true,
	"viewBox": true, "width": true, "height": true, "preserveAspectRatio": true,
	"x": true, "y": true, "x1": true, "y1": true, "x2": true, "y2": true,
	"cx": true, "cy": true, "r": true, "rx": true, "ry": true,
	"points": true, "d": true, "offset": true,
	"fill": true, "fill-rule": true, "fill-opacity": true,
	"stroke": true, "stroke-width": true, "stroke-linecap": true,
	"stroke-linejoin": true, "stroke-dasharray": true, "stroke-opacity": true,
	"opacity": true, "stop-color": true, "stop-opacity": true,
	"gradientUnits": true, "gradientTransform": true,
	"patternUnits": true, "patternContentUnits": true, "patternTransform": true,
	"clip-path": true, "mask": true, "version": true,
	"font-family": true, "font-size": true, "font-weight": true, "font-style": true,
	"text-anchor": true, "dominant-baseline": true, "letter-spacing": true,
	// Presentation attributes that a stylesheet can also set. They are listed
	// explicitly rather than folded in from inlinableProperties, because this
	// map answers a security question — may this attribute reach the CDN —
	// and each entry needs that decision made deliberately. init() asserts the
	// containment instead of establishing it, so the two cannot drift apart.
	"stroke-miterlimit": true, "stroke-dashoffset": true,
	"clip-rule": true, "display": true, "visibility": true, "overflow": true,
	"color": true, "paint-order": true, "vector-effect": true, "isolation": true,
	"mix-blend-mode": true, "shape-rendering": true, "color-interpolation": true,
	"pointer-events": true, "font-variant": true, "font-stretch": true,
	"text-decoration": true, "baseline-shift": true, "writing-mode": true,
}

// svgNamespace is force-declared on the output root regardless of what (if
// anything) the input declared, so sanitized output is always well-formed.
const svgNamespace = "http://www.w3.org/2000/svg"

// maxSVGNestingDepth bounds sanitizeElement's recursion. A logo has no
// legitimate need for deep nesting; without a limit, a small payload of
// thousands of nested allow-listed elements (e.g. minimal 3-byte <g> tags)
// can drive the Go call stack to exhaustion.
const maxSVGNestingDepth = 100

// paintAttributes are the allow-listed attributes whose value can carry a
// CSS url(...) paint-server or filter reference — fill, stroke, clip-path,
// and mask all accept "url(#id)" pointing at a <linearGradient>, <pattern>,
// <clipPath>, or <mask> element. Unlike href, these aren't handled by
// filterAttrs' generic allow-list branch because their value is not itself a
// URL; a URL is only one substring a paint value can legally contain.
//
// Every property here has its target element allow-listed (linearGradient,
// radialGradient, pattern, clipPath, mask), so a surviving fragment reference
// always resolves. marker-* is excluded from the inlinable set for exactly
// that reason — see css.go.
var paintAttributes = map[string]bool{
	"fill": true, "stroke": true, "clip-path": true, "mask": true,
}

// urlFragmentPattern matches a value that is *only* a same-document
// "url(#fragment)" reference, optionally quoted and padded with whitespace —
// the one form of url(...) that can't name an external resource. Go's RE2
// engine has no backreferences, so this doesn't require matching quote
// characters to agree; the character class still excludes quotes, parens,
// and whitespace from the fragment itself, which is what actually matters
// for safety.
var urlFragmentPattern = regexp.MustCompile(`(?i)^url\(\s*['"]?#[^'"()\s]+['"]?\s*\)$`)

// isSafePaintValue rejects a paint/filter attribute value that contains a
// url(...) reference unless the entire value is a same-document fragment
// reference. Without this, fill="url(https://evil/track.svg#x)" (or
// stroke/clip-path/mask) would let a "sanitized" SVG still issue an outbound
// request or point at attacker-controlled markup — the same class of leak
// href's fragment-only rule exists to close, just via a different attribute.
// It normalizes CSS escape sequences first so that encoded variants such as
// u\000072l(...) cannot bypass the url(...) check.
func isSafePaintValue(v string) bool {
	norm := unescapeCSS(v)
	if !strings.Contains(strings.ToLower(norm), "url(") {
		return true
	}
	return urlFragmentPattern.MatchString(strings.TrimSpace(norm))
}

// unescapeCSS decodes CSS escape sequences (\XXXXXX or \char) from s.
func unescapeCSS(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			start := i
			for i < len(s) && (i-start) < 6 && isHex(s[i]) {
				i++
			}
			if i > start {
				hexStr := s[start:i]
				val, _ := strconv.ParseUint(hexStr, 16, 32)
				if val > 0 && val <= 0x10FFFF {
					b.WriteRune(rune(val))
				}
				// Optional trailing whitespace consumed per CSS spec.
				if i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f') {
					// consumed
				} else {
					i--
				}
				continue
			}
			if s[i] != '\r' && s[i] != '\n' && s[i] != '\f' {
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Sanitize parses data as XML, verifies its root element is <svg>, and
// rewrites it through the allow-lists above. It returns an error — never
// partially-sanitized bytes — for anything that isn't well-formed XML,
// doesn't have an <svg> root, or contains a DOCTYPE declaration.
//
// encoding/xml never resolves external entities or DTD-declared ones by
// default, so this is not the only thing standing between an XXE payload and
// the parser — but DOCTYPE is rejected outright anyway, as defense in depth
// against relying on that stdlib behavior remaining unchanged.
func Sanitize(data []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))

	root, err := findRoot(dec)
	if err != nil {
		return nil, err
	}

	// Structural validation above runs first so a DOCTYPE or non-svg root is
	// still reported as such rather than being pre-empted by a CSS complaint.
	// The stylesheet needs its own pass because <style> is a child element:
	// by the time the walk below reaches an element carrying class=, a single
	// streaming pass may not have read the rules that style it yet.
	sheet, err := extractStylesheet(data)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	enc := xml.NewEncoder(&boundedWriter{dst: &buf, remaining: maxSanitizedOutputBytes})
	if err := sanitizeElement(dec, enc, root, true, 0, sheet); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("svgsanitize: encoding output: %s", truncateForError(err.Error()))
	}
	return buf.Bytes(), nil
}

// maxSanitizedOutputBytes bounds the sanitized document.
//
// Inlining copies a declaration onto every element that references it, so
// output is not bounded by input: one large valid value referenced from many
// compact elements amplifies by orders of magnitude. Measured before this
// cap, a 169 KiB upload produced 512 MiB of output and peaked well past the
// pod's 512Mi limit. maxDeclarationValueBytes stops the worst shape at source;
// this is the general bound.
const maxSanitizedOutputBytes = 4 << 20

// boundedWriter fails the encode as soon as the output would exceed its limit.
//
// The check has to happen here rather than on the returned bytes: reaching a
// length check on the finished document requires materializing the whole thing
// first, which is precisely the allocation that exhausts the pod.
type boundedWriter struct {
	dst       *bytes.Buffer
	remaining int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("sanitized document exceeds %d bytes", maxSanitizedOutputBytes)
	}
	w.remaining -= len(p)
	return w.dst.Write(p)
}

// extractStylesheet collects the text of the <style> elements inside the root
// and parses it.
//
// Collection is scoped to the root subtree, and to subtrees the main pass
// keeps: a <style> after </svg>, or inside a dropped element such as
// <foreignObject>, is content the sanitizer otherwise treats as untrusted and
// discards, so letting it steer the output would be a parser differential
// against every real renderer. Scanning stops at the root's end element, which
// also means trailing bytes cannot influence the result.
//
// A decode failure inside that subtree is returned rather than swallowed.
// Swallowing it would silently produce an un-styled document — precisely the
// black-logo outcome this package exists to prevent — and the message matches
// what the main pass emits for the same input.
func extractStylesheet(data []byte) (*stylesheet, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))

	var blocks []string
	depth := 0

scan:
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("svgsanitize: parsing input: %s", truncateForError(err.Error()))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case depth == 0:
				depth = 1
			case isStyleElement(t.Name):
				if applyErr := styleBlockApplies(t.Attr); applyErr != nil {
					return nil, applyErr
				}
				text, textErr := collectStyleText(dec)
				if textErr != nil {
					return nil, textErr
				}
				blocks = append(blocks, text)
			case !allowedElements[t.Name.Local]:
				if skipErr := skipElement(dec); skipErr != nil {
					return nil, skipErr
				}
			default:
				depth++
			}
		case xml.EndElement:
			depth--
			if depth <= 0 {
				break scan
			}
		}
	}

	if len(blocks) == 0 {
		return &stylesheet{}, nil
	}
	return parseStylesheet(blocks)
}

// styleBlockApplies rejects a <style> element whose attributes mean its rules
// would not apply as written.
//
// Inlining bakes rules into presentation attributes unconditionally, so a
// block the browser would have skipped becomes permanent. Left ungated this
// does more than add styling: because stylesheet declarations are prepended,
// a rule from a never-matching block also displaces the element's own authored
// fill, and a display rule from one can reveal artwork the author hid.
//
// Only statically decidable cases are accepted. A feature-bearing query such
// as media="(min-width:100px)" resolves against the viewport of whatever page
// embeds the logo — the same bytes render differently at different sizes — so
// there is no correct answer at upload time and guessing would invent a
// rendering no renderer produces. Rejecting matches how parseStylesheet
// already treats an @media at-rule, which is the same concern in at-rule form.
//
// Matching follows HTML's "update a style block" algorithm, which is stricter
// for type than for media: type must be empty or exactly text/css ignoring
// case, with no parameters and no surrounding whitespace ("text/css;
// charset=utf-8" and " text/css " both fail), while media is trimmed and
// case-insensitive. screen is accepted because a logo rendered from a CDN is
// on a screen target; the residual divergence is that a media="screen" rule
// stays baked in when the embedding page is printed.
func styleBlockApplies(attrs []xml.Attr) error {
	for _, a := range attrs {
		if a.Name.Space != "" {
			continue
		}
		switch strings.ToLower(a.Name.Local) {
		case "type":
			if a.Value != "" && !strings.EqualFold(a.Value, "text/css") {
				return fmt.Errorf("svgsanitize: <style> has unsupported type %q", truncateForError(a.Value))
			}
		case "media":
			switch strings.ToLower(strings.TrimSpace(a.Value)) {
			case "", "all", "screen":
			default:
				return fmt.Errorf("svgsanitize: <style> has media %q, whose applicability cannot be preserved", truncateForError(a.Value))
			}
		}
	}
	return nil
}

// isStyleElement reports whether a start element is an SVG <style>.
//
// The namespace is checked, unlike the allowedElements lookups, because this
// is the one element whose *content* steers the output. A foreign-namespace
// twin such as an XHTML <h:style> is dropped by the walk, so honouring its
// CSS would style the document from markup the sanitizer discards — and,
// since unsupported selectors fail closed, could reject an upload outright
// over a stylesheet that never applied to the SVG in the first place.
func isStyleElement(name xml.Name) bool {
	return name.Local == "style" && (name.Space == "" || name.Space == svgNamespace)
}

// collectStyleText consumes an already-open <style> element and returns its
// text. Comments are included because the legacy
// "<style><!-- ... --></style>" guard is still emitted by some tools, and
// treating that CSS as absent would drop the document's styling without
// raising anything.
func collectStyleText(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("svgsanitize: parsing input: %s", truncateForError(err.Error()))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			b.Write(t)
		case xml.Comment:
			b.Write(t)
		}
	}
	return b.String(), nil
}

// stylesheetAttrs converts the CSS applying to an element into presentation
// attributes, in cascade order: the element's own style="..." outranks the
// document stylesheet, which in turn outranks its presentation attributes.
//
// The style attribute is handled here rather than dropped because Illustrator
// exports it instead of a <style> block whenever "CSS Properties" is set to
// "Style Attributes" — the same artwork, the same all-black result, just the
// other export mode. It is still never emitted: only the declarations that
// survive the inlinable-property and paint checks reach the output.
func stylesheetAttrs(sheet *stylesheet, attrs []xml.Attr) ([]xml.Attr, error) {
	// Only unqualified attributes drive resolution, because those are the ones
	// filterAttrs emits. Honouring a namespaced twin such as i:class would
	// style an element from a class it does not end up carrying.
	var class, inline string
	for _, a := range attrs {
		if a.Name.Space != "" {
			continue
		}
		switch a.Name.Local {
		case "class":
			if class == "" {
				class = a.Value
			}
		case "style":
			if inline == "" {
				inline = a.Value
			}
		}
	}

	sheetDecls, err := sheet.resolveClasses(class)
	if err != nil {
		return nil, err
	}
	inlineDecls, err := resolveInlineStyle(inline)
	if err != nil {
		return nil, err
	}
	if len(sheetDecls) == 0 && len(inlineDecls) == 0 {
		return nil, nil
	}

	// Cascade order: a style attribute outranks an author stylesheet rule,
	// unless that rule carries !important. Dropping the priority here would
	// emit the wrong colour for a document every browser renders differently.
	merged := make([]cssDeclaration, 0, len(inlineDecls)+len(sheetDecls))
	fromSheet := make(map[string]cssDeclaration, len(sheetDecls))
	for _, d := range sheetDecls {
		fromSheet[d.property] = d
	}
	claimed := make(map[string]bool, len(inlineDecls))
	for _, d := range inlineDecls {
		if sheet, ok := fromSheet[d.property]; ok && sheet.important && !d.important {
			continue
		}
		claimed[d.property] = true
		merged = append(merged, d)
	}
	for _, d := range sheetDecls {
		if !claimed[d.property] {
			merged = append(merged, d)
		}
	}

	out := make([]xml.Attr, 0, len(merged))
	for _, d := range merged {
		// filterAttrs drops a paint value that fails this check, which for a
		// CSS-derived one would mean silently rendering the element with no
		// fill — black. Every other unrepresentable stylesheet input is an
		// error, so this one is too.
		if paintAttributes[d.property] && !isSafePaintValue(d.value) {
			return nil, fmt.Errorf("svgsanitize: unsafe CSS value %q for property %q: url(...) references must be same-document fragments",
				truncateForError(d.value), d.property)
		}
		out = append(out, xml.Attr{Name: xml.Name{Local: d.property}, Value: d.value})
	}
	return out, nil
}

// resolveInlineStyle parses an element's style attribute under the same rules
// as a stylesheet rule body.
//
// The cascade within a single declaration block is the same one resolveClasses
// implements: a later declaration wins, except that it cannot displace an
// earlier !important one. Walking forward and applying that rule keeps the two
// paths from disagreeing about identical input — an earlier reverse-walk here
// emitted blue for style="fill:red!important;fill:blue" while the stylesheet
// path emitted red.
func resolveInlineStyle(style string) ([]cssDeclaration, error) {
	if strings.TrimSpace(style) == "" {
		return nil, nil
	}

	decls, err := parseDeclarations(style)
	if err != nil {
		return nil, err
	}

	winner := make(map[string]cssDeclaration, len(decls))
	var order []string
	for _, d := range decls {
		switch {
		case isInertProperty(d.property):
			continue
		case !inlinableProperties[d.property]:
			return nil, fmt.Errorf("svgsanitize: unsupported CSS property %q in style attribute: cannot be represented as an SVG presentation attribute",
				truncateForError(d.property))
		}
		prev, dup := winner[d.property]
		if !dup {
			order = append(order, d.property)
		} else if !displaces(prev, d) {
			continue
		}
		winner[d.property] = d
	}

	out := make([]cssDeclaration, 0, len(order))
	for _, property := range order {
		out = append(out, winner[property])
	}
	return out, nil
}

// findRoot advances past any leading XML declaration, comments, and
// whitespace, and returns the document's root element — rejecting anything
// else it finds first, in particular a DOCTYPE.
func findRoot(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return xml.StartElement{}, fmt.Errorf("svgsanitize: no root element found")
		}
		if err != nil {
			return xml.StartElement{}, fmt.Errorf("svgsanitize: parsing input: %s", truncateForError(err.Error()))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "svg" {
				return xml.StartElement{}, fmt.Errorf("svgsanitize: root element is %q, not svg", truncateForError(t.Name.Local))
			}
			return t.Copy(), nil
		case xml.Directive:
			return xml.StartElement{}, fmt.Errorf("svgsanitize: DOCTYPE declarations are not allowed")
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return xml.StartElement{}, fmt.Errorf("svgsanitize: unexpected content before root element")
			}
		}
		// xml.ProcInst (e.g. the <?xml ...?> declaration) and xml.Comment are
		// silently skipped.
	}
}

// sanitizeElement writes se (allow-listed attributes only) to enc, recurses
// into allow-listed children, drops disallowed children — and their entire
// subtree, not just the tag — and writes the matching end element. isRoot
// forces a fresh, known-safe xmlns onto <svg> regardless of what the input
// declared. depth is the current nesting level, checked against
// maxSVGNestingDepth to bound recursion.
func sanitizeElement(dec *xml.Decoder, enc *xml.Encoder, se xml.StartElement, isRoot bool, depth int, sheet *stylesheet) error {
	if depth > maxSVGNestingDepth {
		return fmt.Errorf("svgsanitize: exceeds maximum nesting depth of %d", maxSVGNestingDepth)
	}
	// Stylesheet-derived attributes are placed ahead of the element's own so
	// that filterAttrs' first-occurrence-wins dedupe reproduces the CSS
	// cascade, where an author rule outranks a presentation attribute. Routing
	// them through filterAttrs rather than appending to its result keeps them
	// subject to the same allow-list and isSafePaintValue checks as any
	// authored attribute.
	inherited, err := stylesheetAttrs(sheet, se.Attr)
	if err != nil {
		return err
	}
	attrs := filterAttrs(append(inherited, se.Attr...))
	if isRoot {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: svgNamespace})
	}
	name := xml.Name{Local: se.Name.Local}
	if err := enc.EncodeToken(xml.StartElement{Name: name, Attr: attrs}); err != nil {
		return fmt.Errorf("svgsanitize: encoding output: %s", truncateForError(err.Error()))
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("svgsanitize: parsing input: %s", truncateForError(err.Error()))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if allowedElements[t.Name.Local] {
				if err := sanitizeElement(dec, enc, t.Copy(), false, depth+1, sheet); err != nil {
					return err
				}
			} else if err := skipElement(dec); err != nil {
				return err
			}
		case xml.EndElement:
			if err := enc.EncodeToken(xml.EndElement{Name: name}); err != nil {
				return fmt.Errorf("svgsanitize: encoding output: %s", truncateForError(err.Error()))
			}
			return nil
		case xml.CharData:
			if textBearingElements[se.Name.Local] {
				if err := enc.EncodeToken(t.Copy()); err != nil {
					return fmt.Errorf("svgsanitize: encoding output: %s", truncateForError(err.Error()))
				}
			}
		}
		// xml.Comment, xml.ProcInst, and xml.Directive nested anywhere in the
		// document are silently dropped.
	}
}

// skipElement consumes tokens through the end of an already-open,
// not-allow-listed element, so the decoder position stays correct without
// ever emitting anything from its (potentially dangerous) subtree.
func skipElement(dec *xml.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("svgsanitize: parsing input: %s", truncateForError(err.Error()))
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// filterAttrs keeps only allow-listed presentation/geometry attributes, plus
// href/xlink:href when — and only when — it's a same-document "#fragment"
// reference. Every xmlns declaration is dropped here; the root's is re-added
// by the caller.
//
// Attributes are emitted unqualified, so any two inputs sharing a Name.Local
// collapse to the same output name regardless of their Name.Space. Every
// allow-listed name is therefore deduplicated, not just href: an element
// carrying both "fill" and "i:fill" (or the "inkscape:"/"sodipodi:" attributes
// real design-tool exports routinely emit) would otherwise produce two
// identically-named attributes on one tag. That is a fatal XML
// well-formedness error, so a browser fetching the sanitized logo renders a
// parse error instead of the image — note encoding/xml itself accepts
// duplicates, so this cannot be caught by round-tripping through Go
// (Copilot/lfx-reviewer finding on PR #87, 2026-08-18).
//
// The unprefixed form wins when both are present, since that is the real SVG
// presentation attribute and a prefixed twin is foreign metadata; otherwise
// the first occurrence is kept. href follows the same rule but is tracked
// separately, because it is additionally gated on being a same-document
// "#fragment" reference and is emitted last.
func filterAttrs(attrs []xml.Attr) []xml.Attr {
	var out []xml.Attr
	var href *xml.Attr
	// Local name -> index in out, and whether the kept entry was unprefixed.
	at := make(map[string]int, len(attrs))
	unprefixed := make(map[string]bool, len(attrs))

	keep := func(a xml.Attr) {
		if i, dup := at[a.Name.Local]; dup {
			if a.Name.Space == "" && !unprefixed[a.Name.Local] {
				out[i].Value = a.Value
				unprefixed[a.Name.Local] = true
			}
			return
		}
		at[a.Name.Local] = len(out)
		unprefixed[a.Name.Local] = a.Name.Space == ""
		out = append(out, xml.Attr{Name: xml.Name{Local: a.Name.Local}, Value: a.Value})
	}

	for _, a := range attrs {
		switch {
		case a.Name.Local == "xmlns" || a.Name.Space == "xmlns":
			continue
		case a.Name.Local == "href":
			if !isFragmentRef(a.Value) {
				continue
			}
			if href == nil || a.Name.Space == "" {
				href = &a
			}
		case paintAttributes[a.Name.Local]:
			if isSafePaintValue(a.Value) {
				keep(a)
			}
		case allowedAttributes[a.Name.Local]:
			keep(a)
		}
	}
	if href != nil {
		out = append(out, xml.Attr{Name: xml.Name{Local: "href"}, Value: href.Value})
	}
	return out
}

// isFragmentRef reports whether v is a same-document fragment reference
// ("#some-id") — the only href/xlink:href form <use>, gradients, clipPath,
// etc. need for a self-contained logo, and the only form that can't point at
// an external or javascript: resource.
func isFragmentRef(v string) bool {
	v = strings.TrimSpace(v)
	return strings.HasPrefix(v, "#") && len(v) > 1 && !strings.ContainsAny(v, " \t\r\n")
}
