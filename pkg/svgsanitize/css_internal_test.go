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
	for _, property := range []string{
		"fill", "stroke", "clip-path", "mask",
		"marker-start", "marker-mid", "marker-end",
	} {
		if !paintAttributes[property] {
			t.Errorf("FuncIRI property %q is not gated by isSafePaintValue", property)
		}
	}
}
