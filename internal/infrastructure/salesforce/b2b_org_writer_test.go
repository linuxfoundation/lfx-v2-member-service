// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// ── writerTestTransport ───────────────────────────────────────────────────────

// writerTestTransport is a minimal http.RoundTripper for writer tests. It
// routes /limits (sf.Init) to a static 200, dispatches PATCH/POST/DELETE
// requests from a sequential patchResponses queue, and serves all GET
// requests from getResponse. Unmatched requests return 200 {}.
type writerTestTransport struct {
	mu             sync.Mutex
	patchResponses []*http.Response
	getResponse    *http.Response
	requests       []*http.Request
}

func (t *writerTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests = append(t.requests, req.Clone(req.Context()))

	if strings.Contains(req.URL.Path, "/limits") {
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}

	if req.Method == http.MethodPatch || req.Method == http.MethodPost || req.Method == http.MethodDelete {
		if len(t.patchResponses) > 0 {
			resp := t.patchResponses[0]
			t.patchResponses = t.patchResponses[1:]
			return cloneResponse(resp), nil
		}
		return fakeResponse(http.StatusNoContent, "", nil), nil
	}

	if t.getResponse != nil {
		return cloneResponse(t.getResponse), nil
	}
	return fakeResponse(http.StatusOK, `{}`, nil), nil
}

// patchRequests returns all PATCH/POST/DELETE requests intercepted by the transport.
func (t *writerTestTransport) patchRequests() []*http.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*http.Request
	for _, r := range t.requests {
		if r.Method == http.MethodPatch || r.Method == http.MethodPost || r.Method == http.MethodDelete {
			out = append(out, r)
		}
	}
	return out
}

func newWriterTransport(patchResp *http.Response, getResp *http.Response) *writerTestTransport {
	return &writerTestTransport{
		patchResponses: []*http.Response{patchResp},
		getResponse:    getResp,
	}
}

// ── TestBuildAccountPatch ─────────────────────────────────────────────────────

func TestBuildAccountPatch(t *testing.T) {
	t.Parallel()

	crunchURL := "https://www.crunchbase.com/organization/acme"
	emptyCrunch := ""
	logoURL := "https://acme.com/logo.png"
	emptyLogo := ""
	var empCount int64 = 500

	tests := []struct {
		name    string
		input   model.B2BOrgInput
		want    map[string]any
		wantErr bool
	}{
		{
			name:  "empty input produces empty patch",
			input: model.B2BOrgInput{},
			want:  map[string]any{},
		},
		{
			name:  "name only",
			input: model.B2BOrgInput{Name: "ACME Corp"},
			want:  map[string]any{"Name": "ACME Corp"},
		},
		{
			name: "all string fields",
			input: model.B2BOrgInput{
				Name:          "ACME Corp",
				Description:   "A great company",
				Phone:         "+1-555-0100",
				Website:       "https://acme.com",
				PrimaryDomain: "acme.com",
				LogoURL:       &logoURL,
				Industry:      "Technology",
				Sector:        "Private",
			},
			want: map[string]any{
				"Name":              "ACME Corp",
				"Description":       "A great company",
				"Phone":             "+1-555-0100",
				"Website":           "https://acme.com",
				"Account_Domain__c": "acme.com",
				"Logo_URL__c":       "https://acme.com/logo.png",
				"Industry":          "Technology",
				"Sector__c":         "Private",
			},
		},
		{
			name:  "LogoURL empty string means explicit null",
			input: model.B2BOrgInput{LogoURL: &emptyLogo},
			want:  map[string]any{"Logo_URL__c": nil},
		},
		{
			name:  "LogoURL nil means no-op",
			input: model.B2BOrgInput{LogoURL: nil},
			want:  map[string]any{},
		},
		{
			name:  "CrunchBaseURL set to value",
			input: model.B2BOrgInput{CrunchBaseURL: &crunchURL},
			want:  map[string]any{"CrunchBase_URL__c": crunchURL},
		},
		{
			name:  "CrunchBaseURL empty string means explicit null",
			input: model.B2BOrgInput{CrunchBaseURL: &emptyCrunch},
			want:  map[string]any{"CrunchBase_URL__c": nil},
		},
		{
			name:  "CrunchBaseURL nil means no-op",
			input: model.B2BOrgInput{CrunchBaseURL: nil},
			want:  map[string]any{},
		},
		{
			name:  "NumberOfEmployees",
			input: model.B2BOrgInput{NumberOfEmployees: &empCount},
			want:  map[string]any{"NumberOfEmployees": empCount},
		},
		{
			name: "precondition header is not included in patch body",
			input: model.B2BOrgInput{
				Name:              "ACME Corp",
				IfUnmodifiedSince: "Mon, 01 Jan 2024 00:00:00 GMT",
			},
			want:    map[string]any{"Name": "ACME Corp"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildAccountPatch(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// ── TestB2BOrgWriter_CreateB2BOrg ─────────────────────────────────────────────

func TestB2BOrgWriter_CreateB2BOrg(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	t.Run("happy path: fetches org by SFID and returns it", func(t *testing.T) {
		t.Parallel()

		transport := &writerTestTransport{
			getResponse: fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		}
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		org, err := writer.CreateB2BOrg(context.Background(), canonicalAccountSFID, model.B2BOrgInput{})

		require.NoError(t, err)
		require.NotNil(t, org)
		assert.Equal(t, uid, org.UID)
		assert.Equal(t, "Linux Foundation", org.Name)
		assert.Equal(t, "linux-foundation", org.Slug)
	})

	t.Run("idempotent: calling twice returns same UID", func(t *testing.T) {
		t.Parallel()

		transport := &writerTestTransport{
			getResponse: fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		}
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		org1, err1 := writer.CreateB2BOrg(context.Background(), canonicalAccountSFID, model.B2BOrgInput{})
		org2, err2 := writer.CreateB2BOrg(context.Background(), canonicalAccountSFID, model.B2BOrgInput{})

		require.NoError(t, err1)
		require.NoError(t, err2)
		assert.Equal(t, org1.UID, org2.UID, "same SFID must produce same UID")
	})

	t.Run("invalid SFID returns validation error", func(t *testing.T) {
		t.Parallel()

		transport := &writerTestTransport{}
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		org, err := writer.CreateB2BOrg(context.Background(), "not-a-valid-sfid", model.B2BOrgInput{})

		require.Error(t, err)
		assert.Nil(t, org)

		var validation errs.Validation
		assert.True(t, errors.As(err, &validation),
			"expected Validation error, got %T: %v", err, err)
	})

	t.Run("salesforce 404 returns NotFound error", func(t *testing.T) {
		t.Parallel()

		transport := &writerTestTransport{
			getResponse: fakeResponse(http.StatusNotFound,
				`[{"errorCode":"NOT_FOUND","message":"Record not found"}]`, nil),
		}
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		org, err := writer.CreateB2BOrg(context.Background(), canonicalAccountSFID, model.B2BOrgInput{})

		require.Error(t, err)
		assert.Nil(t, org)

		var notFound errs.NotFound
		assert.True(t, errors.As(err, &notFound),
			"expected NotFound error, got %T: %v", err, err)
	})
}

// ── TestB2BOrgWriter_UpdateB2BOrg ─────────────────────────────────────────────

// updatedAccountJSON is the re-fetched body after an update (Name changed).
const updatedAccountJSON = `{
	"Id":"001B000000IqhSL",
	"Name":"Linux Foundation (Updated)",
	"Logo_URL__c":"https://linuxfoundation.org/logo.png",
	"Website":"https://linuxfoundation.org",
	"Account_Domain__c":"linuxfoundation.org",
	"Domain_Alias__c":null,
	"Description":"Updated description.",
	"Phone":"+1-415-723-9709",
	"ParentId":null,
	"Industry":"Technology",
	"Sector__c":"Non-Profit",
	"CrunchBase_URL__c":null,
	"NumberOfEmployees":250,
	"LF_Membership_Status__c":"Active",
	"Slug__c":"linux-foundation",
	"CreatedDate":"2020-01-15T10:30:00.000+0000",
	"LastModifiedDate":"2024-07-01T10:00:00.000+0000",
	"SystemModstamp":"2024-07-01T10:00:00.000+0000"
}`

func TestB2BOrgWriter_UpdateB2BOrg(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	t.Run("empty input refetches and returns unchanged org", func(t *testing.T) {
		t.Parallel()

		transport := &writerTestTransport{
			getResponse: fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		}
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		org, err := writer.UpdateB2BOrg(context.Background(), uid, model.B2BOrgInput{})

		require.NoError(t, err)
		require.NotNil(t, org)
		assert.Equal(t, "Linux Foundation", org.Name)
		// No PATCH should have been issued.
		assert.Empty(t, transport.patchRequests(), "no PATCH for empty input")
	})

	t.Run("happy path: patches Salesforce and returns updated org", func(t *testing.T) {
		t.Parallel()

		transport := newWriterTransport(
			fakeResponse(http.StatusNoContent, "", nil),          // PATCH response
			fakeResponse(http.StatusOK, updatedAccountJSON, nil), // re-fetch GET
		)
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		input := model.B2BOrgInput{Name: "Linux Foundation (Updated)"}
		org, err := writer.UpdateB2BOrg(context.Background(), uid, input)

		require.NoError(t, err)
		require.NotNil(t, org)
		assert.Equal(t, "Linux Foundation (Updated)", org.Name)

		patchReqs := transport.patchRequests()
		require.Len(t, patchReqs, 1, "exactly one PATCH must be issued")
		assert.Equal(t, http.MethodPatch, patchReqs[0].Method)
	})

	t.Run("If-Unmodified-Since forwarded in PATCH request", func(t *testing.T) {
		t.Parallel()

		transport := newWriterTransport(
			fakeResponse(http.StatusNoContent, "", nil),
			fakeResponse(http.StatusOK, updatedAccountJSON, nil),
		)
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		input := model.B2BOrgInput{
			Name:              "Linux Foundation (Updated)",
			IfUnmodifiedSince: "Mon, 01 Jan 2024 00:00:00 GMT",
		}
		_, err := writer.UpdateB2BOrg(context.Background(), uid, input)

		require.NoError(t, err)
		patchReqs := transport.patchRequests()
		require.Len(t, patchReqs, 1)
		assert.Equal(t, "Mon, 01 Jan 2024 00:00:00 GMT", patchReqs[0].Header.Get("If-Unmodified-Since"),
			"If-Unmodified-Since must be forwarded to Salesforce")
	})

	t.Run("412 Precondition Failed returns PreconditionFailed error", func(t *testing.T) {
		t.Parallel()

		transport := newWriterTransport(
			fakeResponse(http.StatusPreconditionFailed,
				`[{"errorCode":"PRECONDITION_FAILED","message":"Conflict"}]`, nil),
			nil, // re-fetch should not occur
		)
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		input := model.B2BOrgInput{
			Name:              "Linux Foundation",
			IfUnmodifiedSince: "Mon, 01 Jan 2024 00:00:00 GMT",
		}
		org, err := writer.UpdateB2BOrg(context.Background(), uid, input)

		require.Error(t, err)
		assert.Nil(t, org)

		var precondFailed errs.PreconditionFailed
		assert.True(t, errors.As(err, &precondFailed),
			"expected PreconditionFailed, got %T: %v", err, err)
	})

	t.Run("invalid UID returns validation error", func(t *testing.T) {
		t.Parallel()

		transport := &writerTestTransport{}
		client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
		writer := NewB2BOrgWriter(client)

		org, err := writer.UpdateB2BOrg(context.Background(), "not-a-valid-uid",
			model.B2BOrgInput{Name: "Anything"})

		require.Error(t, err)
		assert.Nil(t, org)

		var validation errs.Validation
		assert.True(t, errors.As(err, &validation),
			"expected Validation error, got %T: %v", err, err)
	})
}

// ── TestRetryOnLock ───────────────────────────────────────────────────────────

// TestRetryOnLock_RetriesOnEntityIsLocked verifies that retryOnLock retries the
// wrapped function up to maxRetries when the error contains ENTITY_IS_LOCKED.
func TestRetryOnLock_RetriesOnEntityIsLocked(t *testing.T) {
	t.Parallel()

	callCount := 0
	err := retryOnLock(context.Background(), 3, time.Millisecond, func() error {
		callCount++
		if callCount < 3 {
			return fmt.Errorf("failed: %s", entityIsLockedCode)
		}
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, 3, callCount, "must retry until success")
}

// TestRetryOnLock_DoesNotRetryOnOtherErrors verifies that non-lock errors are
// returned immediately without retrying.
func TestRetryOnLock_DoesNotRetryOnOtherErrors(t *testing.T) {
	t.Parallel()

	callCount := 0
	err := retryOnLock(context.Background(), 3, time.Millisecond, func() error {
		callCount++
		return fmt.Errorf("some unrelated error")
	})

	assert.Error(t, err)
	assert.Equal(t, 1, callCount, "must not retry non-lock errors")
}

// TestRetryOnLock_ExhaustsRetries verifies that the last lock error is returned
// when all retry attempts are exhausted.
func TestRetryOnLock_ExhaustsRetries(t *testing.T) {
	t.Parallel()

	callCount := 0
	err := retryOnLock(context.Background(), 3, time.Millisecond, func() error {
		callCount++
		return fmt.Errorf("failed: %s", entityIsLockedCode)
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), entityIsLockedCode)
	assert.Equal(t, 3, callCount, "must attempt exactly maxRetries times")
}

// TestRetryOnLock_RespectsContextCancellation verifies that a cancelled context
// stops retry attempts early.
func TestRetryOnLock_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	callCount := 0
	err := retryOnLock(ctx, 5, time.Millisecond, func() error {
		callCount++
		return fmt.Errorf("failed: %s", entityIsLockedCode)
	})

	assert.Error(t, err)
	// Either the first call returns lock error and then select picks ctx.Done(),
	// or context.Canceled is surfaced — either way we must not exhaust all 5.
	assert.LessOrEqual(t, callCount, 2, "cancelled context must stop retries early")
}

// TestDoConditionalWrite_412PassThrough verifies that DoConditionalWrite returns
// the 412 response directly (with nil error) so the caller can inspect the
// status code and return errs.PreconditionFailed.
func TestDoConditionalWrite_412PassThrough(t *testing.T) {
	t.Parallel()

	const uri = "/services/data/v63.0/sobjects/Account/001B000000IqhSL"

	transport := newWriterTransport(
		fakeResponse(http.StatusPreconditionFailed,
			`[{"errorCode":"PRECONDITION_FAILED","message":"Conflict"}]`, nil),
		nil,
	)
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}

	resp, err := client.DoConditionalWrite(
		context.Background(),
		http.MethodPatch,
		uri,
		[]byte(`{"Name":"Test"}`),
		`"stale-etag"`,
		"",
	)

	require.NoError(t, err, "DoConditionalWrite must not error on 412 — the caller interprets the status")
	require.NotNil(t, resp)
	defer resp.Body.Close() //nolint:errcheck
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
}

// TestDoConditionalWrite_EntityIsLockedSurfacedAsError verifies that a 400
// response carrying ENTITY_IS_LOCKED is returned as an error (not a raw
// response), so that retryOnLock can detect and retry it.
func TestDoConditionalWrite_EntityIsLockedSurfacedAsError(t *testing.T) {
	t.Parallel()

	const uri = "/services/data/v63.0/sobjects/Account/001B000000IqhSL"

	transport := newWriterTransport(
		fakeResponse(http.StatusBadRequest,
			`[{"errorCode":"ENTITY_IS_LOCKED","message":"unable to lock row"}]`, nil),
		nil,
	)
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}

	resp, err := client.DoConditionalWrite(
		context.Background(),
		http.MethodPatch,
		uri,
		[]byte(`{"Name":"Test"}`),
		"",
		"",
	)

	require.Error(t, err, "ENTITY_IS_LOCKED must be surfaced as an error for retryOnLock")
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), entityIsLockedCode,
		"error must contain ENTITY_IS_LOCKED for retryOnLock to detect it")
}
