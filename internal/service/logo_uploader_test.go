// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scratchKeyPattern matches the per-attempt scratch key format
// b2b_org_logos/{uid}/tmp-{uuid}{ext} — never the deterministic {uid}{ext}.
var scratchKeyPattern = regexp.MustCompile(`^b2b_org_logos/uid-1/tmp-[0-9a-f-]{36}\.png$`)

// deterministicLogoKey is the stable, reused-every-upload key for uid-1 —
// what makes a copy of a superseded logo URL converge to current bytes
// within the object's Cache-Control TTL instead of staying wrong forever.
const deterministicLogoKey = "b2b_org_logos/uid-1.png"

// validPNGBytes is the minimal 8-byte PNG signature — enough for
// http.DetectContentType to sniff "image/png", which UploadB2BOrgLogo now
// requires to match the declared Content-Type.
const validPNGBytes = "\x89PNG\r\n\x1a\n"

// stubObjectStore captures every Put/Delete call in order and returns a
// canned URL/error for Put, keyed per-call so tests can simulate the scratch
// write succeeding while the final commit write fails, or vice versa.
type stubObjectStore struct {
	url        string
	err        error
	commitErr  error
	deleteErr  error
	putKeys    []string
	gotType    string
	gotDataLen int
	deletedKey string
}

func (s *stubObjectStore) Put(_ context.Context, key, contentType string, data []byte) (string, error) {
	s.putKeys = append(s.putKeys, key)
	s.gotType = contentType
	s.gotDataLen = len(data)
	if key == deterministicLogoKey && s.commitErr != nil {
		return "", s.commitErr
	}
	if key != deterministicLogoKey && s.err != nil {
		return "", s.err
	}
	return s.url, nil
}

func (s *stubObjectStore) VersionedURL(_ string) string {
	return s.url
}

func (s *stubObjectStore) Delete(_ context.Context, key string) error {
	s.deletedKey = key
	return s.deleteErr
}

// gotKey is the scratch key from the first (always-attempted) Put call, kept
// for tests that only care whether the object store was touched at all.
func (s *stubObjectStore) gotKey() string {
	if len(s.putKeys) == 0 {
		return ""
	}
	return s.putKeys[0]
}

// stubLogoOrgWriter records the Update call's input. validateErr controls
// ValidatePrecondition's return value independently of err (Update's), so
// tests can simulate a precondition failure without ever reaching Update.
type stubLogoOrgWriter struct {
	org         *model.B2BOrg
	err         error
	validateErr error
	gotInput    model.B2BOrgInput
	validated   bool
}

func (w *stubLogoOrgWriter) Create(_ context.Context, _ string) (*model.B2BOrg, error) {
	return w.org, w.err
}

func (w *stubLogoOrgWriter) ValidatePrecondition(_ context.Context, _, _ string) error {
	w.validated = true
	return w.validateErr
}

func (w *stubLogoOrgWriter) Update(_ context.Context, _ string, input model.B2BOrgInput, _ string) (*model.B2BOrg, error) {
	w.gotInput = input
	return w.org, w.err
}

func TestLogoUploader_Happy(t *testing.T) {
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.Equal(t, "uid-1", org.UID)
	require.Len(t, objectStore.putKeys, 2, "expected a scratch write followed by the commit write to the deterministic key")
	assert.Regexp(t, scratchKeyPattern, objectStore.putKeys[0], "first write must be to a unique scratch key, not the deterministic one")
	assert.Equal(t, deterministicLogoKey, objectStore.putKeys[1], "commit write must land on the deterministic key so old URLs converge to current bytes")
	assert.Equal(t, "image/png", objectStore.gotType)
	assert.Equal(t, len(validPNGBytes), objectStore.gotDataLen)
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", orgWriter.gotInput.LogoURL)
	assert.True(t, orgWriter.validated, "precondition must be validated before the org write")
	assert.Equal(t, objectStore.putKeys[0], objectStore.deletedKey, "scratch key must be cleaned up once the commit write succeeds")
}

func TestLogoUploader_ContentTypeWithParameters(t *testing.T) {
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png; charset=binary", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.Equal(t, "image/png; charset=binary", objectStore.gotType, "raw content-type header is passed through to storage, only the parsed media type is used for validation")
}

func TestLogoUploader_UnsupportedContentType(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/svg+xml", strings.NewReader("<svg/>"), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called for a rejected content type")
}

func TestLogoUploader_ContentSniffMismatch(t *testing.T) {
	// Declared as PNG, but the bytes aren't — the caller-declared Content-Type
	// alone must not be trusted to publish arbitrary bytes under a PNG/JPEG URL.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/should-not-be-used"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader("this is not a png"), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called when the sniffed content type doesn't match the declared one")
}

func TestLogoUploader_OversizedBody(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	oversized := strings.NewReader(strings.Repeat("a", 2*1024*1024+1))
	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", oversized, "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called for an oversized body")
}

func TestLogoUploader_EmptyBody(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(""), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
}

func TestLogoUploader_ObjectStoreError(t *testing.T) {
	objectStore := &stubObjectStore{err: errors.New("s3 unavailable")}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	assert.False(t, orgWriter.gotInput != (model.B2BOrgInput{}), "org writer must not be called when the upload fails")
}

func TestLogoUploader_OrgWriterError(t *testing.T) {
	// Precondition passes, but the final Update call itself fails (e.g. the
	// org was deleted or modified in the narrow window between the preflight
	// check and this write) — a genuine race, not the ordering bug the
	// preflight check exists to close.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{err: pkgerrors.NewNotFound("b2b org not found")}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected not-found error, got %T: %v", err, err)
	assert.True(t, orgWriter.validated, "precondition must have been checked (and passed) before this call")
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", orgWriter.gotInput.LogoURL, "object store must still be called once the precondition check passes")
	require.Len(t, objectStore.putKeys, 1, "a losing/failing Update must never reach the shared deterministic key")
	assert.Regexp(t, scratchKeyPattern, objectStore.putKeys[0], "the only write attempted must be the scratch one")
	assert.Equal(t, objectStore.putKeys[0], objectStore.deletedKey, "the scratch object must still be cleaned up after a failed update")
}

func TestLogoUploader_CommitWriteErrorAfterSuccessfulUpdate(t *testing.T) {
	// Update has already committed the org's new Logo_URL__c — the commit
	// write to the shared key is what's left, and its failure must surface as
	// an error even though the Salesforce-side write already succeeded, so the
	// caller knows to retry rather than assume the logo is actually live.
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: errors.New("s3 unavailable"),
	}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	assert.True(t, orgWriter.validated)
	require.Len(t, objectStore.putKeys, 2, "expected both the scratch write and the attempted commit write")
	assert.Equal(t, deterministicLogoKey, objectStore.putKeys[1])
}

func TestLogoUploader_PreconditionFailurePreventsUpload(t *testing.T) {
	objectStore := &stubObjectStore{url: "https://cdn.example.com/should-not-be-used"}
	orgWriter := &stubLogoOrgWriter{validateErr: pkgerrors.NewPreconditionFailed("b2b org has been modified since last read")}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "\"stale-etag\"")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected precondition-failed error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called when the precondition check fails")
}

func TestLogoUploader_OrgNotFoundPreventsUpload(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{validateErr: pkgerrors.NewNotFound("b2b org not found")}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected not-found error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called when the org does not exist")
}

// errReader always fails on Read, simulating a client disconnect mid-upload.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) {
	return 0, errors.New("connection reset by peer")
}

func TestLogoUploader_BodyReadError(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", errReader{}, "")

	require.Error(t, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called when the body can't be read")
}
