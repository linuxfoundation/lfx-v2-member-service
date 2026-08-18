// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// KeyContactsByMembershipReader returns current Salesforce key contacts grouped
// by project membership.
type KeyContactsByMembershipReader interface {
	FetchKeyContactsByAssetSFIDs(
		ctx context.Context,
		assetSFIDs []string,
	) (map[string][]*model.KeyContact, error)
}
