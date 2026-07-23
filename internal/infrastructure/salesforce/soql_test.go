// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSOQLDateTime(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "utc zero time",
			in:   time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
			want: "2024-06-01T00:00:00Z",
		},
		{
			name: "offset timezone converted to UTC",
			in:   time.Date(2024, 6, 1, 5, 0, 0, 0, time.FixedZone("PDT", -7*3600)),
			want: "2024-06-01T12:00:00Z",
		},
		{
			name: "non-zero seconds preserved",
			in:   time.Date(2026, 1, 15, 13, 45, 30, 0, time.UTC),
			want: "2026-01-15T13:45:30Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := soqlDateTime(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "'", "SOQL dateTime literal must not be single-quoted")
		})
	}
}

func TestLastModifiedWindowClause(t *testing.T) {
	since := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 1, 15, 13, 45, 30, 0, time.UTC)
	cases := []struct {
		name  string
		since *time.Time
		until *time.Time
		want  string
	}{
		{
			name: "both nil is empty",
			want: "",
		},
		{
			name:  "since only emits lower bound",
			since: &since,
			want:  "\n    AND LastModifiedDate >= 2024-06-01T00:00:00Z",
		},
		{
			name:  "until only emits upper bound",
			until: &until,
			want:  "\n    AND LastModifiedDate <= 2026-01-15T13:45:30Z",
		},
		{
			name:  "bounded window emits both bounds in order",
			since: &since,
			until: &until,
			want:  "\n    AND LastModifiedDate >= 2024-06-01T00:00:00Z\n    AND LastModifiedDate <= 2026-01-15T13:45:30Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lastModifiedWindowClause(tc.since, tc.until)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "'", "SOQL dateTime literal must not be single-quoted")
		})
	}
}
