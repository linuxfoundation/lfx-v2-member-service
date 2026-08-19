// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"os"
	"strings"
	"testing"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Handler tests (AdminReindex endpoint) ──────────────────────────────────

func TestAdminReindex_AcceptsFullReindexAndReturnsRunID(t *testing.T) {
	runner := newTestBackfillRunner(nil)
	svc := newTestSvc(withBackfillRunner(runner))

	result, err := svc.AdminReindex(context.Background(), &membershipservice.AdminReindexPayload{Type: "b2b_org"})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.RunID, "run_id must be a non-empty UUID")
}

func TestAdminReindex_Validation(t *testing.T) {
	since := "2026-05-01T00:00:00Z"
	offsetSince := "2026-05-01T00:00:00-07:00"
	invalidSince := "not-a-date"
	naiveSince := "2026-05-01 00:00:00"

	tests := []struct {
		name       string
		payload    *membershipservice.AdminReindexPayload
		wantErrMsg string
	}{
		{
			name:       "unknown type rejected",
			payload:    &membershipservice.AdminReindexPayload{Type: "foobar"},
			wantErrMsg: "unknown type",
		},
		{
			name:       "empty type rejected (required singular type)",
			payload:    &membershipservice.AdminReindexPayload{Type: ""},
			wantErrMsg: "unknown type",
		},
		{
			name:       "membership_tier rejected with helpful message",
			payload:    &membershipservice.AdminReindexPayload{Type: "membership_tier"},
			wantErrMsg: "membership_tier is not currently supported",
		},
		{
			name: "cdc_repair + items mutually exclusive",
			payload: &membershipservice.AdminReindexPayload{
				Type:      "b2b_org",
				CdcRepair: true,
				Items:     []*membershipservice.AdminReindexItem{{UID: "00000000-0000-0000-0000-000000000001"}},
			},
			wantErrMsg: "mutually exclusive",
		},
		{
			name: "items + since mutually exclusive",
			payload: &membershipservice.AdminReindexPayload{
				Type:  "b2b_org",
				Since: &since,
				Items: []*membershipservice.AdminReindexItem{{UID: "00000000-0000-0000-0000-000000000001"}},
			},
			wantErrMsg: "mutually exclusive",
		},
		{
			name: "item with invalid Salesforce ID rejected",
			payload: &membershipservice.AdminReindexPayload{
				Type:  "b2b_org",
				Items: []*membershipservice.AdminReindexItem{{UID: "not-a-sfid"}},
			},
			wantErrMsg: "invalid Salesforce ID",
		},
		{
			name:       "cdc_repair on b2b_org_settings rejected",
			payload:    &membershipservice.AdminReindexPayload{Type: "b2b_org_settings", CdcRepair: true},
			wantErrMsg: "cdc_repair supports only",
		},
		{
			// Regression: reindexItem returns outcomeIssued for a dry-run
			// without publishing, and RunRepair conditionally deletes the
			// marker on outcomeIssued — so dry_run+cdc_repair must be rejected
			// rather than silently deleting real pending markers.
			name:       "cdc_repair + dry_run rejected",
			payload:    &membershipservice.AdminReindexPayload{Type: "b2b_org", CdcRepair: true, DryRun: true},
			wantErrMsg: "cdc_repair does not support dry_run",
		},
		{
			name:       "invalid since rejected",
			payload:    &membershipservice.AdminReindexPayload{Type: "b2b_org", Since: &invalidSince},
			wantErrMsg: "RFC 3339",
		},
		{
			name:       "naive since (no zone) rejected",
			payload:    &membershipservice.AdminReindexPayload{Type: "b2b_org", Since: &naiveSince},
			wantErrMsg: "RFC 3339",
		},
		{
			name:    "since with non-UTC offset accepted (normalised to UTC)",
			payload: &membershipservice.AdminReindexPayload{Type: "b2b_org", Since: &offsetSince},
			// no error
		},
		{
			name: "multiple same-type targeted UIDs accepted",
			payload: &membershipservice.AdminReindexPayload{
				Type: "b2b_org",
				Items: []*membershipservice.AdminReindexItem{
					{UID: "001000000000001AAA"},
					{UID: "001000000000002AAA"},
					{UID: "001000000000003AAA"},
				},
			},
			// no error
		},
	}

	svc := newTestSvc(withBackfillRunner(newTestBackfillRunner(nil)))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.AdminReindex(context.Background(), tt.payload)
			if tt.wantErrMsg != "" {
				require.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), tt.wantErrMsg),
					"expected error containing %q, got: %v", tt.wantErrMsg, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAdminReindex_CdcRepair_SupportedTypesAccepted(t *testing.T) {
	// Each of the three CDC-backed types must be accepted for cdc_repair when
	// the repair store + quota gauge are wired and quota is below threshold;
	// selected_count must reflect the number of pending markers returned.
	for _, tt := range []string{"b2b_org", "project_membership", "key_contact"} {
		t.Run(tt, func(t *testing.T) {
			repairStore := &mock.MockCDCRepairStore{
				Pending: map[string][]port.RepairMarker{
					tt: {{Type: tt, SFID: "001000000000001AAA", Revision: 1}},
				},
			}
			gauge := &mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}
			runner := usecaseSvc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), mock.NewMockKeyContactSObjectReader(), mock.NewMockB2BOrgSettings(), mock.NewMockMemberPublisher(), nil, "",
				nil, usecaseSvc.WithRepairStore(repairStore), usecaseSvc.WithQuotaGauge(gauge))
			svc := newTestSvc(withBackfillRunner(runner))

			result, err := svc.AdminReindex(context.Background(), &membershipservice.AdminReindexPayload{Type: tt, CdcRepair: true})

			require.NoError(t, err)
			require.NotNil(t, result.SelectedCount)
			assert.Equal(t, 1, *result.SelectedCount)
		})
	}
}

// ── Provider configuration ─────────────────────────────────────────────────

func TestGlobalOrgAdminTeamName(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unset is empty", want: ""},
		{name: "stable name is returned", value: "global_org_admin", want: "global_org_admin"},
		{name: "surrounding whitespace is trimmed", value: "  global_org_admin  ", want: "global_org_admin"},
		{name: "whitespace only is empty", value: "   ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GLOBAL_ORG_ADMIN_TEAM_NAME", tt.value)
			assert.Equal(t, tt.want, GlobalOrgAdminTeamName())
		})
	}
}

// Lives here because the Runner behind AdminReindex is wired with
// B2BOrgAuditorTeamNames (WithRunnerB2BOrgAuditorTeams), and the reindex FGA
// emitter is the path that had its guard widened so a blank global-admin UID
// could not silently swallow these grants.
//
// These cases are load-bearing rather than cosmetic. A team reaching this
// function holds auditor on every b2b_org, and fga-sync never deletes a tuple
// whose subject begins with "team:", so the grant cannot be taken back by
// changing config or reverting code. Only a name given explicitly may get
// through: the chart supplies it (values.yaml is the single copy), and an
// absent, blank or whitespace-only variable must grant nothing rather than fall
// back to a hardcoded name that could drift from the chart. The contractor
// variable is deliberately never read (LFXV2-3071); this fails if it returns.
func TestB2BOrgAuditorTeamNames(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "unset grants nothing rather than falling back to a default",
			env:  nil,
			want: []string{},
		},
		{
			name: "an explicitly configured team is granted",
			env:  map[string]string{"LF_STAFF_TEAM_NAME": "staff-team-dev"},
			want: []string{"staff-team-dev"},
		},
		{
			name: "blank grants nothing",
			env:  map[string]string{"LF_STAFF_TEAM_NAME": ""},
			want: []string{},
		},
		{
			// Guards against rendering a "team:#member" subject.
			name: "whitespace-only is dropped rather than rendered",
			env:  map[string]string{"LF_STAFF_TEAM_NAME": "   "},
			want: []string{},
		},
		{
			name: "surrounding whitespace is trimmed",
			env:  map[string]string{"LF_STAFF_TEAM_NAME": "  staff-team  "},
			want: []string{"staff-team"},
		},
		{
			// The contractor variable is no longer read at all, so setting it
			// cannot reintroduce the grant through a stale deployment.
			name: "a stale contractor team name is ignored",
			env: map[string]string{
				"LF_STAFF_TEAM_NAME":      "staff-team",
				"LF_CONTRACTOR_TEAM_NAME": "contractor-team",
			},
			want: []string{"staff-team"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setenv first so the original value is restored on cleanup, then
			// unset, so the "unset" case does not read the developer's shell.
			for _, k := range []string{"LF_STAFF_TEAM_NAME", "LF_CONTRACTOR_TEAM_NAME"} {
				t.Setenv(k, "")
				require.NoError(t, os.Unsetenv(k))
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			assert.Equal(t, tt.want, B2BOrgAuditorTeamNames())
		})
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────

// newTestBackfillRunner returns a Runner with empty mock iterator (no NATS).
func newTestBackfillRunner(iter usecaseSvc.BackfillIterator) *usecaseSvc.Runner {
	if iter == nil {
		iter = &mock.MockBackfillIterator{}
	}
	return usecaseSvc.NewRunner(iter, mock.NewMockB2BOrgReader(), mock.NewMockProjectMembershipReader(), nil, mock.NewMockB2BOrgSettings(), mock.NewMockMemberPublisher(), nil, "", nil)
}
