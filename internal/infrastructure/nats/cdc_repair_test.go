// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestRepairMarkerValue_LookupAttempts pins the marker value's backward
// compatibility: a marker written before lookup_attempts existed decodes as 0
// attempts, a fresh marker still encodes exactly as before, and a requeued
// marker round-trips its count.
func TestRepairMarkerValue_LookupAttempts(t *testing.T) {
	skippedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		stored       string
		wantAttempts int
	}{
		{name: "legacy marker without the field", stored: `{"skipped_at":"2026-10-01T00:00:00Z"}`, wantAttempts: 0},
		{name: "requeued marker", stored: `{"skipped_at":"2026-10-01T00:00:00Z","lookup_attempts":3}`, wantAttempts: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var val repairMarkerValue
			require.NoError(t, json.Unmarshal([]byte(tc.stored), &val))
			assert.Equal(t, skippedAt, val.SkippedAt)
			assert.Equal(t, tc.wantAttempts, val.LookupAttempts)

			encoded, err := json.Marshal(val)
			require.NoError(t, err)
			assert.JSONEq(t, tc.stored, string(encoded), "re-encoding must reproduce the stored form")
		})
	}
}
