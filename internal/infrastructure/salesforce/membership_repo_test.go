// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contractAccountSFID15 is the 15-char form of contractAccountSFID, used to
// exercise the actual case-suffix widening branch of sfuuid.Normalize18
// rather than an 18-char input's pass-through identity. Kept as a literal
// (Go does not allow slicing a string constant) and checked for drift below
// against contractAccountSFID, the known-good 18-char Salesforce-issued
// value whose "QAF" suffix the 15-char case must reproduce.
const contractAccountSFID15 = "0012M00002FyYww"

// TestConvertSOQLToProjectMembership_AccountID covers the B2BOrgUID
// derivation branches off Asset.AccountId: empty, 15-char, 18-char, and
// malformed (PCC-1610 review fix — a malformed Account.Id must leave
// B2BOrgUID empty rather than falling back to the raw value, since
// B2BOrgUID is interpolated verbatim into FGA object refs and indexer
// parent refs in internal/service/messaging.go).
func TestConvertSOQLToProjectMembership_AccountID(t *testing.T) {
	require.True(t, strings.HasPrefix(contractAccountSFID, contractAccountSFID15),
		"contractAccountSFID15 must stay the 15-char prefix of contractAccountSFID")

	tests := []struct {
		name            string
		accountID       string
		wantB2BOrgUID   string
		wantAccountSFID string
	}{
		{
			name:            "empty AccountID leaves B2BOrgUID and AccountSFID empty",
			accountID:       "",
			wantB2BOrgUID:   "",
			wantAccountSFID: "",
		},
		{
			name:            "15-char AccountID is widened to its 18-char B2BOrgUID",
			accountID:       contractAccountSFID15,
			wantB2BOrgUID:   contractAccountSFID,
			wantAccountSFID: contractAccountSFID15,
		},
		{
			name:            "18-char AccountID passes through unchanged into B2BOrgUID",
			accountID:       contractAccountSFID,
			wantB2BOrgUID:   contractAccountSFID,
			wantAccountSFID: contractAccountSFID,
		},
		{
			name:            "malformed AccountID leaves B2BOrgUID empty but keeps raw AccountSFID",
			accountID:       "not-a-valid-sfid",
			wantB2BOrgUID:   "",
			wantAccountSFID: "not-a-valid-sfid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			soqlRec := soqlAsset{
				ID:               contractAssetSFID,
				Product2ID:       contractProduct2SFID,
				AccountID:        tt.accountID,
				CreatedDate:      "2025-01-01T00:00:00.000+0000",
				LastModifiedDate: "2025-03-20T00:00:00.000+0000",
			}

			m, err := convertSOQLToProjectMembership(t.Context(), soqlRec)
			require.NoError(t, err, "a bad AccountID must not fail the whole conversion")

			assert.Equal(t, tt.wantB2BOrgUID, m.B2BOrgUID)
			assert.Equal(t, tt.wantAccountSFID, m.AccountSFID, "AccountSFID keeps the raw value regardless — it is never published as an FGA/indexer ref")
		})
	}
}
