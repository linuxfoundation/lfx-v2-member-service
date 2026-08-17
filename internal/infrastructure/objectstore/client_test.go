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
	return fakeS3ServerWithDelete(t, headStatus, putStatus, http.StatusOK)
}

// fakeS3ServerWithDelete additionally stubs DeleteObject, for tests that
// exercise Client.Delete.
func fakeS3ServerWithDelete(t *testing.T, headStatus, putStatus, deleteStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(headStatus)
		case http.MethodPut:
			w.WriteHeader(putStatus)
		case http.MethodDelete:
			w.WriteHeader(deleteStatus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// fakeS3ServerWithCopy additionally distinguishes CopyObject from a plain
// PutObject — both are HTTP PUTs, but CopyObject carries an
// X-Amz-Copy-Source header and, on success, must return a well-formed
// CopyObjectResult XML body or the SDK fails to unmarshal the response.
func fakeS3ServerWithCopy(t *testing.T, headStatus, putStatus, copyStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(headStatus)
		case http.MethodPut:
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				w.WriteHeader(copyStatus)
				if copyStatus == http.StatusOK {
					_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"fake-etag"</ETag></CopyObjectResult>`))
				}
				return
			}
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

func TestClient_VersionedURL_NoUpload(t *testing.T) {
	server := fakeS3Server(t, http.StatusOK, http.StatusOK)
	defer server.Close()
	client := newTestClient(t, server.URL)

	origNowUnix := nowUnix
	nowUnix = func() int64 { return 1700000000 }
	defer func() { nowUnix = origNowUnix }()

	url := client.VersionedURL("b2b_org_logos/uid-1.png")

	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1700000000", url)
}

func TestClient_Delete_Success(t *testing.T) {
	server := fakeS3ServerWithDelete(t, http.StatusOK, http.StatusOK, http.StatusNoContent)
	defer server.Close()
	client := newTestClient(t, server.URL)

	assert.NoError(t, client.Delete(context.Background(), "b2b_org_logos/uid-1/tmp-scratch.png"))
}

func TestClient_Delete_Error(t *testing.T) {
	server := fakeS3ServerWithDelete(t, http.StatusOK, http.StatusOK, http.StatusInternalServerError)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.Delete(context.Background(), "b2b_org_logos/uid-1/tmp-scratch.png")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-bucket")
}

func TestClient_Copy_Success(t *testing.T) {
	server := fakeS3ServerWithCopy(t, http.StatusOK, http.StatusOK, http.StatusOK)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.Copy(context.Background(), "b2b_org_logos/uid-1/tmp-scratch.png", "b2b_org_logos/uid-1.png")

	assert.NoError(t, err)
}

func TestClient_Copy_Error(t *testing.T) {
	server := fakeS3ServerWithCopy(t, http.StatusOK, http.StatusOK, http.StatusInternalServerError)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.Copy(context.Background(), "b2b_org_logos/uid-1/tmp-scratch.png", "b2b_org_logos/uid-1.png")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-bucket")
}
