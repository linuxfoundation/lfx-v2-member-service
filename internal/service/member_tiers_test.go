// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

func TestMembershipCountsAsActive(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name    string
		status  string
		endDate string
		want    bool
	}{
		{name: "active with no end date", status: "Active", want: true},
		// Salesforce picklist casing varies between orgs; casing must not drop members.
		{name: "status matches case-insensitively", status: "ACTIVE", want: true},
		{name: "non-active status", status: "Expired", endDate: "2099-12-31", want: false},
		{name: "empty status", status: "", want: false},
		{name: "end date in the past", status: "Active", endDate: "2026-03-14", want: false},
		// A membership stays active through its end date, not just until it.
		{name: "end date today still counts", status: "Active", endDate: "2026-03-15", want: true},
		{name: "end date in the future", status: "Active", endDate: "2027-01-01", want: true},
		{name: "RFC3339 end date in the past", status: "Active", endDate: "2020-01-01T00:00:00Z", want: false},
		// Status is the authority; a malformed date must not silently drop a member.
		{name: "unparseable end date does not deactivate", status: "Active", endDate: "03/15/2020", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &model.ProjectMembership{Status: tt.status, EndDate: tt.endDate}
			assert.Equal(t, tt.want, membershipCountsAsActive(m, now))
		})
	}
}

func TestMembershipOutranks(t *testing.T) {
	m := func(tierName, endDate string) *model.ProjectMembership {
		return &model.ProjectMembership{TierName: tierName, EndDate: endDate}
	}
	tests := []struct {
		name      string
		candidate *model.ProjectMembership
		current   *model.ProjectMembership
		want      bool
	}{
		{
			name:      "higher tier class wins regardless of end dates",
			candidate: m("Platinum Membership", "2026-01-01"),
			current:   m("Gold Membership", "2099-12-31"),
			want:      true,
		},
		{
			name:      "lower tier class never wins",
			candidate: m("Gold Membership", "2099-12-31"),
			current:   m("Platinum Membership", "2026-01-01"),
			want:      false,
		},
		{
			name:      "same class: later end date wins",
			candidate: m("Gold Membership", "2027-01-01"),
			current:   m("Gold Membership", "2026-01-01"),
			want:      true,
		},
		{
			name:      "same class: earlier end date loses",
			candidate: m("Gold Membership", "2026-01-01"),
			current:   m("Gold Membership", "2027-01-01"),
			want:      false,
		},
		{
			// An absent end date is treated as open-ended, so it outlasts any dated membership.
			name:      "same class: open-ended beats dated",
			candidate: m("Gold Membership", ""),
			current:   m("Gold Membership", "2099-01-01"),
			want:      true,
		},
		{
			// Equal on both criteria keeps the incumbent, making the winner deterministic.
			name:      "same class and end date keeps the incumbent",
			candidate: m("Gold Membership", "2026-01-01"),
			current:   m("Gold Membership", "2026-01-01"),
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, membershipOutranks(tt.candidate, tt.current))
		})
	}
}
