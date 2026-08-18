// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package objectstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fakeS3ServerWithPromote stubs HeadObject for dstKey (headStatus, plus
// headMetadata echoed as x-amz-meta-* response headers so CopyIfNewer's
// stale-generation check can be exercised) and CopyObject (copyStatus), for
// testing Client.CopyIfNewer's HeadObject-then-conditional-CopyObject
// sequence. A HeadObject for any other path -- i.e. srcKey, which CopyIfNewer
// also reads to preserve Content-Type/Cache-Control across the promotion
// copy -- always succeeds with a stub Content-Type/Cache-Control, since in
// the real flow the scratch object was already durably Put before promotion
// is ever attempted. copyCalls, if non-nil, is incremented each time a
// CopyObject request actually reaches the server, so tests can assert it was
// skipped entirely (e.g. HeadObject alone already proved the promotion is
// stale).
func fakeS3ServerWithPromote(t *testing.T, dstKey string, headStatus int, headMetadata map[string]string, copyStatus int, copyCalls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			if !strings.Contains(r.URL.Path, dstKey) {
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("Cache-Control", "public, max-age=86400")
				w.WriteHeader(http.StatusOK)
				return
			}
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
	server := fakeS3ServerWithPromote(t, "b2b_org_logos/uid-1", http.StatusNotFound, nil, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	assert.NoError(t, err)
	assert.Equal(t, 1, copyCalls, "must attempt a create-only copy once HeadObject confirms the destination doesn't exist yet")
}

func TestClient_CopyIfNewer_DestinationExists_Success(t *testing.T) {
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, "b2b_org_logos/uid-1", http.StatusOK, nil, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	assert.NoError(t, err)
	assert.Equal(t, 1, copyCalls, "must attempt an ETag-conditional copy once HeadObject finds an existing, unstamped destination")
}

func TestClient_CopyIfNewer_StaleGenerationCaughtByHeadObject(t *testing.T) {
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, "b2b_org_logos/uid-1", http.StatusOK, map[string]string{"Promoted-At": "999999999999999999"}, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.ErrorIs(t, err, port.ErrStalePromotion)
	assert.Equal(t, 0, copyCalls, "a newer generation already recorded on the destination must be caught before ever attempting CopyObject")
}

func TestClient_CopyIfNewer_EqualGenerationIsStale(t *testing.T) {
	// generation is derived from Salesforce's millisecond-precision
	// LastModifiedDate, so two genuinely concurrent promotions for the same
	// org can carry an identical generation. There is no signal available
	// here that can order them correctly across the API chart's replicas, so
	// an equal existing generation must be treated as stale (same as a
	// strictly greater one) -- letting it through would allow a delayed
	// attempt that happens to share a generation with an already-promoted one
	// to overwrite it later, which is a live regression rather than a missed
	// optimization (LFXV2-2016 copilot-pull-request-reviewer finding on PR
	// #87, 2026-08-18, on an earlier revision that let equal generations
	// through on the mistaken assumption that an unbreakable tie is harmless
	// to leave unordered).
	copyCalls := 0
	server := fakeS3ServerWithPromote(t, "b2b_org_logos/uid-1", http.StatusOK, map[string]string{"Promoted-At": "1000"}, http.StatusOK, &copyCalls)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1000)

	require.ErrorIs(t, err, port.ErrStalePromotion)
	assert.Equal(t, 0, copyCalls, "an equal generation must be caught by HeadObject before ever attempting CopyObject")
}

func TestClient_CopyIfNewer_StaleGenerationCaughtByConditionalCopy(t *testing.T) {
	// Simulates the race CopyIfNewer exists to close: HeadObject observes no
	// generation, but a concurrent writer commits a newer one before this
	// CopyObject lands, so S3 itself rejects it with 412. The re-check after
	// the conflict must confirm that writer's generation is actually >= ours
	// before giving up -- a bare 412 alone doesn't prove that.
	dstKey := "b2b_org_logos/uid-1"
	headCalls := 0
	copyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, dstKey):
			headCalls++
			if headCalls > 1 {
				w.Header().Set("X-Amz-Meta-Promoted-At", "999999999999999999")
			}
			w.Header().Set("ETag", `"dst-etag"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			copyCalls++
			w.WriteHeader(http.StatusPreconditionFailed)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", dstKey, 1)

	require.ErrorIs(t, err, port.ErrStalePromotion)
	assert.Equal(t, 1, copyCalls, "the re-check after one conflict must catch the now-confirmed-newer generation before attempting another copy")
}

func TestClient_CopyIfNewer_ConflictFromOlderWriterRetriesAndWins(t *testing.T) {
	// A conditional-copy 412 only proves dstKey changed, not that whoever
	// changed it was newer than us. If the re-check shows their stamped
	// generation is still older than ours, this must retry against the fresh
	// ETag and win, instead of wrongly abandoning a promotion it should still
	// complete (LFXV2-2016 lfx-reviewer finding on PR #87).
	dstKey := "b2b_org_logos/uid-1"
	headCalls := 0
	copyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, dstKey):
			headCalls++
			if headCalls > 1 {
				w.Header().Set("X-Amz-Meta-Promoted-At", "1")
				w.Header().Set("ETag", `"dst-etag-2"`)
			} else {
				w.Header().Set("ETag", `"dst-etag-1"`)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			copyCalls++
			if r.Header.Get("If-Match") == `"dst-etag-1"` {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"fake-etag"</ETag></CopyObjectResult>`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", dstKey, 2)

	require.NoError(t, err)
	assert.Equal(t, 2, copyCalls, "must retry against the fresh ETag once the re-check shows the conflicting writer was actually older")
}

func TestClient_CopyIfNewer_ExhaustsCASAttempts(t *testing.T) {
	dstKey := "b2b_org_logos/uid-1"
	copyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, dstKey):
			w.Header().Set("ETag", `"dst-etag"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			copyCalls++
			w.WriteHeader(http.StatusPreconditionFailed)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", dstKey, 1)

	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrStalePromotion), "an unresolvable conflict is a real error, not a confirmed loss")
	assert.Equal(t, copyIfNewerCASAttempts, copyCalls)
}

func TestClient_CopyIfNewer_CopyObjectError(t *testing.T) {
	server := fakeS3ServerWithPromote(t, "b2b_org_logos/uid-1", http.StatusOK, nil, http.StatusInternalServerError, nil)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrStalePromotion), "a generic 500 must not be misclassified as a lost race")
	assert.Contains(t, err.Error(), "test-bucket")
}

func TestClient_CopyIfNewer_HeadObjectError(t *testing.T) {
	server := fakeS3ServerWithPromote(t, "b2b_org_logos/uid-1", http.StatusForbidden, nil, http.StatusOK, nil)
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrStalePromotion))
	assert.Contains(t, err.Error(), "test-bucket")
}

// TestClient_CopyIfNewer_PreservesContentTypeAndCacheControl guards against a
// regression where MetadataDirectiveReplace (needed to stamp the generation)
// silently dropped the promoted object's Content-Type and Cache-Control,
// since S3 does not merge REPLACE metadata with the source object's --
// anything not explicitly restated in the CopyObject request is lost
// (LFXV2-2016 lfx-reviewer finding on PR #87).
func TestClient_CopyIfNewer_PreservesContentTypeAndCacheControl(t *testing.T) {
	dstKey := "b2b_org_logos/uid-1"
	var gotContentType, gotCacheControl string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, dstKey):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
			gotContentType = r.Header.Get("Content-Type")
			gotCacheControl = r.Header.Get("Cache-Control")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"fake-etag"</ETag></CopyObjectResult>`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", dstKey, 1)

	require.NoError(t, err)
	assert.Equal(t, "image/png", gotContentType, "promotion must preserve the scratch object's Content-Type instead of defaulting to application/octet-stream")
	assert.Equal(t, "public, max-age=86400", gotCacheControl, "promotion must preserve the scratch object's Cache-Control instead of dropping it")
}

func TestClient_CopyIfNewer_SourceHeadObjectError(t *testing.T) {
	// dstKey missing (so CopyIfNewer proceeds past the destination check), but
	// the srcKey HeadObject it now performs to read Content-Type/Cache-Control
	// fails -- must surface as an error, not silently promote without them.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "b2b_org_logos/uid-1"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)

	err := client.CopyIfNewer(context.Background(), "b2b_org_logo_scratch/uid-1/tmp.png", "b2b_org_logos/uid-1", 1)

	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrStalePromotion))
	assert.Contains(t, err.Error(), "test-bucket")
}
