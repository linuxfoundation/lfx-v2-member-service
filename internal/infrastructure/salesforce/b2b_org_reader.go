// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// B2BOrgReader implements port.B2BOrgReader using the Salesforce sObject REST
// API as the source of truth and NATS KV as a per-record conditional-GET cache.
// accountRepo is used for SOQL-based queries that the sObject client cannot serve.
type B2BOrgReader struct {
	client      *SObjectClient
	accountRepo *AccountRepo
}

// NewB2BOrgReader creates a B2BOrgReader backed by the given SObjectClient and
// an optional Salesforce SOQL client (used for child-list queries). sfClient
// may be nil in tests that only exercise GetB2BOrg.
func NewB2BOrgReader(client *SObjectClient, sfClient *sf.Salesforce) *B2BOrgReader {
	var repo *AccountRepo
	if sfClient != nil {
		repo = NewAccountRepo(sfClient)
	}
	return &B2BOrgReader{client: client, accountRepo: repo}
}

// Ensure B2BOrgReader satisfies the port at compile time.
var _ port.B2BOrgReader = (*B2BOrgReader)(nil)

// GetB2BOrg returns the B2BOrg identified by its v2 UUID. Returns an error
// wrapping ErrNotFound if no record exists.
func (r *B2BOrgReader) GetB2BOrg(ctx context.Context, uid string) (*model.B2BOrg, error) {
	org, _, err := r.client.FetchB2BOrg(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("fetching b2b org %s from Salesforce: %w", uid, err)
	}
	if org == nil {
		return nil, errs.NewNotFound("b2b org not found", fmt.Errorf("uid: %s", uid))
	}
	return org, nil
}

// FetchChildUIDsByParentUID delegates to AccountRepo for the SOQL child-list query.
func (r *B2BOrgReader) FetchChildUIDsByParentUID(ctx context.Context, parentUID string) ([]string, error) {
	if r.accountRepo == nil {
		return nil, fmt.Errorf("accountRepo not initialised")
	}
	return r.accountRepo.FetchChildUIDsByParentUID(ctx, parentUID)
}

// FetchChildUIDsByParentUIDs delegates to AccountRepo for the batched SOQL child-list query.
func (r *B2BOrgReader) FetchChildUIDsByParentUIDs(ctx context.Context, parentUIDs []string) (map[string][]string, error) {
	if r.accountRepo == nil {
		return nil, fmt.Errorf("accountRepo not initialised")
	}
	return r.accountRepo.FetchChildUIDsByParentUIDs(ctx, parentUIDs)
}
