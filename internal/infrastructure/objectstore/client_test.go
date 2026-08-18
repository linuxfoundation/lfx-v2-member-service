// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package objectstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
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

// fakeS3ServerWithPromote stubs HeadObject (headStatus, plus headMetadata
// echoed as x-amz-meta-* response headers so CopyIfNewer's stale-generation
// check can be exercised) and CopyObject (copyStatus), for testing
// Client.CopyIfNewer's HeadObject-then-conditional-CopyObject sequence.
// copyCalls, if non-nil, is incremented each time a CopyObject request
// actually reaches the server, so tests can assert it was skipped entirely
// (e.g. HeadObject alone already proved the promotion is stale).
func fakeS3ServerWithPromote(t *testing.T, headStatus int, headMetadata map[string]string, copyStatus int, copyCalls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			for k, v := range headMetadata {
				w.Header().Set("X-Amz-Meta-"+k, v)
			}
			if headStatus == http.StatusOK {
				w.Header().Set("ETag", `"dst-etag"`)
			}
			w.WriteHeader(headStatus)
		case http.MethodPut:
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				if copyCalls != nil {
					*copyCalls++
				}
				w.WriteHeader(copyStatus)
				if copyStatus == http.StatusOK {
					_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"fake-etag"</ETag></CopyObjectResult>`))
				}
				return
			}
			w.WriteHeader(http.StatusOK)
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

	origNowUnixNano := nowUnixNano
	nowUnixNano = func() int64 { return 1700000000000000000 }
	defer func() { nowUnixNano = origNowUnixNano }()

	url, err := client.Put(context.Background(), "b2b_org_logos/uid-1.png", "image/png", []byte("fake-png-bytes"))

	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1700000000000000000", url)
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

	origNowUnixNano := nowUnixNano
	nowUnixNano = func() int64 { return 1700000000000000000 }
	defer func() { nowUnixNano = origNowUnixNano }()

	url := client.VersionedURL("b2b_org_logos/uid-1.png")

	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1700000000000000000", url)
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

func TestClient_CopyIfNewer_DestinationMissing_Success(t *testing.T) {
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, http.StatusNotFound, nil, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	assert.NoError(t, err)
	assert.Equal(t, 1, copyCalls, "must attempt a create-only copy once HeadObject confirms the destination doesn't exist yet")
}

func TestClient_CopyIfNewer_DestinationExists_Success(t *testing.T) {
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, http.StatusOK, nil, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	assert.NoError(t, err)
	assert.Equal(t, 1, copyCalls, "must attempt an ETag-conditional copy once HeadObject finds an existing, unstamped destination")
}

func TestClient_CopyIfNewer_StaleGenerationCaughtByHeadObject(t *testing.T) {
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, http.StatusOK, map[string]string{"Promoted-At": "999999999999999999"}, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.ErrorIs(t, err, port.ErrStalePromotion)
	assert.Equal(t, 0, copyCalls, "a newer generation already recorded on the destination must be caught before ever attempting CopyObject")
}

func TestClient_CopyIfNewer_StaleGenerationCaughtByConditionalCopy(t *testing.T) {
	// Simulates the race CopyIfNewer exists to close: HeadObject observes no
	// (or an older) generation, but a concurrent writer commits a newer one
	// before this CopyObject lands, so S3 itself rejects it with 412.
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, http.StatusOK, nil, http.StatusPreconditionFailed, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.ErrorIs(t, err, port.ErrStalePromotion)
	assert.Equal(t, 1, copyCalls, "the conditional CopyObject must still be attempted -- HeadObject alone can't see a writer that commits after it")
}

func TestClient_CopyIfNewer_CopyObjectError(t *testing.T) {
	server := fakeS3ServerWithPromote(t, http.StatusOK, nil, http.StatusInternalServerError, nil)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrStalePromotion), "a generic 500 must not be misclassified as a lost race")
	assert.Contains(t, err.Error(), "test-bucket")
}

func TestClient_CopyIfNewer_HeadObjectError(t *testing.T) {
	server := fakeS3ServerWithPromote(t, http.StatusForbidden, nil, http.StatusOK, nil)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrStalePromotion))
	assert.Contains(t, err.Error(), "test-bucket")
}
