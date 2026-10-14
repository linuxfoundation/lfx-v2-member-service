// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// AccountBatchReader fetches multiple B2BOrg (Account) records in a single
// SOQL query. Used by the CDC consumer to replace per-record sObject fan-out
// with batched IN-clause fetches.
type AccountBatchReader interface {
	// FetchAccountsBySFIDs returns all eligible Account records
	// (model.IsB2BOrgEligible) whose Salesforce Id matches one of the given
	// SFIDs. Records absent from the result set have been soft-deleted or are
	// no longer eligible.
	// The second return value is the subset of requested SFIDs that were
	// present in the SOQL result but could not be converted to domain objects;
	// callers must NOT treat these as absent.
	FetchAccountsBySFIDs(ctx context.Context, sfids []string) ([]*model.B2BOrg, []string, error)
}
