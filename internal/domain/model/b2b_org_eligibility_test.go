// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsB2BOrgEligible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		recordType      string
		hasMembership   bool
		wantWithCompany bool
	}{
		{"Company_Account", false, true},
		{"Company_Account", true, true},
		{"Partner_Restricted", false, false},
		{"Partner_Restricted", true, true},
		{"Project_Account", false, false},
		{"Project_Account", true, true},
		{"PersonAccount", false, false},
		{"PersonAccount", true, true},
		{"", false, false},
		{"", true, true},
	}
	for _, tt := range tests {
		got := IsB2BOrgEligible(tt.recordType, tt.hasMembership, true)
		assert.Equal(t, tt.wantWithCompany, got, "company accounts on: record type %q, membership %v", tt.recordType, tt.hasMembership)

		got = IsB2BOrgEligible(tt.recordType, tt.hasMembership, false)
		assert.Equal(t, tt.hasMembership, got, "company accounts off: record type %q, membership %v", tt.recordType, tt.hasMembership)
	}
}
