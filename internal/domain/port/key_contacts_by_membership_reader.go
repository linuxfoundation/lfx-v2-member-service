// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// KeyContactsByMembershipReader returns the current Salesforce key contacts
// attached to a project membership.
type KeyContactsByMembershipReader interface {
	FetchKeyContactsByAssetSFID(ctx context.Context, assetSFID string) ([]*model.KeyContact, error)
}
