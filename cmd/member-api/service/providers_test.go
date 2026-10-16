// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import "testing"

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
