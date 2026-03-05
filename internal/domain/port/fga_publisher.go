// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// FGAPublisher publishes access control messages to the fga-sync service.
type FGAPublisher interface {
	// UpdateAccess publishes an update_access message for the given membership UID,
	// granting the specified team's members auditor access.
	UpdateMemberAccess(ctx context.Context, membershipUID string, auditorTeamID string) error
}
