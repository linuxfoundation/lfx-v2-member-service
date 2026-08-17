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
	"fmt"
	"io"
	"regexp"
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
func isSafePaintValue(v string) bool {
	if !strings.Contains(strings.ToLower(v), "url(") {
		return true
	}
	return urlFragmentPattern.MatchString(strings.TrimSpace(v))
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

	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	if err := sanitizeElement(dec, enc, root, true, 0); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("svgsanitize: encoding output: %w", err)
	}
	return buf.Bytes(), nil
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
			return xml.StartElement{}, fmt.Errorf("svgsanitize: parsing input: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "svg" {
				return xml.StartElement{}, fmt.Errorf("svgsanitize: root element is %q, not svg", t.Name.Local)
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
func sanitizeElement(dec *xml.Decoder, enc *xml.Encoder, se xml.StartElement, isRoot bool, depth int) error {
	if depth > maxSVGNestingDepth {
		return fmt.Errorf("svgsanitize: exceeds maximum nesting depth of %d", maxSVGNestingDepth)
	}
	attrs := filterAttrs(se.Attr)
	if isRoot {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: svgNamespace})
	}
	name := xml.Name{Local: se.Name.Local}
	if err := enc.EncodeToken(xml.StartElement{Name: name, Attr: attrs}); err != nil {
		return fmt.Errorf("svgsanitize: encoding output: %w", err)
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("svgsanitize: parsing input: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if allowedElements[t.Name.Local] {
				if err := sanitizeElement(dec, enc, t.Copy(), false, depth+1); err != nil {
					return err
				}
			} else if err := skipElement(dec); err != nil {
				return err
			}
		case xml.EndElement:
			if err := enc.EncodeToken(xml.EndElement{Name: name}); err != nil {
				return fmt.Errorf("svgsanitize: encoding output: %w", err)
			}
			return nil
		case xml.CharData:
			if textBearingElements[se.Name.Local] {
				if err := enc.EncodeToken(t.Copy()); err != nil {
					return fmt.Errorf("svgsanitize: encoding output: %w", err)
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
			return fmt.Errorf("svgsanitize: parsing input: %w", err)
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
func filterAttrs(attrs []xml.Attr) []xml.Attr {
	var out []xml.Attr
	for _, a := range attrs {
		switch {
		case a.Name.Local == "xmlns" || a.Name.Space == "xmlns":
			continue
		case a.Name.Local == "href":
			if isFragmentRef(a.Value) {
				out = append(out, xml.Attr{Name: xml.Name{Local: "href"}, Value: a.Value})
			}
		case paintAttributes[a.Name.Local]:
			if isSafePaintValue(a.Value) {
				out = append(out, xml.Attr{Name: xml.Name{Local: a.Name.Local}, Value: a.Value})
			}
		case allowedAttributes[a.Name.Local]:
			out = append(out, xml.Attr{Name: xml.Name{Local: a.Name.Local}, Value: a.Value})
		}
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
