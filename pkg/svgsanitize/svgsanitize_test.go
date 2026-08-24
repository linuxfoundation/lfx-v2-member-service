// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package svgsanitize_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"

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

func TestSanitize_StripsStyleElementAndAttribute(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg"><style>rect{fill:url(javascript:alert(1))}</style><rect style="fill:red" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.NotContains(t, got, "<style")
	assert.NotContains(t, got, "javascript")
	assert.NotContains(t, got, `style=`)
	assert.Contains(t, got, "rect")
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

func TestSanitize_KeepsIDAndClassForCSSTargeting(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" id="logo" class="brand"><rect id="bg" class="fill" width="1" height="1"/></svg>`

	out, err := svgsanitize.Sanitize([]byte(in))

	require.NoError(t, err)
	got := string(out)
	assert.True(t, strings.Contains(got, `id="logo"`) && strings.Contains(got, `class="brand"`))
	assert.True(t, strings.Contains(got, `id="bg"`) && strings.Contains(got, `class="fill"`))
}
