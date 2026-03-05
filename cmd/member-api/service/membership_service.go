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
	membershipReaderOrchestrator usecaseSvc.MembershipReader
	storage                      port.MembershipReader
	auth                         domain.Authenticator
}

// JWTAuth implements the authorization logic for service "membership-service"
func (s *membershipServicesrvc) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	principal, err := s.auth.ParsePrincipal(ctx, token, slog.Default())
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, constants.PrincipalContextID, principal), nil
}

// ListMemberships lists memberships with pagination and filtering
func (s *membershipServicesrvc) ListMemberships(ctx context.Context, p *membershipservice.ListMembershipsPayload) (res *membershipservice.ListMembershipsResult, err error) {
	slog.DebugContext(ctx, "membershipService.list-memberships",
		"page_size", p.PageSize,
		"offset", p.Offset,
		"filter", p.Filter,
	)

	// Parse filters
	filters := parseFilters(p.Filter)

	params := model.ListParams{
		PageSize: p.PageSize,
		Offset:   p.Offset,
		Filters:  filters,
	}

	memberships, totalSize, err := s.membershipReaderOrchestrator.ListMemberships(ctx, params)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	// Convert to response
	membershipResponses := make([]*membershipservice.MembershipResponse, 0, len(memberships))
	for _, m := range memberships {
		membershipResponses = append(membershipResponses, convertMembershipToResponse(m))
	}

	res = &membershipservice.ListMembershipsResult{
		Memberships: membershipResponses,
		Metadata: &membershipservice.ListMetadata{
			TotalSize: totalSize,
			PageSize:  p.PageSize,
			Offset:    p.Offset,
		},
	}

	return res, nil
}

// GetMembership retrieves a specific membership by UID
func (s *membershipServicesrvc) GetMembership(ctx context.Context, p *membershipservice.GetMembershipPayload) (res *membershipservice.GetMembershipResult, err error) {
	slog.DebugContext(ctx, "membershipService.get-membership",
		"uid", p.UID,
	)

	membership, revision, err := s.membershipReaderOrchestrator.GetMembership(ctx, *p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	result := convertMembershipToResponse(membership)
	revisionStr := fmt.Sprintf("%d", revision)

	res = &membershipservice.GetMembershipResult{
		Membership: result,
		Etag:       &revisionStr,
	}

	return res, nil
}

// ListMembershipContacts retrieves key contacts for a specific membership
func (s *membershipServicesrvc) ListMembershipContacts(ctx context.Context, p *membershipservice.ListMembershipContactsPayload) (res *membershipservice.ListMembershipContactsResult, err error) {
	slog.DebugContext(ctx, "membershipService.list-membership-contacts",
		"uid", p.UID,
	)

	contacts, err := s.membershipReaderOrchestrator.ListKeyContacts(ctx, *p.UID)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	contactResponses := make([]*membershipservice.KeyContactResponse, 0, len(contacts))
	for _, c := range contacts {
		contactResponses = append(contactResponses, convertKeyContactToResponse(c))
	}

	res = &membershipservice.ListMembershipContactsResult{
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
func NewMembershipService(readMembershipUseCase usecaseSvc.MembershipReader, storage port.MembershipReader, authenticator domain.Authenticator) membershipservice.Service {
	return &membershipServicesrvc{
		membershipReaderOrchestrator: readMembershipUseCase,
		storage:                      storage,
		auth:                         authenticator,
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
