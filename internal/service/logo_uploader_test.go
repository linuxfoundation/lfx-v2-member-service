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

// logoKeyPattern matches the immutable per-attempt key format
// b2b_org_logos/{uid}/{uuid}{ext} — never the deterministic {uid}{ext}.
var logoKeyPattern = regexp.MustCompile(`^b2b_org_logos/uid-1/[0-9a-f-]{36}\.png$`)

// stubObjectStore captures the Put call and returns a canned URL/error.
type stubObjectStore struct {
	url        string
	err        error
	gotKey     string
	gotType    string
	gotDataLen int
}

func (s *stubObjectStore) Put(_ context.Context, key, contentType string, data []byte) (string, error) {
	s.gotKey = key
	s.gotType = contentType
	s.gotDataLen = len(data)
	return s.url, s.err
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

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader("fake-png-bytes"), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.Equal(t, "uid-1", org.UID)
	assert.Regexp(t, logoKeyPattern, objectStore.gotKey, "key must be b2b_org_logos/{uid}/{unique}.png, never the deterministic {uid}.png")
	assert.Equal(t, "image/png", objectStore.gotType)
	assert.Equal(t, len("fake-png-bytes"), objectStore.gotDataLen)
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", orgWriter.gotInput.LogoURL)
	assert.True(t, orgWriter.validated, "precondition must be validated before the org write")
}

func TestLogoUploader_ContentTypeWithParameters(t *testing.T) {
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png; charset=binary", strings.NewReader("fake-png-bytes"), "")

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
	assert.Equal(t, "", objectStore.gotKey, "object store must not be called for a rejected content type")
}

func TestLogoUploader_OversizedBody(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	oversized := strings.NewReader(strings.Repeat("a", 2*1024*1024+1))
	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", oversized, "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey, "object store must not be called for an oversized body")
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

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader("fake-png-bytes"), "")

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

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader("fake-png-bytes"), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected not-found error, got %T: %v", err, err)
	assert.True(t, orgWriter.validated, "precondition must have been checked (and passed) before this call")
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", orgWriter.gotInput.LogoURL, "object store must still be called once the precondition check passes")
}

func TestLogoUploader_PreconditionFailurePreventsUpload(t *testing.T) {
	objectStore := &stubObjectStore{url: "https://cdn.example.com/should-not-be-used"}
	orgWriter := &stubLogoOrgWriter{validateErr: pkgerrors.NewPreconditionFailed("b2b org has been modified since last read")}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader("fake-png-bytes"), "\"stale-etag\"")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected precondition-failed error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey, "object store must not be called when the precondition check fails")
}

func TestLogoUploader_OrgNotFoundPreventsUpload(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{validateErr: pkgerrors.NewNotFound("b2b org not found")}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader("fake-png-bytes"), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected not-found error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey, "object store must not be called when the org does not exist")
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
	assert.Equal(t, "", objectStore.gotKey, "object store must not be called when the body can't be read")
}
