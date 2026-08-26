// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package svgsanitize_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/svgsanitize"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitize_AllowedContentPassesThrough(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" width="10" height="10">
		<title>Acme logo</title>
		<defs>
			<linearGradient id="g1"><stop offset="0" stop-color="#fff"/></linearGradient>
		</defs>
		<g fill="#123456" transform="translate(1,1)">
			<circle cx="5" cy="5" r="4" fill="url(#g1)"/>
			<use href="#g1"/>
		</g>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `xmlns="http://www.w3.org/2000/svg"`)
	assert.Contains(t, got, "<title>Acme logo</title>")
	assert.Contains(t, got, "linearGradient")
	assert.Contains(t, got, `fill="#123456"`)
	assert.Contains(t, got, `href="#g1"`)
	assert.Contains(t, got, "circle")
}

func TestSanitize_StripsScriptElement(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script><circle r="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "script")
	assert.NotContains(t, got, "alert")
	assert.Contains(t, got, "circle")
}

func TestSanitize_StripsEventHandlerAttributes(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect onclick="alert(2)" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "onload")
	assert.NotContains(t, got, "onclick")
	assert.NotContains(t, got, "alert")
	assert.Contains(t, got, "rect")
}

func TestSanitize_DropsForeignObjectSubtreeEntirely(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><body xmlns="http://www.w3.org/1999/xhtml" onload="alert(1)">hi</body></foreignObject><circle r="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "foreignObject")
	assert.NotContains(t, got, "onload")
	assert.NotContains(t, got, "hi")
	assert.Contains(t, got, "circle")
}

func TestSanitize_DropsImageElement(t *testing.T) {
	// <image> can load an arbitrary external URL from a document that's
	// supposed to be a self-contained logo — excluded entirely, not just its
	// href, since there's no safe form of an external fetch here.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><image href="https://evil.example/track.png"/><circle r="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "image")
	assert.NotContains(t, got, "evil.example")
	assert.Contains(t, got, "circle")
}

func TestSanitize_DropsNonFragmentHref(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">
		<use xlink:href="javascript:alert(1)"/>
		<use href="https://evil.example/x.svg#y"/>
		<use href="#local"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "javascript")
	assert.NotContains(t, got, "evil.example")
	assert.Contains(t, got, `href="#local"`)
}

func TestSanitize_DedupesHrefAndXlinkHrefOnSameElement(t *testing.T) {
	// encoding/xml reports both "href" and "xlink:href" with Name.Local ==
	// "href" (they differ only by namespace), so an element carrying both
	// would otherwise round-trip as two "href" attributes on one output tag
	// -- a duplicate, malformed attribute (Copilot/lfx-reviewer finding on PR
	// #87, 2026-08-18). The unprefixed form must win when both are present.
	in := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">
		<use xlink:href="#legacy" href="#modern"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Equal(t, 1, strings.Count(got, "href="), "at most one href attribute must survive on the element")
	assert.Contains(t, got, `href="#modern"`)
	assert.NotContains(t, got, "#legacy")
}

func TestSanitize_FallsBackToXlinkHrefWhenUnprefixedAbsent(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">
		<use xlink:href="#legacy"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Equal(t, 1, strings.Count(got, "href="))
	assert.Contains(t, got, `href="#legacy"`)
}

// assertNoDuplicateAttrs fails if any element in doc carries the same
// attribute name twice. This has to be asserted explicitly: duplicate
// attributes are a fatal well-formedness error to a conformant XML parser (so
// a browser refuses to render the logo), but encoding/xml accepts them
// silently, meaning a plain Sanitize-then-parse round trip in Go cannot catch
// the bug this guards against.
func assertNoDuplicateAttrs(t *testing.T, doc []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		require.NoError(t, err)
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		seen := make(map[string]bool, len(se.Attr))
		for _, a := range se.Attr {
			name := a.Name.Local
			assert.Falsef(t, seen[name], "element <%s> emits duplicate attribute %q", se.Name.Local, name)
			seen[name] = true
		}
	}
}

func TestSanitize_DedupesNamespacedAllowedAttributes(t *testing.T) {
	// Attributes are emitted unqualified, so a prefixed twin of an
	// allow-listed name collapses onto it. Before the fix these produced two
	// identically-named attributes on one tag -- fatal XML to a browser, and
	// invisible to encoding/xml (Copilot/lfx-reviewer finding on PR #87,
	// 2026-08-18). inkscape:/sodipodi: are the realistic trigger: design-tool
	// exports carry them on ordinary logos, so this is reachable without a
	// crafted payload.
	in := `<svg xmlns="http://www.w3.org/2000/svg">
		<rect x="1" ns:x="2" width="3" other:width="4"/>
		<rect fill="red" i:fill="blue" width="1" height="1"/>
		<g id="a" inkscape:id="b" sodipodi:role="c"></g>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assertNoDuplicateAttrs(t, out)

	got := string(out)
	// The unprefixed form wins, matching the href rule above.
	assert.Contains(t, got, `x="1"`)
	assert.NotContains(t, got, `x="2"`)
	assert.Contains(t, got, `width="3"`)
	assert.NotContains(t, got, `width="4"`)
	assert.Contains(t, got, `fill="red"`)
	assert.NotContains(t, got, `fill="blue"`)
	assert.Contains(t, got, `id="a"`)
	assert.NotContains(t, got, `id="b"`)
}

func TestSanitize_KeepsPrefixedAttributeWhenUnprefixedAbsent(t *testing.T) {
	// Mirrors TestSanitize_FallsBackToXlinkHrefWhenUnprefixedAbsent for the
	// general attribute path: a lone prefixed attribute still collapses to
	// its allow-listed unqualified name rather than being dropped.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><rect ns:fill="red" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assertNoDuplicateAttrs(t, out)
	assert.Contains(t, string(out), `fill="red"`)
}

func TestSanitize_AllowedContentHasNoDuplicateAttrs(t *testing.T) {
	// Regression guard on the ordinary happy path, so the invariant is held
	// for every element the sanitizer emits, not only the crafted cases.
	in := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" width="10" height="10">
		<defs><linearGradient id="g1"><stop offset="0" stop-color="#fff"/></linearGradient></defs>
		<rect x="0" y="0" width="10" height="10" fill="url(#g1)"/>
		<use href="#g1"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_DropsNonFragmentPaintURL(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">
		<linearGradient id="g"/>
		<rect fill="url(https://evil.example/track.svg#x)" width="1" height="1"/>
		<rect stroke="url('javascript:alert(1)')" width="1" height="1"/>
		<rect clip-path="url(#local)" width="1" height="1"/>
		<rect mask="url(#g) black" width="1" height="1"/>
		<rect fill="url(#g)" width="1" height="1"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "evil.example")
	assert.NotContains(t, got, "javascript")
	assert.Contains(t, got, `clip-path="url(#local)"`, "a bare same-document fragment reference must survive")
	assert.Contains(t, got, `fill="url(#g)"`, "a bare same-document fragment reference must survive")
	assert.NotContains(t, got, `mask=`, "a fragment reference with a trailing fallback value must be dropped, not partially rewritten")
}

func TestSanitize_DropsEscapedNonFragmentPaintURL(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">
		<linearGradient id="g"/>
		<rect fill="u\000072l(https://evil.example/track.svg#x)" width="1" height="1"/>
		<rect stroke="u\72l('https://evil.example/track.svg#y')" width="1" height="1"/>
		<rect clip-path="\75\72\6c(https://evil.example/track.svg#z)" width="1" height="1"/>
		<rect mask="u\72 l(https://evil.example/track.svg#w)" width="1" height="1"/>
		<rect fill="u\72l(javascript:alert(1))" width="1" height="1"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "evil.example")
	assert.NotContains(t, got, "javascript")
	assert.NotContains(t, got, "fill=")
	assert.NotContains(t, got, "stroke=")
	assert.NotContains(t, got, "clip-path=")
	assert.NotContains(t, got, "mask=")
}

func TestSanitize_KeepsEscapedSameDocumentFragmentPaintURL(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">
		<linearGradient id="g"/>
		<rect fill="u\000072l(#g)" width="1" height="1"/>
		<rect stroke="u\72l(#g)" width="1" height="1"/>
		<rect clip-path="\75\72\6c(#g)" width="1" height="1"/>
	</svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="u\000072l(#g)"`)
	assert.Contains(t, got, `stroke="u\72l(#g)"`)
	assert.Contains(t, got, `clip-path="\75\72\6c(#g)"`)
}

func TestSanitize_RejectsUnsafeStyleAttributeValue(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url(javascript:alert(1))" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe CSS value")
}

func TestSanitize_InlinesStyleAttribute(t *testing.T) {
	// Illustrator emits style="..." instead of a <style> block when its CSS
	// Properties setting is "Style Attributes" — same artwork, same all-black
	// outcome if the declarations are simply dropped.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><path style="fill:#009ADE;opacity:0.5" d="M0,0"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#009ADE"`)
	assert.Contains(t, got, `opacity="0.5"`)
	assert.NotContains(t, got, ` style=`)
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_StyleAttributeBeatsStylesheet(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#111111;}</style>` +
		`<rect class="a" style="fill:#222222" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	// A style attribute outranks an author stylesheet rule.
	assert.Contains(t, got, `fill="#222222"`)
	assert.NotContains(t, got, `fill="#111111"`)
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_InlinablePropertiesAreAllEmittable(t *testing.T) {
	// Every property the resolver accepts must also survive filterAttrs. A
	// property that passes the CSS gate but is not emittable would be dropped
	// silently — the exact failure this package exists to prevent. Two of
	// these, color and visibility, previously regressed that way: a dropped
	// color left fill="currentColor" resolving to black, and a dropped
	// visibility:hidden revealed artwork the author had hidden.
	for _, property := range []string{
		"color", "visibility", "overflow", "paint-order", "vector-effect",
		"isolation", "mix-blend-mode", "font-variant", "stroke-dashoffset",
		"shape-rendering", "text-decoration", "writing-mode",
	} {
		t.Run(property, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{` + property + `:inherit;}</style>` +
				`<rect class="a" width="1" height="1"/></svg>`

			out, err := svgsanitize.Sanitize([]byte(in))

			require.NoError(t, err)
			assert.Contains(t, string(out), property+`="inherit"`,
				"accepted by resolveClasses but dropped by filterAttrs")
		})
	}
}

func TestSanitize_RejectsStyleAttributeSettingGeometryOrIdentity(t *testing.T) {
	// The style attribute path needs the same gate as the stylesheet path:
	// these are XML attributes but not CSS properties, so honouring them would
	// erase path geometry or rewrite the id a url(#...) reference resolves
	// against.
	for _, decl := range []string{"d:none", "points:0", "id:hijack", "class:other", "transform:scale(99)", "width:9"} {
		t.Run(decl, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><path style="` + decl + `" d="M0 0 L9 9"/></svg>`

			_, err := svgsanitize.Sanitize([]byte(in))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsupported CSS property")
		})
	}
}

func TestSanitize_RejectsMarkerProperties(t *testing.T) {
	// <marker> is not allow-listed, so supporting marker-* would emit a
	// reference whose target has been stripped — arrowheads that silently
	// vanish. Failing closed keeps the contract that unrepresentable CSS is an
	// error rather than a wrong rendering.
	for _, source := range []string{
		`<style>.a{marker-end:url(#arrow);}</style><path class="a" d="M0,0"/>`,
		`<path style="marker-end:url(#arrow)" d="M0,0"/>`,
	} {
		in := `<svg xmlns="http://www.w3.org/2000/svg">` + source + `</svg>`

		_, err := svgsanitize.Sanitize([]byte(in))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported CSS property")
	}
}

func TestSanitize_DropsAuthoredMarkerAttribute(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><path marker-start="url(https://attacker.example/probe)" d="M0,0"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "attacker.example")
	assert.NotContains(t, got, "marker-start")
}

func TestSanitize_RejectsCustomPropertyDefinition(t *testing.T) {
	// Treating --brand as inert while keeping fill:var(--brand) would leave an
	// unresolvable reference, which computes to the initial value: black.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{--brand:#00FF00;fill:var(--brand);}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--brand")
}

func TestSanitize_StylesheetImportantBeatsStyleAttribute(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#111111 !important;}</style>` +
		`<rect class="a" style="fill:#222222" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	// An !important author rule outranks a style attribute; dropping the
	// priority would emit a colour no browser renders.
	assert.Contains(t, got, `fill="#111111"`)
	assert.NotContains(t, got, `fill="#222222"`)
}

func TestSanitize_AllowsCommentsInStyleAttribute(t *testing.T) {
	// Comments are legal in a style attribute's declaration list; rejecting
	// them on only that path would be an availability regression.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><rect style="/* brand */fill:#003764" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_AllowsEscapedQuoteInDeclarationValue(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{font-family:'It\'s';fill:#003764;}</style>` +
		`<text class="a">hi</text></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_UnterminatedCommentDoesNotAffectOtherStyleBlock(t *testing.T) {
	// The positive half of the isolation guarantee: concatenating blocks would
	// let the dangling /* in the second block comment out the first, so this
	// must still resolve. Asserting only that some error occurs would pass
	// against the concatenating implementation too.
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.b{fill:#009ADE;}</style><style>/* dangling */</style>` +
		`<rect class="b" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#009ADE"`)
}

func TestSanitize_DanglingSelectorCannotSpliceAcrossBlocks(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.a</style><style>{fill:#FF0000}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	// Neither block is a complete rule; inventing one across the boundary
	// would emit a colour no renderer would apply.
	require.Error(t, err)
}

func TestSanitize_IgnoresForeignNamespaceStyleElement(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:h="http://www.w3.org/1999/xhtml">` +
		`<h:style>.a{fill:#FF0000}</h:style><rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	// The element is dropped by the walk, so its CSS must not style the
	// output — nor reject the upload.
	require.NoError(t, err)
	assert.NotContains(t, string(out), `fill="#FF0000"`)
}

func TestSanitize_RejectsUnterminatedDeclarationString(t *testing.T) {
	// Recovering best-effort would swallow every following declaration into
	// one nonsensical value, silently losing the fill.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{font-family:"x;fill:#003764;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unterminated string")
}

func TestSanitize_AcceptsVendorPrefixedProperties(t *testing.T) {
	// Inkscape emits these; a browser ignores them, so rejecting the upload
	// would be an availability regression rather than a fidelity guarantee.
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.a{-inkscape-font-specification:'Sans';fill:#003764;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_BoundsErrorTextFromXMLDecoder(t *testing.T) {
	// Decoder errors embed element names taken straight from the document, so
	// they need the same bound as the CSS paths.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><` + strings.Repeat("z", 200_000) + `>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Less(t, len(err.Error()), 500, "decoder error text must be bounded")
}

func TestSanitize_StyleElementNeverReachesOutput(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#003764;}</style><rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "<style")
	// The declaration survives as a presentation attribute rather than being
	// discarded with the stylesheet — the whole point of inlining.
	assert.Contains(t, got, `fill="#003764"`)
}

func TestSanitize_InlinesClassRulesAsPresentationAttributes(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style type="text/css">.st211{fill:#009ADE;}.st212{fill:#003764;opacity:0.5;}</style>` +
		`<path class="st211" d="M0,0"/><path class="st212" d="M1,1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#009ADE"`)
	assert.Contains(t, got, `fill="#003764"`)
	assert.Contains(t, got, `opacity="0.5"`)
	assert.NotContains(t, got, "<style")
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_StylesheetRuleBeatsPresentationAttribute(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.brand{fill:#003764;}</style>` +
		`<rect class="brand" fill="red" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	// CSS outranks a presentation attribute in SVG, so inlining must not let
	// the element's own fill win.
	assert.Contains(t, got, `fill="#003764"`)
	assert.NotContains(t, got, `fill="red"`)
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_LaterRuleWinsWithinStylesheet(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#111111;}.a{fill:#222222;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#222222"`)
	assert.NotContains(t, got, `fill="#111111"`)
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_InlinesDisplayNoneSoHiddenArtworkStaysHidden(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.hidden{display:none;fill:#FFFFFF;}</style>` +
		`<rect class="hidden" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	// Dropping display:none would reveal layers the author hid — a different
	// corruption from the black-logo bug, in the opposite direction.
	assert.Contains(t, string(out), `display="none"`)
}

func TestSanitize_KeepsInlinedSameDocumentPaintURL(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.ok{fill:url(#SVGID_1_);}</style>` +
		`<rect class="ok" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="url(#SVGID_1_)"`)
}

func TestSanitize_RejectsInlinedExternalPaintURL(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.bad{fill:url(https://evil.example/t.svg#x);}</style>` +
		`<rect class="bad" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	// Dropping the declaration instead would render the element with no fill
	// — black — which is the failure this package exists to prevent, so an
	// unusable stylesheet value has to be loud.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe CSS value")
}

func TestSanitize_KeepsStylesheetDespiteTrailingBytes(t *testing.T) {
	// A single trailing byte used to abort stylesheet collection silently,
	// yielding a successfully "sanitized" document with every class rule
	// dropped — the all-black logo again, this time with no error at all.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#003764;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>&`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_IgnoresStyleAfterRoot(t *testing.T) {
	// Content past the root is not part of the document the sanitizer emits,
	// so letting it style the output would be a parser differential against
	// every real renderer.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><rect class="a" width="1" height="1"/></svg>` +
		`<style>.a{fill:#FF0000;}</style>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.NotContains(t, string(out), `fill="#FF0000"`)
}

func TestSanitize_StripsImportantPriority(t *testing.T) {
	// fill="#003764 !important" is not a valid presentation-attribute value;
	// browsers discard it and fall back to black.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:#003764 !important;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#003764"`)
	assert.NotContains(t, got, "important")
}

func TestSanitize_ReadsCSSFromLegacyCommentGuard(t *testing.T) {
	// Some exporters still wrap CSS in the HTML comment guard. Treating that
	// as an empty stylesheet would drop the document's styling silently.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style><!-- .a{fill:#003764;} --></style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_ReadsCSSFromCDATA(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style><![CDATA[.a{fill:#003764;}]]></style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_AppliesMultipleStyleBlocksInOrder(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.a{fill:#111111;}</style><style>.a{fill:#222222;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#222222"`)
	assert.NotContains(t, got, `fill="#111111"`)
}

func TestSanitize_UnterminatedCommentCannotSwallowNextStyleBlock(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>/* unterminated</style><style>.a{fill:#003764;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	// Blocks are kept separate, so the dangling comment fails closed on its
	// own block rather than consuming the following one.
	require.Error(t, err)
}

func TestSanitize_ResolvesMultipleClassesInRuleOrder(t *testing.T) {
	// Cascade order follows the stylesheet, not the order classes appear in
	// the attribute, so both orderings must resolve to the later rule.
	for _, classAttr := range []string{"a b", "b a"} {
		t.Run(classAttr, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg">` +
				`<style>.a{fill:#111111;}.b{fill:#222222;}</style>` +
				`<rect class="` + classAttr + `" width="1" height="1"/></svg>`

			out, err := svgsanitize.Sanitize([]byte(in))

			require.NoError(t, err)
			assert.Contains(t, string(out), `fill="#222222"`)
		})
	}
}

func TestSanitize_RejectsCSSSettingGeometryOrIdentity(t *testing.T) {
	// These are XML attributes but not CSS properties. No browser applies
	// them from a stylesheet; honouring them would erase path geometry or
	// rewrite the id a url(#...) reference resolves against.
	for _, decl := range []string{"d:none", "points:0", "id:hijack", "class:other", "transform:scale(99)", "width:9"} {
		t.Run(decl, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{` + decl + `;}</style>` +
				`<path class="a" d="M0 0 L9 9"/></svg>`

			_, err := svgsanitize.Sanitize([]byte(in))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsupported CSS property")
		})
	}
}

func TestSanitize_AcceptsPropertiesRealExportsEmit(t *testing.T) {
	// Illustrator routinely emits these alongside the colour rules; rejecting
	// them would turn a fidelity fix into an availability regression.
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.a{fill:#003764;overflow:visible;stroke-dashoffset:2;vector-effect:none;isolation:auto;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	// Assert each one individually: checking only fill would let a property
	// pass the CSS gate and then be dropped by filterAttrs unnoticed.
	assert.Contains(t, got, `fill="#003764"`)
	assert.Contains(t, got, `overflow="visible"`)
	assert.Contains(t, got, `stroke-dashoffset="2"`)
	assert.Contains(t, got, `vector-effect="none"`)
	assert.Contains(t, got, `isolation="auto"`)
}

func TestSanitize_KeepsQuotedSemicolonInDeclarationValue(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{font-family:"x;y";fill:#003764;}</style>` +
		`<text class="a">hi</text></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#003764"`)
	assert.NotContains(t, got, `font-family="&#34;x"`, "value must not be truncated at a quoted semicolon")
}

func TestSanitize_IgnoresStyleInDroppedSubtree(t *testing.T) {
	// <foreignObject> is discarded wholesale, so CSS inside it must neither
	// style the output nor reject the upload.
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<foreignObject><style>body{margin:0}</style></foreignObject>` +
		`<rect width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), "rect")
}

func TestSanitize_IgnoresNamespacedClassTwin(t *testing.T) {
	// filterAttrs emits the unprefixed class, so resolution must use the same
	// one — otherwise an element is styled from a class it does not carry.
	in := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:i="http://ns.adobe.com/AdobeIllustrator/10.0/">` +
		`<style>.evil{fill:#FF0000;}.good{fill:#00FF00;}</style>` +
		`<rect i:class="evil" class="good" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#00FF00"`)
	assert.NotContains(t, got, `fill="#FF0000"`)
}

func TestSanitize_HandlesDegenerateStylesheets(t *testing.T) {
	// Each case must either parse to a known result or fail closed — never
	// panic, hang, or silently produce an un-styled document.
	cases := map[string]struct {
		css       string
		wantError bool
	}{
		"empty":             {``, false},
		"whitespace only":   {"  \n\t ", false},
		"empty rule":        {`{}`, true},
		"leading brace":     {`}`, true},
		"double open":       {`{{`, true},
		"double close":      {`}}`, true},
		"unterminated rule": {`.a{fill:red`, true},
		"unterminated cmt":  {`/* .a{fill:red}`, true},
		"only selector":     {`.a`, true},
		"valid":             {`.a{fill:#003764}`, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><style>` + tc.css +
				`</style><rect class="a" width="1" height="1"/></svg>`

			var err error
			require.NotPanics(t, func() {
				_, err = svgsanitize.Sanitize([]byte(in))
			})
			if tc.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSanitize_BoundsAttackerControlledErrorText(t *testing.T) {
	// The error reaches the uploader and the request log, so a hostile file
	// must not be able to use it as an amplification channel.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{` +
		strings.Repeat("z", 200_000) + `:red;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Less(t, len(err.Error()), 500, "error text must be bounded")
}

func TestSanitize_IgnoresRulesNoElementReferences(t *testing.T) {
	// A real Illustrator export carries hundreds of dead rules copied from a
	// shared master document; the Linux Foundation logo defines 304 classes
	// and uses 2. Rejecting a file over a property in a rule that styles
	// nothing would reject nearly every genuine logo.
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.dead{some-unsupported-property:whatever;}.live{fill:#003764;}</style>` +
		`<rect class="live" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_RejectsUnsupportedPropertyOnReferencedRule(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.live{filter:blur(3px);}</style>` +
		`<rect class="live" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	// Fail closed: a rejected upload is recoverable, a silently miscoloured
	// logo is not.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported CSS property")
}

func TestSanitize_RejectsUnsupportedSelectors(t *testing.T) {
	cases := map[string]struct{ css, wantMsg string }{
		"type selector":       {`rect{fill:red}`, "unsupported CSS selector"},
		"id selector":         {`#logo{fill:red}`, "unsupported CSS selector"},
		"descendant selector": {`g rect{fill:red}`, "unsupported CSS selector"},
		"universal selector":  {`*{fill:red}`, "unsupported CSS selector"},
		"at-rule":             {`@media screen{.a{fill:red}}`, "at-rule"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><style>` + tc.css +
				`</style><rect class="a" width="1" height="1"/></svg>`

			_, err := svgsanitize.Sanitize([]byte(in))

			require.Error(t, err)
			// Assert the discriminating phrase: a generic "unsupported CSS"
			// check would pass even if the at-rule branch were deleted.
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestSanitize_IgnoresInertProperties(t *testing.T) {
	// enable-background was dropped from SVG 2 and is implemented by no
	// current browser, so discarding it cannot change rendering.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{enable-background:new;fill:#003764;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#003764"`)
	assert.NotContains(t, got, "enable-background")
}

func TestSanitize_StructuralErrorsOutrankStylesheetErrors(t *testing.T) {
	// A document that is both malformed and carries unsupported CSS must
	// still report the structural problem, which is the more fundamental one.
	in := `<!DOCTYPE svg><svg xmlns="http://www.w3.org/2000/svg"><style>rect{fill:red}</style></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DOCTYPE")
}

func TestSanitize_PreservesIllustratorLogoColours(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "lf-logo-illustrator.svg"))
	require.NoError(t, err)

	out, err := svgsanitize.Sanitize(in)

	require.NoError(t, err)
	got := string(out)
	// Regression guard for the production incident: this exact file was
	// sanitized to a valid, correctly-sized, entirely black logo.
	assert.Contains(t, got, `fill="#009ADE"`)
	assert.Contains(t, got, `fill="#003764"`)
	assert.NotContains(t, got, "<style")
	assert.Equal(t, 20, strings.Count(got, "fill="), "every classed element should carry an inlined fill")
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_PreservesMinimalIllustratorFixture(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "illustrator-style-minimal.svg"))
	require.NoError(t, err)

	out, err := svgsanitize.Sanitize(in)

	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, `fill="#009ADE"`)
	assert.Contains(t, got, `fill="#003764"`)
	assert.Contains(t, got, `display="none"`)
	assert.Contains(t, got, `clip-path="url(#SVGID_1_)"`)
	assert.NotContains(t, got, "<style")
	assertNoDuplicateAttrs(t, out)
}

func TestSanitize_StripsCommentsAndProcessingInstructions(t *testing.T) {
	in := `<?xml version="1.0"?><!-- comment with <script>alert(1)</script> --><svg xmlns="http://www.w3.org/2000/svg"><?foo bar?><circle r="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "comment")
	assert.NotContains(t, got, "script")
	assert.NotContains(t, got, "foo")
	assert.Contains(t, got, "circle")
}

func TestSanitize_RejectsDoctype(t *testing.T) {
	in := `<!DOCTYPE svg [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><svg xmlns="http://www.w3.org/2000/svg"><title>&xxe;</title></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DOCTYPE")
}

func TestSanitize_RejectsNonSVGRoot(t *testing.T) {
	in := `<html xmlns="http://www.w3.org/1999/xhtml"><body>hi</body></html>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "svg")
}

func TestSanitize_RejectsMalformedXML(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1"</svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
}

func TestSanitize_RejectsPlainText(t *testing.T) {
	_, err := svgsanitize.Sanitize([]byte("just some text, not xml at all"))

	require.Error(t, err)
}

func TestSanitize_RejectsUndefinedEntity(t *testing.T) {
	// No DOCTYPE at all, so this entity is never defined anywhere in the
	// document — encoding/xml only ever recognizes the five predefined XML
	// entities without one, so this must fail closed rather than silently
	// dropping or passing through the raw "&nbsp;" text.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><title>a&nbsp;b</title></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
}

func TestSanitize_RejectsDeeplyNestedSVG(t *testing.T) {
	// A small payload (a few hundred KB of 3-byte "<g>" tags) can encode far
	// more nesting than any real logo needs; without a depth bound this drives
	// sanitizeElement's recursion deep enough to risk stack exhaustion.
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg">`)
	for range 10_000 {
		b.WriteString("<g>")
	}
	b.WriteString(`<circle r="1"/>`)
	for range 10_000 {
		b.WriteString("</g>")
	}
	b.WriteString(`</svg>`)

	_, err := svgsanitize.Sanitize([]byte(b.String()))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nesting depth")
}

func TestSanitize_KeepsIDAndClassForDownstreamStyling(t *testing.T) {
	// id and class survive so a consuming page can still target the logo,
	// even though an embedded <style> can no longer reach them — its rules
	// are resolved into presentation attributes and the sheet is dropped.
	in := `<svg xmlns="http://www.w3.org/2000/svg" id="logo" class="brand"><rect id="bg" class="fill" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.True(t, strings.Contains(got, `id="logo"`) && strings.Contains(got, `class="brand"`))
	assert.True(t, strings.Contains(got, `id="bg"`) && strings.Contains(got, `class="fill"`))
}

func TestSanitize_EarlierImportantBeatsLaterNormalInStyleAttribute(t *testing.T) {
	// Within one declaration block a later declaration wins, except that it
	// cannot displace an earlier !important one. A reverse walk got this
	// backwards and emitted blue where every browser renders red.
	cases := map[string]struct{ style, want, notWant string }{
		"earlier important":  {`fill:red!important;fill:blue`, `fill="red"`, `fill="blue"`},
		"later important":    {`fill:blue;fill:red!important`, `fill="red"`, `fill="blue"`},
		"both important":     {`fill:red!important;fill:blue!important`, `fill="blue"`, `fill="red"`},
		"neither important":  {`fill:red;fill:blue`, `fill="blue"`, `fill="red"`},
		"important then two": {`fill:red!important;fill:blue;fill:green`, `fill="red"`, `fill="green"`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><rect style="` + tc.style + `" width="1" height="1"/></svg>`

			out, err := svgsanitize.Sanitize([]byte(in))

			require.NoError(t, err)
			got := string(out)
			assert.Contains(t, got, tc.want)
			assert.NotContains(t, got, tc.notWant)
		})
	}
}

func TestSanitize_StyleAttributeAndStylesheetAgreeOnImportant(t *testing.T) {
	// The same declarations must resolve identically whichever path carries
	// them; the two implementations previously disagreed.
	sheet := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{fill:red!important;fill:blue;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`
	inline := `<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:red!important;fill:blue" width="1" height="1"/></svg>`

	fromSheet, err := svgsanitize.Sanitize([]byte(sheet))
	require.NoError(t, err)
	fromInline, err := svgsanitize.Sanitize([]byte(inline))
	require.NoError(t, err)

	assert.Contains(t, string(fromSheet), `fill="red"`)
	assert.Contains(t, string(fromInline), `fill="red"`)
}

func TestSanitize_RejectsWhiteSpaceProperty(t *testing.T) {
	// SVG 2 makes white-space the whitespace mechanism for <text>, so pre and
	// pre-line change what renders. It cannot be emitted either — browsers do
	// not honour it as a presentation attribute — so it has to fail closed.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{white-space:pre;fill:#003764;}</style>` +
		`<text class="a">A  B</text></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "white-space")
}

func TestSanitize_RejectsUnknownVendorPrefixedProperty(t *testing.T) {
	// Blink honours these on SVG content, so discarding them silently changes
	// the artwork. Only design-tool prefixes are treated as inert.
	for _, property := range []string{"-webkit-clip-path", "-webkit-mask-image", "-webkit-filter", "-moz-box-shadow"} {
		t.Run(property, func(t *testing.T) {
			in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{` + property + `:none;fill:#003764;}</style>` +
				`<rect class="a" width="1" height="1"/></svg>`

			_, err := svgsanitize.Sanitize([]byte(in))

			require.Error(t, err)
			assert.Contains(t, err.Error(), property)
		})
	}
}

func TestSanitize_IgnoresDesignToolPrefixedProperties(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>.a{-inkscape-font-specification:'Sans';-sodipodi-role:line;fill:#003764;}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	assert.Contains(t, string(out), `fill="#003764"`)
}

func TestSanitize_RejectsOversizedDeclarationValue(t *testing.T) {
	// One large valid value is copied onto every element that references it,
	// so bounding it at source stops the highest-amplification shape.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>.a{font-family:` +
		strings.Repeat("Helvetica ", 1000) + `;}</style><rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestSanitize_RejectsOversizedStylesheet(t *testing.T) {
	// A selector list makes each extra rule three bytes, so without a cap a
	// small upload can define hundreds of thousands of rules.
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>` +
		strings.Repeat(".a,", 5000) + `.a{fill:red}</style>` +
		`<rect class="a" width="1" height="1"/></svg>`

	_, err := svgsanitize.Sanitize([]byte(in))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rules")
}

func TestSanitize_BoundsTotalResolutionWork(t *testing.T) {
	// Memoization alone cannot bound this: giving each element a distinct
	// class attribute — varying a dead class name is enough — means no cache
	// key ever repeats. A document-wide budget bounds the product regardless.
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg"><style>`)
	b.WriteString(strings.Repeat(".a,", 4000) + ".a{fill:red}")
	b.WriteString(`</style>`)
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, `<g class="a d%07d"></g>`, i)
	}
	b.WriteString(`</svg>`)

	start := time.Now()
	_, err := svgsanitize.Sanitize([]byte(b.String()))
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "too expensive")
	assert.Less(t, elapsed, 5*time.Second, "budget must stop the walk quickly")
}

func TestSanitize_BoundsOutputSize(t *testing.T) {
	// Inlining copies a declaration onto every referencing element, so output
	// is not bounded by input. The cap has to fire during encoding: reaching a
	// length check on the finished document means already allocating it.
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg"><style>.a{font-family:`)
	b.WriteString(strings.Repeat("Helvetica", 400))
	b.WriteString(`;}</style>`)
	for i := 0; i < 20000; i++ {
		b.WriteString(`<g class="a"></g>`)
	}
	b.WriteString(`</svg>`)

	out, err := svgsanitize.Sanitize([]byte(b.String()))

	require.Error(t, err)
	assert.Nil(t, out)
}
