// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package svgsanitize

import "testing"

// TestInlinablePropertiesAreEmittable enumerates the unexported sets directly,
// which the external test package cannot do. init() panics on a violation, so
// this mainly documents the invariant and pins the exact membership — a
// property that is inlinable but not emittable passes the fail-closed gate and
// is then silently dropped by filterAttrs.
func TestInlinablePropertiesAreEmittable(t *testing.T) {
	for property := range inlinableProperties {
		if !allowedAttributes[property] {
			t.Errorf("inlinable property %q is missing from allowedAttributes", property)
		}
	}
}

// TestFuncIRIPropertiesArePaintGated pins the properties whose value is a
// FuncIRI. An ungated one lets a sanitized document on a public CDN name an
// attacker-controlled resource.
func TestFuncIRIPropertiesArePaintGated(t *testing.T) {
	for _, property := range []string{"fill", "stroke", "clip-path", "mask"} {
		if !paintAttributes[property] {
			t.Errorf("FuncIRI property %q is not gated by isSafePaintValue", property)
		}
	}
}

// TestPaintTargetsAreAllowedElements is the invariant that keeps marker-*
// out of the inlinable set: a paint property may only be supported when the
// element its FuncIRI points at survives sanitizing. Otherwise a valid
// reference is emitted whose target has been stripped.
func TestPaintTargetsAreAllowedElements(t *testing.T) {
	targets := map[string][]string{
		"fill":      {"linearGradient", "radialGradient", "pattern"},
		"stroke":    {"linearGradient", "radialGradient", "pattern"},
		"clip-path": {"clipPath"},
		"mask":      {"mask"},
	}
	for property := range paintAttributes {
		elements, known := targets[property]
		if !known {
			t.Errorf("paint property %q has no declared target element; confirm the target survives sanitizing before supporting it", property)
			continue
		}
		for _, element := range elements {
			if !allowedElements[element] {
				t.Errorf("paint property %q targets %q, which is not in allowedElements", property, element)
			}
		}
	}
}

// TestMarkerPropertiesAreUnsupported pins the decision rather than leaving it
// implicit. <marker> is not allow-listed and its geometry attributes are not
// either, so supporting the properties would emit a dangling reference, and
// admitting the element alone would render markers at the wrong size, anchor
// and angle — worse than rendering none.
func TestMarkerPropertiesAreUnsupported(t *testing.T) {
	for _, property := range []string{"marker-start", "marker-mid", "marker-end"} {
		if inlinableProperties[property] || allowedAttributes[property] {
			t.Errorf("%q is supported but <marker> is not sanitized; see css.go", property)
		}
	}
}
