// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	sf "github.com/k-capehart/go-salesforce/v3"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"

	membershipservicec "github.com/linuxfoundation/lfx-v2-member-service/gen/http/membership_service/client"
	membershipservicesvr "github.com/linuxfoundation/lfx-v2-member-service/gen/http/membership_service/server"
	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
)

const findOrCreateToken = "round-trip-token"

func withAuth(a *auth.MockJWTAuth) svcOpt {
	return func(b *svcBuilder) { b.auth = a }
}

// newFindOrCreateHTTP mounts the generated HTTP server over the service and returns its base URL
func newFindOrCreateHTTP(t *testing.T, uc *stubFindOrCreateWriterUC) string {
	t.Helper()
	return newFindOrCreateHTTPWith(t, uc)
}

func newFindOrCreateHTTPWith(t *testing.T, uc usecaseSvc.B2BOrgWriter) string {
	t.Helper()
	authMock := &auth.MockJWTAuth{}
	authMock.On("ParsePrincipal", tmock.Anything, findOrCreateToken, tmock.Anything).Return("round-trip-principal", nil)
	endpoints := membershipservice.NewEndpoints(newTestSvc(withB2BOrgWriterUC(uc), withAuth(authMock)))
	mux := goahttp.NewMuxer()
	dir := http.Dir(t.TempDir())
	server := membershipservicesvr.New(endpoints, mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, func(context.Context, http.ResponseWriter, error) {}, nil, dir, dir, dir, dir)
	membershipservicesvr.Mount(mux, server)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func findOrCreateClient(t *testing.T, base string) goa.Endpoint {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	return membershipservicec.NewClient(u.Scheme, u.Host, http.DefaultClient, goahttp.RequestEncoder, goahttp.ResponseDecoder, false).FindOrCreateB2bOrg()
}

func TestFindOrCreateB2bOrgHTTPRoundTrip(t *testing.T) {
	token, version := findOrCreateToken, "1"
	payload := &membershipservice.FindOrCreateB2bOrgPayload{BearerToken: &token, Version: &version, Name: "Linux Foundation", Website: "https://linuxfoundation.org"}

	for _, tc := range []struct {
		action model.AccountAction
		status int
	}{{model.AccountActionCreated, http.StatusCreated}, {model.AccountActionMatched, http.StatusOK}} {
		t.Run(string(tc.action), func(t *testing.T) {
			uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{org: sampleB2BOrg}, action: tc.action}
			base := newFindOrCreateHTTP(t, uc)

			// raw HTTP: status, body shape (the b2b_org, as POST /b2b_orgs) and headers
			req, err := http.NewRequest(http.MethodPost, base+"/b2b_orgs/find-or-create?v=1", strings.NewReader(`{"name":"Linux Foundation","website":"https://linuxfoundation.org"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+findOrCreateToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tc.status, resp.StatusCode, string(raw))
			assert.Equal(t, string(tc.action), resp.Header.Get("X-Account-Action"))
			assert.NotEmpty(t, resp.Header.Get("ETag"))
			assert.NotEmpty(t, resp.Header.Get("Last-Modified"))
			var body map[string]any
			require.NoError(t, json.Unmarshal(raw, &body))
			assert.Equal(t, "lf-uid-001", body["uid"])
			assert.Equal(t, "Linux Foundation", body["name"])
			_, hasAction := body["action"]
			assert.False(t, hasAction, "the body stays the plain b2b_org")

			// generated client: both statuses decode the action
			res, err := findOrCreateClient(t, base)(context.Background(), payload)
			require.NoError(t, err)
			result, ok := res.(*membershipservice.FindOrCreateB2bOrgResult)
			require.True(t, ok, "%T", res)
			assert.Equal(t, string(tc.action), result.Action)
			require.NotNil(t, result.B2bOrg)
			assert.Equal(t, "lf-uid-001", *result.B2bOrg.UID)
			assert.NotNil(t, result.Etag)
			assert.NotNil(t, result.LastModified)
			assert.Equal(t, []string{"Linux Foundation|https://linuxfoundation.org", "Linux Foundation|https://linuxfoundation.org"}, uc.calls)
		})
	}

	for name, tc := range map[string]struct {
		err    error
		status int
		goaErr string
	}{
		"ambiguous": {pkgerrors.NewConflict("several accounts match the website domain"), http.StatusConflict, "Conflict"},
		"disabled":  {pkgerrors.NewServiceUnavailable("b2b org find-or-create is not enabled"), http.StatusServiceUnavailable, "ServiceUnavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			base := newFindOrCreateHTTP(t, &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{err: tc.err}})

			req, err := http.NewRequest(http.MethodPost, base+"/b2b_orgs/find-or-create?v=1", strings.NewReader(`{"name":"Linux Foundation","website":"https://linuxfoundation.org"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+findOrCreateToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tc.status, resp.StatusCode)

			_, err = findOrCreateClient(t, base)(context.Background(), payload)
			var serviceErr *goa.ServiceError
			require.True(t, errors.As(err, &serviceErr), "%T: %v", err, err)
			assert.Equal(t, tc.goaErr, serviceErr.Name)
		})
	}

	t.Run("missing name is rejected before the service", func(t *testing.T) {
		uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{org: sampleB2BOrg}, action: model.AccountActionCreated}
		base := newFindOrCreateHTTP(t, uc)
		req, err := http.NewRequest(http.MethodPost, base+"/b2b_orgs/find-or-create?v=1", strings.NewReader(`{"website":"https://linuxfoundation.org"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+findOrCreateToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, uc.calls)
	})
}

func TestFindOrCreateB2bOrgHTTPParentEtagMatchesGetShape(t *testing.T) {
	token, version := findOrCreateToken, "1"
	payload := &membershipservice.FindOrCreateB2bOrgPayload{BearerToken: &token, Version: &version, Name: "Linux Foundation", Website: "https://linuxfoundation.org"}
	parent := &model.B2BOrg{UID: "001410000FindOrAQI", Name: "Linux Foundation", IsParent: true}
	cleared := *parent
	cleared.IsParent = false
	want, err := etag.LFXEtag(&cleared)
	require.NoError(t, err)
	for _, action := range []model.AccountAction{model.AccountActionMatched, model.AccountActionCreated} {
		t.Run(string(action), func(t *testing.T) {
			uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{org: parent}, action: action}
			base := newFindOrCreateHTTP(t, uc)
			req, err := http.NewRequest(http.MethodPost, base+"/b2b_orgs/find-or-create?v=1", strings.NewReader(`{"name":"Linux Foundation","website":"https://linuxfoundation.org"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+findOrCreateToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, want, resp.Header.Get("Etag"), "the ETag a parent org's own If-Match can present")
			assert.True(t, parent.IsParent, "the source model keeps its parent flag")

			result, err := findOrCreateClient(t, base)(context.Background(), payload)
			require.NoError(t, err)
			require.NotNil(t, result.(*membershipservice.FindOrCreateB2bOrgResult).Etag)
			assert.Equal(t, want, *result.(*membershipservice.FindOrCreateB2bOrgResult).Etag)
		})
	}
}

func TestFindOrCreateB2bOrgHTTPEtagFailureIs500(t *testing.T) {
	token, version := findOrCreateToken, "1"
	payload := &membershipservice.FindOrCreateB2bOrgPayload{BearerToken: &token, Version: &version, Name: "Linux Foundation", Website: "https://linuxfoundation.org"}
	for _, action := range []model.AccountAction{model.AccountActionMatched, model.AccountActionCreated} {
		t.Run(string(action), func(t *testing.T) {
			uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{org: &model.B2BOrg{UID: "001410000FindOrAQI", Name: "Linux Foundation", UpdatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}, action: action}
			base := newFindOrCreateHTTP(t, uc)

			req, err := http.NewRequest(http.MethodPost, base+"/b2b_orgs/find-or-create?v=1", strings.NewReader(`{"name":"Linux Foundation","website":"https://linuxfoundation.org"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+findOrCreateToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "a real response, not an aborted connection")
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(raw))
			assert.Empty(t, resp.Header.Get("Etag"))

			_, err = findOrCreateClient(t, base)(context.Background(), payload)
			var serviceErr *goa.ServiceError
			require.True(t, errors.As(err, &serviceErr), "%T: %v", err, err)
			assert.Equal(t, "InternalServerError", serviceErr.Name)
		})
	}
}

// apexFakeTransport answers the Apex find-or-create POST with one canned response and everything else with 200 {}
type apexFakeTransport struct {
	status int
	body   string
}

func (a apexFakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/services/apexrest/lfx/account") {
		resp.StatusCode, resp.Body = a.status, io.NopCloser(strings.NewReader(a.body))
	}
	return resp, nil
}

func TestFindOrCreateB2bOrgHTTPKeepsUpstreamTextOut(t *testing.T) {
	token, version := findOrCreateToken, "1"
	payload := &membershipservice.FindOrCreateB2bOrgPayload{BearerToken: &token, Version: &version, Name: "Linux Foundation", Website: "https://linuxfoundation.org"}
	for name, tc := range map[string]struct {
		status int
		body   string
		leaked string
		want   string
		goaErr string
	}{
		"400 non-json": {http.StatusBadRequest, "<html><body>Bad Request from the proxy</body></html>", "proxy", "rejected the account name or website", "BadRequest"},
		"422 dml":      {http.StatusUnprocessableEntity, `{"message":"FIELD_CUSTOM_VALIDATION_EXCEPTION: Please enter valid domain address."}`, "FIELD_CUSTOM_VALIDATION_EXCEPTION", "cannot be created in salesforce", "UnprocessableEntity"},
		"409 text":     {http.StatusConflict, "the domain acme.example does not identify exactly one account", "acme.example", "several accounts match the website domain", "Conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			sfClient, err := sf.Init(sf.Creds{Domain: "https://test.salesforce.com", AccessToken: "fake-token-for-tests"}, sf.WithRoundTripper(apexFakeTransport{status: tc.status, body: tc.body}))
			require.NoError(t, err)
			finder := salesforce.NewApexAccountClient(salesforce.NewSObjectClient(sfClient, nil), "/services/apexrest/lfx/account")
			base := newFindOrCreateHTTPWith(t, usecaseSvc.NewB2BOrgWriter(usecaseSvc.WithAccountFindOrCreator(finder)))

			req, err := http.NewRequest(http.MethodPost, base+"/b2b_orgs/find-or-create?v=1", strings.NewReader(`{"name":"Linux Foundation","website":"https://linuxfoundation.org"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+findOrCreateToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tc.status, resp.StatusCode, string(raw))
			assert.NotContains(t, string(raw), tc.leaked, "upstream text never reaches the client")
			assert.Contains(t, string(raw), tc.want)

			_, err = findOrCreateClient(t, base)(context.Background(), payload)
			var serviceErr *goa.ServiceError
			require.True(t, errors.As(err, &serviceErr), "%T: %v", err, err)
			assert.Equal(t, tc.goaErr, serviceErr.Name)
			assert.NotContains(t, serviceErr.Message, tc.leaked)
		})
	}
}
