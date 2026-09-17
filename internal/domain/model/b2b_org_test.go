// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestB2BOrg_Tags_Slug verifies the `slug:{slug}` search tag that lets Org Lens
// resolve `/org/{slug}/…` through the access-filtered query-service
// (lfx-self-serve#2570). The tag is present iff the org has a slug and is
// emitted verbatim — lowercasing happens at Salesforce ingest, not here.
func TestB2BOrg_Tags_Slug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		org  *B2BOrg
		want []string
	}{
		{
			name: "slug present emits slug tag last",
			org:  &B2BOrg{UID: "0014100000Te02DAAR", Slug: "google-llc", IsMember: true},
			want: []string{"0014100000Te02DAAR", "b2b_org_uid:0014100000Te02DAAR", "is_member:true", "slug:google-llc"},
		},
		{
			name: "slug absent emits no slug tag",
			org:  &B2BOrg{UID: "0014100000Te02DAAR"},
			want: []string{"0014100000Te02DAAR", "b2b_org_uid:0014100000Te02DAAR", "is_member:false"},
		},
		{
			name: "slug and parent both present",
			org:  &B2BOrg{UID: "child", ParentUID: "parent", Slug: "child-co"},
			want: []string{"child", "b2b_org_uid:child", "parent_b2b_org_uid:parent", "is_member:false", "slug:child-co"},
		},
		{
			name: "nil receiver yields nil",
			org:  nil,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.org.Tags())
		})
	}
}
