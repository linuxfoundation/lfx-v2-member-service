// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
)

func TestParseIncludeCompanyAccounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: "", want: false},
		{raw: "  ", want: false},
		{raw: "true", want: true},
		{raw: " TRUE ", want: true},
		{raw: "1", want: true},
		{raw: "false", want: false},
		{raw: "0", want: false},
		{raw: "yes", wantErr: true},
		{raw: "ture", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseIncludeCompanyAccounts(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseIncludeCompanyAccounts(%q): expected an error", tt.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseIncludeCompanyAccounts(%q): unexpected error %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseIncludeCompanyAccounts(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestWriteEligibilityReader(t *testing.T) {
	t.Parallel()

	built := 0
	newReader := func() port.B2BOrgEligibilityReader {
		built++
		return mock.MockB2BOrgEligibilityReader{}
	}

	if got := writeEligibilityReader(false, newReader); got != nil {
		t.Errorf("setting off: got %T, want nil so staging and prod refuse nothing", got)
	}
	if built != 0 {
		t.Errorf("setting off: reader built %d times, want 0", built)
	}
	if got := writeEligibilityReader(true, newReader); got == nil {
		t.Error("setting on: got nil, want the reader")
	}
	if built != 1 {
		t.Errorf("setting on: reader built %d times, want 1", built)
	}
}
