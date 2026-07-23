// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// MockBackfillIterator is a test BackfillIterator with pre-loaded pages.
type MockBackfillIterator struct {
	B2BOrgs     [][]*model.B2BOrg
	Memberships [][]*model.ProjectMembership
	KeyContacts [][]*model.KeyContact
	B2BErr      error
	PmErr       error
	KcErr       error
}

func (m *MockBackfillIterator) IterB2BOrgs(_ context.Context, _, _ *time.Time, fn func([]*model.B2BOrg) error) error {
	for _, page := range m.B2BOrgs {
		if err := fn(page); err != nil {
			return err
		}
	}
	return m.B2BErr
}

func (m *MockBackfillIterator) IterProjectMemberships(_ context.Context, _, _ *time.Time, fn func([]*model.ProjectMembership) error) error {
	for _, page := range m.Memberships {
		if err := fn(page); err != nil {
			return err
		}
	}
	return m.PmErr
}

func (m *MockBackfillIterator) IterKeyContacts(_ context.Context, _, _ *time.Time, fn func([]*model.KeyContact) error) error {
	for _, page := range m.KeyContacts {
		if err := fn(page); err != nil {
			return err
		}
	}
	return m.KcErr
}
