// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSlugify pins the slug rule of spec 050 (DR-007) with the vectors from
// contracts/member-service-b2b-org-slug.md §1: every row is a real or derived
// production account name. A change here is an address change for existing
// organizations.
func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// legacy-dashboard parity (myorg.lfx.dev/{slug}/home)
		{name: "google", in: "Google LLC", want: "google-llc"},
		{name: "target", in: "Target Corporation", want: "target-corporation"},
		{name: "shenzhen", in: "Shenzhen Kaiyuan Gongchuang Technology Co., Ltd.", want: "shenzhen-kaiyuan-gongchuang-technology-co-ltd"},
		{name: "ampersand keeps legacy form", in: "AT&T", want: "at-t"},
		{name: "apostrophe keeps legacy form", in: "Macy's", want: "macy-s"},
		{name: "surrounding whitespace", in: "  3D Systems ", want: "3d-systems"},
		{name: "decoration collapses", in: "🌵 Needle", want: "needle"},

		// accent folding (the legacy rule deletes these letters)
		{name: "diaeresis", in: "Amoniac OÜ", want: "amoniac-ou"},
		{name: "grave", in: "Alma Mater Studiorum - Università di Bologna", want: "alma-mater-studiorum-universita-di-bologna"},
		{name: "sharp s", in: "Straße GmbH", want: "strasse-gmbh"},
		{name: "o slash", in: "Ørsted", want: "orsted"},
		{name: "l stroke", in: "Łódź", want: "lodz"},

		// scripts without a Latin rendering
		{name: "mixed script keeps the Latin part", in: "Alibaba (China) Co., Ltd. / 阿里巴巴（中国）有限公司", want: "alibaba-china-co-ltd"},
		{name: "cjk only yields no slug", in: "蓝芯算力", want: ""},

		// degenerate inputs
		{name: "hyphen only", in: "-", want: ""},
		{name: "empty", in: "", want: ""},
		{name: "sfid-shaped name yields no slug", in: "0014100000Te02DAAR", want: ""},

		// length
		{name: "word-boundary cut", in: "Asociación Colombiana de Informática Sistemas y Tecnologías Afines", want: "asociacion-colombiana-de-informatica-sistemas-y"},
		{name: "hard cut for a single long token", in: strings.Repeat("a", 60), want: strings.Repeat("a", 50)},
		{name: "exactly 50 is untouched", in: strings.Repeat("b", 50), want: strings.Repeat("b", 50)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Slugify(tt.in)
			assert.Equal(t, tt.want, got)
			if got != "" {
				assert.LessOrEqual(t, len(got), slugMaxLen)
				assert.Regexp(t, `^[a-z0-9]+(-[a-z0-9]+)*$`, got, "slug must be lowercase kebab with no edge or double hyphens")
			}
		})
	}
}

// TestSlugify_Idempotent guards the property the resolver relies on: lowercasing
// an address segment never changes what Slugify would have produced.
func TestSlugify_Idempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"Google LLC", "Straße GmbH", "Università di Bologna", "AT&T"} {
		once := Slugify(in)
		assert.Equal(t, once, Slugify(once), in)
		assert.Equal(t, once, Slugify(strings.ToUpper(in)), in)
	}
}
