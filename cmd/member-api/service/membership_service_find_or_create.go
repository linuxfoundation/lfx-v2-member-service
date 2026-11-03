// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
)

// FindOrCreateB2bOrg finds the Salesforce Account by website domain or creates
// it, then registers it as a B2B organization (lfx-self-serve#4998).
func (s *membershipServicesrvc) FindOrCreateB2bOrg(ctx context.Context, p *membershipservice.FindOrCreateB2bOrgPayload) (*membershipservice.FindOrCreateB2bOrgResult, error) {
	org, action, err := s.b2bOrgWriter.FindOrCreate(ctx, p.Name, p.Website)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	etagVal, etagErr := etag.LFXEtag(org)
	if etagErr != nil {
		slog.WarnContext(ctx, "failed to compute etag for b2b org", "uid", org.UID, "error", etagErr)
	}

	lastMod := org.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	result := &membershipservice.FindOrCreateB2bOrgResult{
		B2bOrg:       b2bOrgToResponse(org),
		Action:       string(action),
		LastModified: &lastMod,
	}
	if etagVal != "" {
		result.Etag = &etagVal
	}
	return result, nil
}
