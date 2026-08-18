// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// TestValidateReindexType_AcceptsAllRegisteredTypes guards against the
// production CDCRepairStore silently rejecting a reindex type that a caller
// (the CDC consumer's quota-skip path or its delete_access failure marker)
// is allowed to write. A mock-backed test of the caller cannot catch this:
// only the real store's fixed allowlist enforces it.
func TestValidateReindexType_AcceptsAllRegisteredTypes(t *testing.T) {
	for _, reindexType := range []string{
		constants.ReindexTypeB2BOrg,
		constants.ReindexTypeProjectMembership,
		constants.ReindexTypeKeyContact,
		constants.ReindexTypeB2BOrgDeleteAccess,
		constants.ReindexTypeProjectMembershipDeleteAccess,
	} {
		assert.NoError(t, validateReindexType(reindexType), "type %q must be accepted", reindexType)
	}
}

func TestValidateReindexType_RejectsUnknownType(t *testing.T) {
	assert.Error(t, validateReindexType("b2b_org_settings"),
		"b2b_org_settings is backfill-only and has no CDC skip/repair path")
	assert.Error(t, validateReindexType("not_a_real_type"))
}

func TestValidateTarget_RejectsNonCanonicalSFID(t *testing.T) {
	assert.Error(t, validateTarget(constants.ReindexTypeB2BOrgDeleteAccess, "not-an-sfid"))
}
