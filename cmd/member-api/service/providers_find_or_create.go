// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce"
)

// AccountFindOrCreatorImpl returns the port.AccountFindOrCreator behind
// POST /b2b_orgs/find-or-create when B2B_ORG_FIND_OR_CREATE_ENABLED is "true",
// SF_APEX_ACCOUNT_PATH is set and Company Accounts are b2b_org-eligible
// (B2B_ORG_INCLUDE_COMPANY_ALL_ACCOUNTS); otherwise nil, and the route answers 503.
func AccountFindOrCreatorImpl(ctx context.Context) port.AccountFindOrCreator {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("B2B_ORG_FIND_OR_CREATE_ENABLED")), "true") {
		slog.InfoContext(ctx, "b2b org find-or-create disabled")
		return nil
	}
	path := strings.TrimSpace(os.Getenv("SF_APEX_ACCOUNT_PATH"))
	if path == "" {
		slog.WarnContext(ctx, "b2b org find-or-create enabled but SF_APEX_ACCOUNT_PATH is empty; the route answers 503")
		return nil
	}
	if !B2BOrgIncludeCompanyAccounts(ctx) {
		slog.WarnContext(ctx, "b2b org find-or-create enabled without B2B_ORG_INCLUDE_COMPANY_ALL_ACCOUNTS; a created Account could not be registered, so the route answers 503")
		return nil
	}

	repoSource := os.Getenv("REPOSITORY_SOURCE")
	if repoSource == "" {
		repoSource = "salesforce"
	}

	switch repoSource {
	case "mock":
		slog.InfoContext(ctx, "initialising mock account find-or-create client")
		return mock.NewMockAccountFindOrCreator()

	case "salesforce":
		slog.InfoContext(ctx, "initialising Salesforce Apex account find-or-create client", "path", path)
		sObjectClientInit(ctx)
		return salesforce.NewApexAccountClient(sObjectClient, path)

	default:
		log.Fatalf("unsupported REPOSITORY_SOURCE value: %q", repoSource)
		return nil
	}
}
