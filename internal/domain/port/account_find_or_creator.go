// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// AccountFindOrCreator finds a Salesforce Account by website domain or creates
// it as a Company Account (lfx-self-serve#4997).
type AccountFindOrCreator interface {
	// FindOrCreateAccount returns the 18-character Account SFID and whether it
	// was matched or created. An ambiguous domain (several Accounts) is a
	// Conflict error and writes nothing.
	FindOrCreateAccount(ctx context.Context, name, website string) (string, model.AccountAction, error)
}
