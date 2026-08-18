// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package svgsanitize_test

import (
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
