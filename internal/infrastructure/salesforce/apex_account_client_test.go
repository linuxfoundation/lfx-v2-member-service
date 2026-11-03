// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

const apexAccountPath = "/services/apexrest/lfx/account"

func newApexTestClient(t *testing.T, rt http.RoundTripper) *ApexAccountClient {
	t.Helper()
	return NewApexAccountClient(&SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}, apexAccountPath)
}

func TestApexAccountClient_FindOrCreateAccount(t *testing.T) {
	t.Parallel()
	const sfid = "001000000000001AAA"

	t.Run("created", func(t *testing.T) {
		t.Parallel()
		transport := newWriterTransport(fakeResponse(http.StatusOK, `{"id":"`+sfid+`","action":"created"}`, nil), nil)

		got, action, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), " Acme ", " https://acme.example ")

		require.NoError(t, err)
		assert.Equal(t, sfid, got)
		assert.Equal(t, model.AccountActionCreated, action)
		reqs := transport.patchRequests()
		require.Len(t, reqs, 1)
		assert.Equal(t, http.MethodPost, reqs[0].Method)
		assert.Equal(t, apexAccountPath, reqs[0].URL.Path)
		assert.Equal(t, "Bearer fake-token-for-tests", reqs[0].Header.Get("Authorization"))
		assert.Equal(t, "application/json", reqs[0].Header.Get("Content-Type"))
		raw, err := io.ReadAll(reqs[0].Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		assert.Equal(t, map[string]any{"name": "Acme", "website": "https://acme.example", "source": AccountFindOrCreateSource, "dryRun": false}, body)
	})

	t.Run("matched with a 15-character id is canonicalised", func(t *testing.T) {
		t.Parallel()
		transport := newWriterTransport(fakeResponse(http.StatusOK, `{"id":"001000000000001","action":"matched"}`, nil), nil)

		got, action, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")

		require.NoError(t, err)
		assert.Equal(t, sfid, got)
		assert.Equal(t, model.AccountActionMatched, action)
	})

	t.Run("ambiguous is a conflict", func(t *testing.T) {
		t.Parallel()
		transport := newWriterTransport(fakeResponse(http.StatusOK, `{"action":"ambiguous","message":"more than one account has the domain acme.example"}`, nil), nil)

		got, action, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")

		var conflict errs.Conflict
		require.True(t, errors.As(err, &conflict), "%v", err)
		assert.Empty(t, got)
		assert.Equal(t, model.AccountActionAmbiguous, action)
	})

	t.Run("unexpected action or invalid id fails", func(t *testing.T) {
		t.Parallel()
		for _, body := range []string{`{"id":"` + sfid + `","action":"weird"}`, `{"id":"not-an-id","action":"created"}`, `not json`} {
			transport := newWriterTransport(fakeResponse(http.StatusOK, body, nil), nil)
			got, _, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")
			require.Error(t, err, body)
			assert.Empty(t, got)
		}
	})

	t.Run("apex error statuses keep their typed meaning", func(t *testing.T) {
		t.Parallel()
		transport := newWriterTransport(fakeResponse(http.StatusUnprocessableEntity, `{"message":"FIELD_CUSTOM_VALIDATION_EXCEPTION: Please enter valid domain address."}`, nil), nil)
		got, _, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")
		var unprocessable errs.UnprocessableEntity
		require.True(t, errors.As(err, &unprocessable), "%v", err)
		assert.Contains(t, err.Error(), "FIELD_CUSTOM_VALIDATION_EXCEPTION")
		assert.Empty(t, got)

		transport = newWriterTransport(fakeResponse(http.StatusBadRequest, `{"message":"name and website are required"}`, nil), nil)
		_, _, err = newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")
		var validation errs.Validation
		require.True(t, errors.As(err, &validation), "%v", err)

		transport = newWriterTransport(fakeResponse(http.StatusConflict, `{"message":"more than one account has the domain acme.example"}`, nil), nil)
		_, action, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")
		var conflict errs.Conflict
		require.True(t, errors.As(err, &conflict), "%v", err)
		assert.Equal(t, model.AccountActionAmbiguous, action)

		transport = newWriterTransport(fakeResponse(http.StatusBadGateway, `<html>bad gateway</html>`, nil), nil)
		_, _, err = newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "502")
		assert.False(t, errors.As(err, &conflict) || errors.As(err, &validation) || errors.As(err, &unprocessable))
	})

	t.Run("401 refreshes the session and retries once", func(t *testing.T) {
		t.Parallel()
		calls := 0
		transport := &countingTransport{
			callCount:  &calls,
			firstResp:  fakeResponse(http.StatusUnauthorized, `[{"errorCode":"INVALID_SESSION_ID"}]`, nil),
			retryResp:  fakeResponse(http.StatusOK, `{"id":"`+sfid+`","action":"created"}`, nil),
			limitsResp: fakeResponse(http.StatusOK, `{}`, nil),
		}

		got, action, err := newApexTestClient(t, transport).FindOrCreateAccount(context.Background(), "Acme", "https://acme.example")

		require.NoError(t, err)
		assert.Equal(t, sfid, got)
		assert.Equal(t, model.AccountActionCreated, action)
		assert.Equal(t, 2, calls)
	})

	t.Run("blank input is refused before any call", func(t *testing.T) {
		t.Parallel()
		transport := newWriterTransport(fakeResponse(http.StatusOK, `{}`, nil), nil)
		client := newApexTestClient(t, transport)
		for _, in := range [][2]string{{"", "https://acme.example"}, {"Acme", ""}, {" ", " "}} {
			_, _, err := client.FindOrCreateAccount(context.Background(), in[0], in[1])
			var validation errs.Validation
			require.True(t, errors.As(err, &validation), "%q: %v", in, err)
		}
		assert.Empty(t, transport.patchRequests())
	})
}
