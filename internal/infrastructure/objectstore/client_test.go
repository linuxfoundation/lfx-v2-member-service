// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package objectstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeS3Server serves just enough of the S3 HTTP API (HeadBucket/PutObject)
// for Client's paths, keyed on the HTTP method + path so tests stay small.
func fakeS3Server(t *testing.T, headStatus int, putStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(headStatus)
		case http.MethodPut:
			w.WriteHeader(putStatus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	c, err := NewClient(context.Background(), Config{
		Bucket:       "test-bucket",
		Region:       "us-west-2",
		CDNURLPrefix: "https://cdn.example.com",
		EndpointURL:  endpoint,
	})
	require.NoError(t, err)
	return c
}

func TestClient_Readyz_Healthy(t *testing.T) {
	server := fakeS3Server(t, http.StatusOK, http.StatusOK)
	defer server.Close()
	client := newTestClient(t, server.URL)

	assert.NoError(t, client.Readyz(context.Background()))
}

func TestClient_Readyz_Unreachable(t *testing.T) {
	server := fakeS3Server(t, http.StatusForbidden, http.StatusOK)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.Readyz(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-bucket")
}

func TestClient_Put_ReturnsVersionedCDNURL(t *testing.T) {
	server := fakeS3Server(t, http.StatusOK, http.StatusOK)
	defer server.Close()
	client := newTestClient(t, server.URL)

	origNowUnix := nowUnix
	nowUnix = func() int64 { return 1700000000 }
	defer func() { nowUnix = origNowUnix }()

	url, err := client.Put(context.Background(), "b2b_org_logos/uid-1.png", "image/png", []byte("fake-png-bytes"))

	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1700000000", url)
}

func TestClient_Put_UploadError(t *testing.T) {
	server := fakeS3Server(t, http.StatusOK, http.StatusInternalServerError)
	defer server.Close()
	client := newTestClient(t, server.URL)

	_, err := client.Put(context.Background(), "b2b_org_logos/uid-1.png", "image/png", []byte("fake-png-bytes"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-bucket")
}
