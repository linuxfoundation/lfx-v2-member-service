// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
)

// resetIncludeCompanyAccounts lets each case read B2B_ORG_INCLUDE_COMPANY_ALL_ACCOUNTS afresh.
func resetIncludeCompanyAccounts(t *testing.T) {
	t.Helper()
	includeCompanyAccountsOnce = sync.Once{}
	includeCompanyAccounts = false
	t.Cleanup(func() {
		includeCompanyAccountsOnce = sync.Once{}
		includeCompanyAccounts = false
	})
}

func TestAccountFindOrCreatorImpl(t *testing.T) {
	cases := []struct {
		name    string
		enabled string
		path    string
		include string
		want    bool
	}{
		{name: "disabled", enabled: "false", path: "/services/apexrest/lfx/account", include: "true"},
		{name: "unset flag", enabled: "", path: "/services/apexrest/lfx/account", include: "true"},
		{name: "missing path", enabled: "true", path: " ", include: "true"},
		{name: "company accounts not eligible", enabled: "true", path: "/services/apexrest/lfx/account", include: "false"},
		{name: "enabled", enabled: "TRUE", path: "/services/apexrest/lfx/account", include: "true", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetIncludeCompanyAccounts(t)
			t.Setenv("REPOSITORY_SOURCE", "mock")
			t.Setenv("B2B_ORG_FIND_OR_CREATE_ENABLED", tc.enabled)
			t.Setenv("SF_APEX_ACCOUNT_PATH", tc.path)
			t.Setenv("B2B_ORG_INCLUDE_COMPANY_ALL_ACCOUNTS", tc.include)

			finder := AccountFindOrCreatorImpl(context.Background())

			if !tc.want {
				assert.Nil(t, finder)
				return
			}
			require.NotNil(t, finder)
			assert.IsType(t, &mock.MockAccountFindOrCreator{}, finder)
		})
	}
}
