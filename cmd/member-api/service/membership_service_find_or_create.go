// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
)

// FindOrCreateB2bOrg finds the Salesforce Account by website domain or creates
// it, then registers it as a B2B organization (lfx-self-serve#4998).
func (s *membershipServicesrvc) FindOrCreateB2bOrg(ctx context.Context, p *membershipservice.FindOrCreateB2bOrgPayload) (*membershipservice.FindOrCreateB2bOrgResult, error) {
	org, action, err := s.b2bOrgWriter.FindOrCreate(ctx, p.Name, p.Website)
	if err != nil {
		return nil, wrapError(ctx, err)
	}

	// the generated 200 (matched) encoder dereferences Etag and LastModified, so neither may stay nil;
	// IsParent is cleared before hashing so the etag matches the shape GetB2bOrg and If-Match use
	orgForEtag := *org
	orgForEtag.IsParent = false
	etagVal, etagErr := etag.LFXEtag(&orgForEtag)
	if etagErr != nil {
		return nil, wrapError(ctx, pkgerrors.NewUnexpected("failed to compute etag for b2b org", etagErr))
	}

	lastMod := org.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	return &membershipservice.FindOrCreateB2bOrgResult{
		B2bOrg:       b2bOrgToResponse(org),
		Action:       string(action),
		Etag:         &etagVal,
		LastModified: &lastMod,
	}, nil
}
