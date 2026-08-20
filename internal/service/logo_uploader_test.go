// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scratchKeyPattern matches the per-attempt scratch key format
// org-logos-public-scratch/{uid}/{uuid}{ext} — a top-level prefix distinct
// from the deterministic b2b_org_logos/{uid} key, so an S3 lifecycle rule can
// target scratch objects by prefix alone.
var scratchKeyPattern = regexp.MustCompile(`^org-logos-public-scratch/uid-1/[0-9a-f-]{36}\.png$`)

// deterministicLogoKey is the stable, reused-every-upload key for uid-1 —
// what makes a copy of a superseded logo URL converge to current bytes
// within the object's Cache-Control TTL instead of staying wrong forever.
// It deliberately has no file extension: a content-type-derived extension
// would change key across a type change (e.g. PNG replaced by SVG),
// breaking convergence for exactly that case.
const deterministicLogoKey = "b2b_org_logos/uid-1"

// validPNGBytes is a real, fully-decodable 4x4 PNG — needed not just for
// http.DetectContentType's sniff check but also for ShrinkToMax's decode-config
// read (LFXV2-2016 dimension auto-shrink). 4x4 is well within
// constants.MaxLogoDimensionPx, so it always takes the resize no-op path and
// is returned byte-for-byte unchanged, preserving every test's exact-bytes
// assertions.
var validPNGBytes = string(mustEncodePNG(4, 4))

func mustEncodePNG(width, height int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// validSVGBytes is a well-formed, entirely benign SVG document.
const validSVGBytes = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"/></svg>`

// maliciousSVGBytes embeds a script and an event-handler attribute alongside
// otherwise-valid content, to prove UploadB2BOrgLogo's SVG path sanitizes
// rather than merely validates well-formedness.
const maliciousSVGBytes = `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(document.cookie)</script><circle cx="5" cy="5" r="4"/></svg>`

// stubObjectStore captures every Put/CopyIfNewer/Delete call in order.
// commitErr controls the promotion CopyIfNewer call (which retries
// commitPromoteAttempts times before giving up) independently of err (the
// scratch Put's error), so tests can simulate the scratch write succeeding
// while the promotion fails, or vice versa. commitErrCount, if nonzero, makes
// CopyIfNewer fail only that many times before succeeding, to exercise the
// retry loop itself.
type stubObjectStore struct {
	url              string
	err              error
	commitErr        error
	commitErrCount   int
	deleteErr        error
	putKeys          []string
	putData          [][]byte
	copyCalls        []copyCall
	versionedURLKeys []string
	gotType          string
	gotDataLen       int
	deletedKey       string
}

type copyCall struct {
	src, dst   string
	generation int64
}

func (s *stubObjectStore) Put(_ context.Context, key, contentType string, data []byte) (string, error) {
	s.putKeys = append(s.putKeys, key)
	s.putData = append(s.putData, data)
	s.gotType = contentType
	s.gotDataLen = len(data)
	if s.err != nil {
		return "", s.err
	}
	return s.url, nil
}

func (s *stubObjectStore) VersionedURL(key string) string {
	s.versionedURLKeys = append(s.versionedURLKeys, key)
	return s.url
}

func (s *stubObjectStore) Delete(_ context.Context, key string) error {
	s.deletedKey = key
	return s.deleteErr
}

func (s *stubObjectStore) CopyIfNewer(_ context.Context, src, dst string, generation int64) error {
	s.copyCalls = append(s.copyCalls, copyCall{src: src, dst: dst, generation: generation})
	if s.commitErr == nil {
		return nil
	}
	if s.commitErrCount > 0 && len(s.copyCalls) > s.commitErrCount {
		return nil
	}
	return s.commitErr
}

// gotKey is the scratch key from the first (always-attempted) Put call, kept
// for tests that only care whether the object store was touched at all.
func (s *stubObjectStore) gotKey() string {
	if len(s.putKeys) == 0 {
		return ""
	}
	return s.putKeys[0]
}

// stubLogoOrgWriter records every Update call's input. validateErr controls
// ValidatePrecondition's return value independently of err (Update's), so
// tests can simulate a precondition failure without ever reaching Update.
// repointErr, if set, is returned only from the second Update call — the
// repoint from the scratch URL to the shared key's URL, attempted once Copy
// has promoted the bytes there — independently of err, which governs the
// first (scratch-URL) call.
type stubLogoOrgWriter struct {
	org         *model.B2BOrg
	err         error
	validateErr error
	// validateErrFromCall, if > 0, makes ValidatePrecondition return
	// validateErr only from this 1-indexed call onward, leaving earlier calls
	// to succeed -- lets tests simulate the initial preflight check passing
	// but the later pre-promotion repoint precheck failing.
	validateErrFromCall int
	// validateErrOnCall, if > 0, makes only that one 1-indexed call return
	// validateErr, leaving every other call to succeed -- lets a test fail the
	// pre-promotion precheck while still allowing rollbackLogoURL's own
	// re-read afterwards.
	validateErrOnCall int
	validateCallCount int
	repointErr        error
	// racedLogoURL, if set, is what the first Update call returns as
	// org.LogoURL instead of the input's own LogoURL -- simulating a
	// concurrent logo upload's PATCH landing between this attempt's PATCH and
	// its own unconditional re-fetch, so the record handed back describes
	// that other upload's commit, not this one's.
	racedLogoURL string
	// previousLogoURL is what ValidatePrecondition reports as the org's
	// current Logo_URL__c. The upload path captures it up front as the value
	// to roll back to, so "" models an org uploading its first-ever logo --
	// the case that cannot be rolled back at all, since an empty LogoURL means
	// "leave unchanged" to buildAccountPatch.
	previousLogoURL string
	// rollbackLogoURL re-reads before writing and only proceeds while the org
	// still points at this attempt's scratch object. rollbackSeesLogoURL, if
	// set, overrides what that re-read reports -- letting a test simulate a
	// concurrent upload having already moved the field on.
	rollbackSeesLogoURL string
	gotInput            model.B2BOrgInput
	updateCalls         []model.B2BOrgInput
	ifMatches           []string
	validated           bool
}

// precondOrg is what ValidatePrecondition hands back. The first call reports
// the pre-upload state; later calls (the pre-promotion precheck and
// rollbackLogoURL's own re-read) report the scratch URL this attempt committed,
// unless rollbackSeesLogoURL overrides it.
func (w *stubLogoOrgWriter) precondOrg() *model.B2BOrg {
	base := &model.B2BOrg{UID: "uid-1"}
	if w.org != nil {
		clone := *w.org
		base = &clone
	}
	if w.validateCallCount <= 1 {
		base.LogoURL = w.previousLogoURL
		return base
	}
	if w.rollbackSeesLogoURL != "" {
		base.LogoURL = w.rollbackSeesLogoURL
		return base
	}
	// Whatever this attempt last committed -- i.e. its scratch URL.
	if n := len(w.updateCalls); n > 0 {
		base.LogoURL = w.updateCalls[n-1].LogoURL
	}
	return base
}

func (w *stubLogoOrgWriter) Create(_ context.Context, _ string) (*model.B2BOrg, error) {
	return w.org, w.err
}

func (w *stubLogoOrgWriter) ValidatePrecondition(_ context.Context, _, _ string) (*model.B2BOrg, error) {
	w.validated = true
	w.validateCallCount++
	if w.validateErrOnCall > 0 {
		if w.validateCallCount == w.validateErrOnCall {
			return nil, w.validateErr
		}
		return w.precondOrg(), nil
	}
	if w.validateErrFromCall > 0 && w.validateCallCount < w.validateErrFromCall {
		return w.precondOrg(), nil
	}
	if w.validateErr != nil {
		return nil, w.validateErr
	}
	return w.precondOrg(), nil
}

func (w *stubLogoOrgWriter) Update(_ context.Context, _ string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
	w.gotInput = input
	w.updateCalls = append(w.updateCalls, input)
	w.ifMatches = append(w.ifMatches, ifMatch)
	if w.org != nil {
		if len(w.updateCalls) == 1 && w.racedLogoURL != "" {
			w.org.LogoURL = w.racedLogoURL
		} else {
			w.org.LogoURL = input.LogoURL
		}
	}
	if len(w.updateCalls) >= 2 && w.repointErr != nil {
		return nil, w.repointErr
	}
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
	require.Len(t, objectStore.putKeys, 1, "expected only the scratch write via Put")
	assert.Regexp(t, scratchKeyPattern, objectStore.putKeys[0], "the only Put must be to a unique scratch key, not the deterministic one")
	require.Len(t, objectStore.copyCalls, 1, "expected the scratch object promoted to the deterministic key via Copy")
	assert.Equal(t, objectStore.putKeys[0], objectStore.copyCalls[0].src)
	assert.Equal(t, deterministicLogoKey, objectStore.copyCalls[0].dst, "promotion must land on the deterministic key so old URLs converge to current bytes")
	assert.Equal(t, "image/png", objectStore.gotType)
	assert.Equal(t, len(validPNGBytes), objectStore.gotDataLen)
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", orgWriter.gotInput.LogoURL)
	assert.True(t, orgWriter.validated, "precondition must be validated before the org write")
	assert.Equal(t, "", objectStore.deletedKey, "scratch key must not be synchronously deleted once published -- cleanup is deferred to the object store's own lifecycle rule so any in-flight propagation of the scratch URL isn't raced")

	require.Len(t, orgWriter.updateCalls, 2, "expected the scratch-URL update (determines the race winner), then the shared-key repoint (only after Copy promotes the bytes there)")
	require.Len(t, objectStore.versionedURLKeys, 2)
	assert.Regexp(t, scratchKeyPattern, objectStore.versionedURLKeys[0], "the first Update must publish the scratch object's URL -- bytes that already exist -- not the shared key's")
	assert.Equal(t, deterministicLogoKey, objectStore.versionedURLKeys[1], "the second Update must only run after Copy has promoted bytes to the shared key")
	assert.Equal(t, "", orgWriter.ifMatches[0], "first update forwards the original request's if_match")
	assert.NotEmpty(t, orgWriter.ifMatches[1], "the shared-key repoint must use a freshly computed if_match")
	assert.NotEqual(t, orgWriter.ifMatches[0], orgWriter.ifMatches[1])
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

func TestLogoUploader_SVGSanitizedBeforeUpload(t *testing.T) {
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.svg?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/svg+xml", strings.NewReader(maliciousSVGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, objectStore.putKeys, 1)
	assert.True(t, strings.HasSuffix(objectStore.putKeys[0], ".svg"), "scratch key must use the .svg extension")
	require.Len(t, objectStore.copyCalls, 1)
	assert.Equal(t, deterministicLogoKey, objectStore.copyCalls[0].dst, "the shared key must not embed the .svg extension")
	require.Len(t, objectStore.putData, 1)
	uploaded := string(objectStore.putData[0])
	assert.NotContains(t, uploaded, "script", "the bytes handed to Put must already be sanitized, not the raw malicious input")
	assert.NotContains(t, uploaded, "onload")
	assert.Contains(t, uploaded, "circle", "sanitization must not drop the benign content alongside the malicious content")
}

func TestLogoUploader_KeyStableAcrossContentTypeChange(t *testing.T) {
	// A PNG upload followed by an SVG upload for the same org must promote to
	// the identical shared key. If the key embedded a content-type-derived
	// extension, replacing a PNG logo with an SVG one (or vice versa) would
	// promote to a different key than the prior upload, leaving the old key's
	// object — and any cached or copied URL pointing at it — permanently
	// stale instead of converging within the Cache-Control TTL.
	pngStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1?v=1"}
	pngUploader := svc.NewLogoUploader(pngStore, &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}})
	_, err := pngUploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")
	require.NoError(t, err)
	require.Len(t, pngStore.copyCalls, 1)

	svgStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1?v=2"}
	svgUploader := svc.NewLogoUploader(svgStore, &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}})
	_, err = svgUploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/svg+xml", strings.NewReader(validSVGBytes), "")
	require.NoError(t, err)
	require.Len(t, svgStore.copyCalls, 1)

	assert.Equal(t, pngStore.copyCalls[0].dst, svgStore.copyCalls[0].dst, "the shared key must be identical regardless of content type")
	assert.Equal(t, deterministicLogoKey, pngStore.copyCalls[0].dst)
}

func TestLogoUploader_SVGRejectsInvalidContent(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/svg+xml", strings.NewReader("<html><body>not an svg</body></html>"), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called when the SVG fails to sanitize")
}

func TestLogoUploader_UnsupportedContentType(t *testing.T) {
	objectStore := &stubObjectStore{}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/gif", strings.NewReader("GIF89a"), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "expected validation error, got %T: %v", err, err)
	assert.Equal(t, "", objectStore.gotKey(), "object store must not be called for a rejected content type")
}

func TestLogoUploader_OversizedDimensionsShrunkBeforeUpload(t *testing.T) {
	// 2048x1024 exceeds constants.MaxLogoDimensionPx (1024) in width — the
	// upload must still succeed, with the bytes actually reaching the object
	// store downscaled to fit, aspect ratio preserved, rather than rejected.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)
	oversized := mustEncodePNG(2048, 1024)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", bytes.NewReader(oversized), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, objectStore.putData, 1)
	assert.Less(t, len(objectStore.putData[0]), len(oversized), "the shrunk image must be smaller than the original oversized upload")
	cfg, _, err := image.DecodeConfig(bytes.NewReader(objectStore.putData[0]))
	require.NoError(t, err)
	assert.Equal(t, 1024, cfg.Width)
	assert.Equal(t, 512, cfg.Height, "aspect ratio (2:1) must be preserved when downscaling to the max width")
}

func TestLogoUploader_SVGDimensionsNotShrunk(t *testing.T) {
	// SVG is vector, so it must never route through the raster resize path —
	// confirmed by asserting the sanitized bytes are untouched by it (nothing
	// in the SVG upload path could produce a decode error even though SVG
	// text isn't a decodable raster image).
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.svg?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/svg+xml", strings.NewReader(validSVGBytes), "")

	require.NoError(t, err)
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

func TestLogoUploader_AbandonsWhenConcurrentUploadRacesTheReFetch(t *testing.T) {
	// Update's PATCH is conditioned on ifMatch, so it only ever succeeds for
	// the attempt that actually wins the race -- but the record it returns
	// comes from an unconditional re-fetch afterward, and a concurrent logo
	// upload's own PATCH can land in the gap between this attempt's PATCH and
	// that re-fetch. When that happens, the returned org describes the other
	// upload's commit, not this one's, and must not be treated as proof this
	// attempt won (copilot-pull-request-reviewer finding on PR #87,
	// 2026-08-18).
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{
		org:          &model.B2BOrg{UID: "uid-1"},
		racedLogoURL: "https://cdn.example.com/org-logos-public-scratch/uid-1/other-attempt.png?v=99",
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err, "abandoning this attempt is tolerated, not surfaced as an upload failure")
	require.NotNil(t, org)
	assert.Equal(t, "https://cdn.example.com/org-logos-public-scratch/uid-1/other-attempt.png?v=99", org.LogoURL, "the returned org belongs to the concurrent commit, not this attempt")
	assert.Empty(t, objectStore.copyCalls, "promotion must never run once the re-fetch is known to belong to another upload")
	require.Len(t, orgWriter.updateCalls, 1, "only the scratch-URL update -- the repoint Update must never be attempted")
}

func TestLogoUploader_CommitWriteErrorAfterSuccessfulUpdate(t *testing.T) {
	// The first Update (to the scratch object's URL) has already committed —
	// org's Logo_URL__c already names bytes that exist. Promoting those bytes
	// to the shared key via Copy is what's left, and every retry here is
	// exhausted. This org has no previous logo to restore (previousLogoURL is
	// unset), so no rollback is possible and it stays pointed at the scratch
	// object, exactly as before — see
	// TestLogoUploader_RollsBackToPreviousLogoWhenPromotionFails for the same
	// path on an org that does have one.
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: errors.New("s3 unavailable"),
	}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.True(t, orgWriter.validated)
	require.Len(t, objectStore.putKeys, 1, "expected only the scratch Put")
	require.Len(t, objectStore.copyCalls, svc.CommitPromoteAttempts, "expected every promotion retry attempt to be exhausted")
	for _, c := range objectStore.copyCalls {
		assert.Equal(t, objectStore.putKeys[0], c.src)
		assert.Equal(t, deterministicLogoKey, c.dst)
	}
	require.Len(t, orgWriter.updateCalls, 1, "expected only the scratch-URL update -- no repoint is needed since it already points at real bytes")
	assert.Equal(t, "", objectStore.deletedKey, "scratch object must not be cleaned up once it becomes the object Salesforce names")
}

func TestLogoUploader_KeyRepointFailsAfterSuccessfulPromotion(t *testing.T) {
	// Copy succeeds, but the second Update -- repointing Salesforce from the
	// scratch URL to the shared key's URL -- fails. org already durably
	// points at the scratch object from the first Update, which is already a
	// valid, resolvable URL, so this must be tolerated rather than surfaced
	// as an upload failure.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{
		org:        &model.B2BOrg{UID: "uid-1"},
		repointErr: pkgerrors.NewPreconditionFailed("b2b org has been modified since last read"),
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, orgWriter.updateCalls, 2, "expected the scratch-URL update, then the failed repoint attempt")
	assert.Equal(t, "", objectStore.deletedKey, "scratch object must not be cleaned up when the repoint fails")
}

func TestLogoUploader_RepointPrecheckAbandonsPromotionWhenOrgChangedSinceCommit(t *testing.T) {
	// The initial precondition passes and the scratch-URL update commits, but
	// before Copy promotes those bytes to the shared key, the org has
	// changed again (e.g. a second, faster upload already won its own
	// promotion). Copy must never run in that case -- otherwise this
	// (older, losing) attempt could clobber the shared key with stale bytes
	// even though its own repoint would later be correctly rejected.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{
		org:                 &model.B2BOrg{UID: "uid-1"},
		validateErr:         pkgerrors.NewPreconditionFailed("b2b org has been modified since last read"),
		validateErrFromCall: 2,
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err, "abandoning the promotion is tolerated, not surfaced as an upload failure")
	require.NotNil(t, org)
	assert.Empty(t, objectStore.copyCalls, "Copy must never run once the pre-promotion precheck detects the org changed")
	require.Len(t, orgWriter.updateCalls, 1, "only the scratch-URL update -- the repoint Update must never be attempted")
}

func TestLogoUploader_RepointEtagIgnoresParentFlag(t *testing.T) {
	// org.IsParent is populated in place by the writer's publishEvents step
	// after the first Update -- the plain reader behind the precondition
	// check never sets it. The repoint if-match must be computed as though
	// IsParent were unset, matching what that fresh read produces, or every
	// repoint for a parent org would spuriously fail its precondition check.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1", IsParent: true}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.Len(t, orgWriter.ifMatches, 2)
	unparented := *orgWriter.org
	unparented.IsParent = false
	wantEtag, etagErr := etag.LFXEtag(&unparented)
	require.NoError(t, etagErr)
	assert.Equal(t, wantEtag, orgWriter.ifMatches[1], "repoint if-match must be computed as though IsParent were unset")
}

func TestLogoUploader_CommitWriteRetriesThenSucceeds(t *testing.T) {
	// The promotion Copy fails on its first attempt but succeeds on a retry —
	// the retry loop must paper over exactly this kind of transient failure
	// once Update has already committed, without surfacing an error.
	objectStore := &stubObjectStore{
		url:            "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr:      errors.New("s3 unavailable"),
		commitErrCount: svc.CommitPromoteAttempts - 1,
	}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, objectStore.copyCalls, svc.CommitPromoteAttempts)
	require.Len(t, orgWriter.updateCalls, 2, "expected the scratch-URL update, then the shared-key repoint once the retry succeeds")
	assert.Equal(t, "", objectStore.deletedKey, "scratch key must not be synchronously deleted once a retry succeeds -- cleanup is deferred to the object store's own lifecycle rule")
}

func TestLogoUploader_PromotionGenerationDerivedFromOrgUpdatedAt(t *testing.T) {
	// generation must be tied to Salesforce's own commit order (org.UpdatedAt,
	// i.e. LastModifiedDate) rather than a local wall-clock read taken after
	// the pre-promotion precheck -- otherwise an arbitrary scheduling delay
	// between the precheck and this line could let an older, losing attempt's
	// sampled timestamp outrank a competitor that fully committed and
	// promoted in between (LFXV2-2016 lfx-reviewer finding on PR #87).
	committedAt := time.Date(2026, 8, 18, 7, 42, 39, 0, time.UTC)
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1", UpdatedAt: committedAt}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.Len(t, objectStore.copyCalls, 1)
	assert.Equal(t, committedAt.UnixNano(), objectStore.copyCalls[0].generation, "promotion generation must be derived from org.UpdatedAt, not a local wall-clock read")
}

func TestLogoUploader_CommitAbandonsWhenNewerPromotionAlreadyWon(t *testing.T) {
	// CopyIfNewer itself (not the precheck) is what detects the race here: a
	// second, faster upload has already promoted a newer generation to the
	// shared key by the time this attempt's copy runs. The retry loop must
	// stop immediately rather than burn through every attempt, and must
	// tolerate it the same way every other "abandon promotion" path does.
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: port.ErrStalePromotion,
	}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err, "abandoning the promotion is tolerated, not surfaced as an upload failure")
	require.NotNil(t, org)
	require.Len(t, objectStore.copyCalls, 1, "must not retry once a newer promotion is detected")
	require.Len(t, orgWriter.updateCalls, 1, "only the scratch-URL update -- the repoint Update must never be attempted")
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

const previousLogoURL = "https://cdn.example.com/b2b_org_logos/uid-1?v=100"

func TestLogoUploader_RollsBackToPreviousLogoWhenPromotionFails(t *testing.T) {
	// Once the first Update has committed the scratch URL, every abandon path
	// leaves Salesforce naming an object under the scratch prefix -- which the
	// object store's lifecycle rule expires after 2 days, turning a logo that
	// looked fine on upload into a 404 with nothing to self-heal it. So the
	// field is put back to what it held before, and the upload is reported as
	// failed rather than as a degraded success.
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: errors.New("s3 unavailable"),
	}
	orgWriter := &stubLogoOrgWriter{
		org:             &model.B2BOrg{UID: "uid-1"},
		previousLogoURL: previousLogoURL,
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "an upload that had no lasting effect must not report success, got: %v", err)
	assert.Nil(t, org)
	require.Len(t, orgWriter.updateCalls, 2, "expected the scratch-URL update plus the rollback")
	assert.Equal(t, previousLogoURL, orgWriter.updateCalls[1].LogoURL, "rollback must restore the pre-upload URL")
	assert.NotEmpty(t, orgWriter.ifMatches[1], "rollback must be conditional on a freshly-read etag")
}

func TestLogoUploader_CannotRollBackFirstEverLogo(t *testing.T) {
	// buildAccountPatch (salesforce/b2b_org_writer.go) treats an empty LogoURL
	// as "leave unchanged" rather than "clear", so an org that had no logo
	// before has no restorable prior state. It stays pointed at the scratch
	// object and the call still succeeds -- the pre-existing behaviour, kept
	// deliberately rather than reported as a failure that the caller could not
	// act on. This org remains exposed to the scratch expiry.
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: errors.New("s3 unavailable"),
	}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}} // previousLogoURL zero
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, orgWriter.updateCalls, 1, "no rollback is possible, so only the scratch-URL update should have run")
}

func TestLogoUploader_SkipsRollbackWhenConcurrentUploadAlreadyMovedTheLogo(t *testing.T) {
	// rollbackLogoURL re-reads before writing. If Logo_URL__c no longer names
	// this attempt's scratch object, a later upload already owns the field and
	// restoring the older URL would clobber it.
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: errors.New("s3 unavailable"),
	}
	orgWriter := &stubLogoOrgWriter{
		org:                 &model.B2BOrg{UID: "uid-1"},
		previousLogoURL:     previousLogoURL,
		rollbackSeesLogoURL: "https://cdn.example.com/b2b_org_logos/uid-1?v=999",
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, orgWriter.updateCalls, 1, "the newer upload's value must be left alone")
}

func TestLogoUploader_RollsBackWhenAnyConcurrentOrgChangeFailsThePrecheck(t *testing.T) {
	// The pre-promotion precheck fails on *any* concurrent change to the
	// Account -- a name edit or a CDC-driven sync, not only a competing logo
	// upload -- so this path needs no S3 failure at all to be reached. It was
	// previously a silent abandon onto the expiring scratch prefix.
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{
		org:               &model.B2BOrg{UID: "uid-1"},
		previousLogoURL:   previousLogoURL,
		validateErr:       pkgerrors.NewPreconditionFailed("b2b org has been modified since last read"),
		validateErrOnCall: 2, // the precheck only; rollback's own re-read succeeds
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr, "got: %v", err)
	assert.Nil(t, org)
	require.Len(t, orgWriter.updateCalls, 2)
	assert.Equal(t, previousLogoURL, orgWriter.updateCalls[1].LogoURL)
	assert.Empty(t, objectStore.copyCalls, "promotion must not be attempted once the precheck failed")
}
