// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// MockAccountFindOrCreator is a stub port.AccountFindOrCreator for
// REPOSITORY_SOURCE=mock; it always returns NotImplemented.
type MockAccountFindOrCreator struct{}

// NewMockAccountFindOrCreator creates a new MockAccountFindOrCreator.
func NewMockAccountFindOrCreator() *MockAccountFindOrCreator {
	return &MockAccountFindOrCreator{}
}

// FindOrCreateAccount always returns NotImplemented.
func (m *MockAccountFindOrCreator) FindOrCreateAccount(_ context.Context, _, _ string) (string, model.AccountAction, error) {
	return "", "", errors.NewNotImplemented("find-or-create-b2b-org not implemented in mock")
}
