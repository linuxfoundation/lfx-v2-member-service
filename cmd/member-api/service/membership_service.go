// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"

	"goa.design/goa/v3/security"
)

// membershipServicesrvc service implementation
type membershipServicesrvc struct {
	memberReaderOrchestrator usecaseSvc.MemberReader
	storage                  port.MemberReader
	auth                     domain.Authenticator
}

// JWTAuth implements the authorization logic for service "membership-service"
func (s *membershipServicesrvc) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	principal, err := s.auth.ParsePrincipal(ctx, token, slog.Default())
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, constants.PrincipalContextID, principal), nil
}

// ListMembers lists members with pagination, filtering, and search
func (s *membershipServicesrvc) ListMembers(ctx context.Context, p *membershipservice.ListMembersPayload) (res *membershipservice.ListMembersResult, err error) {
	slog.DebugContext(ctx, "membershipService.list-members",
		"page_size", p.PageSize,
		"offset", p.Offset,
		"filter", p.Filter,
		"search", p.Search,
	)

	// Parse filters
	filters := parseFilters(p.Filter)

	var search string
	if p.Search != nil {
		search = *p.Search
	}

	params := model.ListParams{
		PageSize: p.PageSize,
		Offset:   p.Offset,
		Filters:  filters,
		Search:   search,
	}

	members, totalSize, err := s.memberReaderOrchestrator.ListMembers(ctx, params)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	// Convert to response
	memberResponses := make([]*membershipservice.MemberResponse, 0, len(members))
	for _, m := range members {
		memberResponses = append(memberResponses, convertMemberToResponse(m))
	}

	res = &membershipservice.ListMembersResult{
		Members: memberResponses,
		Metadata: &membershipservice.ListMetadata{
			TotalSize: totalSize,
			PageSize:  p.PageSize,
			Offset:    p.Offset,
		},
	}

	return res, nil
}

// GetMemberMembership retrieves a specific membership for a member
func (s *membershipServicesrvc) GetMemberMembership(ctx context.Context, p *membershipservice.GetMemberMembershipPayload) (res *membershipservice.GetMemberMembershipResult, err error) {
	slog.DebugContext(ctx, "membershipService.get-member-membership",
		"member_id", p.MemberID,
		"id", p.ID,
	)

	membership, revision, err := s.memberReaderOrchestrator.GetMembershipForMember(ctx, *p.MemberID, *p.ID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	result := convertMembershipToResponse(membership)
	revisionStr := fmt.Sprintf("%d", revision)

	res = &membershipservice.GetMemberMembershipResult{
		Membership: result,
		Etag:       &revisionStr,
	}

	return res, nil
}

// ListMemberMembershipKeyContacts retrieves key contacts for a membership under a member
func (s *membershipServicesrvc) ListMemberMembershipKeyContacts(ctx context.Context, p *membershipservice.ListMemberMembershipKeyContactsPayload) (res *membershipservice.ListMemberMembershipKeyContactsResult, err error) {
	slog.DebugContext(ctx, "membershipService.list-member-membership-key-contacts",
		"member_id", p.MemberID,
		"id", p.ID,
	)

	contacts, err := s.memberReaderOrchestrator.ListKeyContactsForMembership(ctx, *p.MemberID, *p.ID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	contactResponses := make([]*membershipservice.KeyContactResponse, 0, len(contacts))
	for _, c := range contacts {
		contactResponses = append(contactResponses, convertKeyContactToResponse(c))
	}

	res = &membershipservice.ListMemberMembershipKeyContactsResult{
		Contacts: contactResponses,
	}

	return res, nil
}

// Readyz checks if the service is ready to take inbound requests
func (s *membershipServicesrvc) Readyz(ctx context.Context) (res []byte, err error) {
	if err := s.storage.IsReady(ctx); err != nil {
		slog.ErrorContext(ctx, "service not ready", "error", err)
		return nil, err
	}
	return []byte("OK\n"), nil
}

// Livez checks if the service is alive
func (s *membershipServicesrvc) Livez(ctx context.Context) (res []byte, err error) {
	return []byte("OK\n"), nil
}

// NewMembershipService returns the membership-service service implementation with dependencies
func NewMembershipService(readMemberUseCase usecaseSvc.MemberReader, storage port.MemberReader, authenticator domain.Authenticator) membershipservice.Service {
	return &membershipServicesrvc{
		memberReaderOrchestrator: readMemberUseCase,
		storage:                  storage,
		auth:                     authenticator,
	}
}

// parseFilters parses a filter string into a map
// Format: "key1=value1;key2=value2"
func parseFilters(filter *string) map[string]string {
	filters := make(map[string]string)
	if filter == nil || *filter == "" {
		return filters
	}

	pairs := strings.Split(*filter, ";")
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key != "" && value != "" {
				filters[key] = value
			}
		}
	}

	return filters
}
