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

	withSlug := &B2BOrg{UID: "0014100000Te02DAAR", Slug: "google-llc", IsMember: true}
	assert.Equal(t,
		[]string{"0014100000Te02DAAR", "b2b_org_uid:0014100000Te02DAAR", "is_member:true", "slug:google-llc"},
		withSlug.Tags(),
	)

	noSlug := &B2BOrg{UID: "0014100000Te02DAAR"}
	for _, tag := range noSlug.Tags() {
		assert.NotContains(t, tag, "slug:", "no slug tag without a slug")
	}

	var nilOrg *B2BOrg
	assert.Nil(t, nilOrg.Tags())
}
