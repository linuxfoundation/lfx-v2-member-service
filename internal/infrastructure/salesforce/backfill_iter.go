// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// salesforceBackfillIterator is the production BackfillIterator backed by
// Salesforce SOQL repos.
type salesforceBackfillIterator struct {
	accounts    *AccountRepo
	memberships *MembershipRepo
	keyContacts *KeyContactRepo
}

// NewBackfillIterator constructs a production Salesforce-backed iterator.
func NewBackfillIterator(
	accounts *AccountRepo,
	memberships *MembershipRepo,
	keyContacts *KeyContactRepo,
) *salesforceBackfillIterator {
	return &salesforceBackfillIterator{
		accounts:    accounts,
		memberships: memberships,
		keyContacts: keyContacts,
	}
}

func (s *salesforceBackfillIterator) IterB2BOrgs(ctx context.Context, since, until *time.Time, fn func([]*model.B2BOrg) error) error {
	return s.accounts.IterB2BOrgs(ctx, since, until, fn)
}

func (s *salesforceBackfillIterator) IterProjectMemberships(ctx context.Context, since, until *time.Time, fn func([]*model.ProjectMembership) error) error {
	return s.memberships.IterProjectMemberships(ctx, since, until, fn)
}

func (s *salesforceBackfillIterator) IterKeyContacts(ctx context.Context, since, until *time.Time, fn func([]*model.KeyContact) error) error {
	return s.keyContacts.IterKeyContacts(ctx, since, until, fn)
}
