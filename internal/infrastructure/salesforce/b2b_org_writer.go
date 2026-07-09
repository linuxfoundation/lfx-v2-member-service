// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// B2BOrgWriter implements port.B2BOrgWriter using the Salesforce sObject REST
// API for conditional writes and FetchB2BOrg for post-write re-fetches.
type B2BOrgWriter struct {
	client *SObjectClient
}

// NewB2BOrgWriter creates a B2BOrgWriter backed by the given SObjectClient.
func NewB2BOrgWriter(client *SObjectClient) *B2BOrgWriter {
	return &B2BOrgWriter{client: client}
}

// Ensure B2BOrgWriter satisfies the port at compile time.
var _ port.B2BOrgWriter = (*B2BOrgWriter)(nil)

// CreateB2BOrg fetches the Salesforce Account identified by sfid and returns
// the corresponding B2BOrg. The Account is expected to already exist in
// Salesforce (created by EasyCLA or Enrollment); this method is idempotent —
// a repeated call with the same SFID has no side-effects and returns the same record.
func (w *B2BOrgWriter) CreateB2BOrg(ctx context.Context, sfid string, input model.B2BOrgInput) (*model.B2BOrg, error) {
	uid, err := sfuuid.Normalize18(sfid)
	if err != nil {
		return nil, errs.NewValidation(fmt.Sprintf("invalid Account SFID %q: %v", sfid, err))
	}

	org, _, err := w.client.FetchB2BOrg(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("fetching b2b org for sfid %s: %w", sfid, err)
	}
	if org == nil {
		return nil, errs.NewNotFound("b2b org not found", fmt.Errorf("sfid: %s", sfid))
	}

	slog.DebugContext(ctx, "b2b org created (fetched from Salesforce)", "uid", uid, "sfid", sfid)
	return org, nil
}

// UpdateB2BOrg applies a partial update to the Salesforce Account identified
// by uid. Only non-zero fields in input are sent in the PATCH body.
// If-Unmodified-Since from input is forwarded to Salesforce for server-side
// concurrency protection; Salesforce returns 412 when the record has been
// modified since that timestamp. If-Match (ETag) validation is handled in the
// service layer before this method is called — it is never forwarded to SF.
//
// After a successful update the sObject cache entry is invalidated and the
// record is re-fetched from Salesforce.
func (w *B2BOrgWriter) UpdateB2BOrg(ctx context.Context, uid string, input model.B2BOrgInput) (*model.B2BOrg, error) {
	sfid, err := sfuuid.Normalize18(uid)
	if err != nil {
		return nil, errs.NewValidation(fmt.Sprintf("invalid Account UID %q: %v", uid, err))
	}

	patch, err := buildAccountPatch(input)
	if err != nil {
		return nil, errs.NewValidation(fmt.Sprintf("invalid b2b org input for %s: %v", uid, err))
	}
	if len(patch) == 0 {
		// Nothing to update — return the current record unchanged.
		org, _, fetchErr := w.client.FetchB2BOrg(ctx, uid)
		if fetchErr != nil {
			return nil, fmt.Errorf("fetching b2b org %s: %w", uid, fetchErr)
		}
		return org, nil
	}

	patchBody, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshalling account patch for %s: %w", uid, err)
	}

	uri := fmt.Sprintf("/services/data/%s/sobjects/Account/%s",
		w.client.sf.GetAPIVersion(), sfid)

	var resp *http.Response
	if err := retryOnLock(ctx, writerMaxRetries, writerRetryDelay, func() error {
		var writeErr error
		resp, writeErr = w.client.DoConditionalWrite(ctx, http.MethodPatch, uri, patchBody,
			"", input.IfUnmodifiedSince)
		return writeErr
	}); err != nil {
		return nil, fmt.Errorf("updating b2b org %s in Salesforce: %w", uid, err)
	}

	if resp != nil {
		defer resp.Body.Close() //nolint:errcheck
		switch resp.StatusCode {
		case http.StatusPreconditionFailed:
			return nil, errs.NewPreconditionFailed(
				fmt.Sprintf("b2b org %s has been modified since last read (stale If-Match)", uid))
		case http.StatusOK, http.StatusNoContent:
			// Success — fall through to re-fetch.
		default:
			return nil, fmt.Errorf("updating b2b org %s: unexpected Salesforce status %d", uid, resp.StatusCode)
		}
	}

	slog.DebugContext(ctx, "b2b org updated in Salesforce", "uid", uid, "sfid", sfid)

	// Invalidate the sObject cache so the re-fetch below sees fresh data.
	if err := w.client.InvalidateB2BOrg(ctx, uid); err != nil {
		slog.WarnContext(ctx, "failed to invalidate b2b org cache after update",
			"uid", uid, "error", err)
	}

	org, _, err := w.client.FetchB2BOrg(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("re-fetching b2b org %s after update: %w", uid, err)
	}
	if org == nil {
		return nil, errs.NewNotFound("b2b org not found after update", fmt.Errorf("uid: %s", uid))
	}

	return org, nil
}

// buildAccountPatch constructs the JSON-serialisable PATCH body for a Salesforce
// Account update. Only non-zero fields from input are included; nil pointer fields
// skip unless explicitly set (CrunchBaseURL nil = no-op, "" = explicit null).
func buildAccountPatch(input model.B2BOrgInput) (map[string]any, error) {
	patch := make(map[string]any)
	if input.Name != "" {
		patch["Name"] = input.Name
	}
	if input.Description != "" {
		patch["Description"] = input.Description
	}
	if input.Phone != "" {
		patch["Phone"] = input.Phone
	}
	if input.Website != "" {
		patch["Website"] = input.Website
	}
	if input.PrimaryDomain != "" {
		patch["Account_Domain__c"] = input.PrimaryDomain
	}
	if input.LogoURL != "" {
		patch["Logo_URL__c"] = input.LogoURL
	}
	if input.Industry != "" {
		patch["Industry"] = input.Industry
	}
	if input.Sector != "" {
		patch["Sector__c"] = input.Sector
	}
	if input.CrunchBaseURL != nil {
		if *input.CrunchBaseURL == "" {
			patch["CrunchBase_URL__c"] = nil // explicit JSON null = clear the field
		} else {
			patch["CrunchBase_URL__c"] = *input.CrunchBaseURL
		}
	}
	if input.NumberOfEmployees != nil {
		patch["NumberOfEmployees"] = *input.NumberOfEmployees
	}
	return patch, nil
}
