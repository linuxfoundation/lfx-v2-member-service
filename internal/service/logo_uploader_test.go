// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	keyedURLs        bool
	versionCounter   int
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

func (s *stubObjectStore) enableKeyedURLs() { s.keyedURLs = true }

func (s *stubObjectStore) VersionedURL(key string) string {
	s.versionedURLKeys = append(s.versionedURLKeys, key)
	// keyedURLs derives the URL from the key the way the real client does.
	// The default single fixed url is fine for most tests, but any test that
	// has to tell the scratch URL apart from the shared key's URL needs this.
	if s.keyedURLs {
		s.versionCounter++
		return fmt.Sprintf("https://cdn.example.com/%s?v=%d", key, s.versionCounter)
	}
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
	// to roll back to, so "" models an org uploading its first-ever logo.
	previousLogoURL string
	// rollbackLogoURL re-reads before writing and only proceeds while the org
	// still points at this attempt's scratch object. rollbackSeesLogoURL, if
	// set, overrides what that re-read reports -- letting a test simulate a
	// concurrent upload having already moved the field on.
	rollbackSeesLogoURL string
	// commitDespiteErr models UpdateB2BOrg's PATCH-then-re-fetch shape: the
	// write lands in Salesforce but the re-fetch afterwards fails, so Update
	// returns an error even though Logo_URL__c already changed.
	commitDespiteErr bool
	// committedLogoURL is what Salesforce actually holds -- set only by an
	// Update that committed, so a failed one is not mistaken for persisted
	// state by the precondition re-reads.
	committedLogoURL       string
	publishedOrg           *model.B2BOrg
	onUpdateWithoutPublish func()
	gotInput               model.B2BOrgInput
	updateCalls            []model.B2BOrgInput
	quietUpdateCalls       int
	ifMatches              []string
	validated              bool
}

func (w *stubLogoOrgWriter) PublishOrgUpdated(_ context.Context, _, org *model.B2BOrg) {
	w.publishedOrg = org
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
	base.LogoURL = w.committedLogoURL
	if base.LogoURL == "" {
		base.LogoURL = w.previousLogoURL
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

func inputLogoURL(input model.B2BOrgInput) string {
	if input.LogoURL == nil {
		return ""
	}
	return *input.LogoURL
}

func (w *stubLogoOrgWriter) Update(_ context.Context, _ string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
	return w.update(input, ifMatch)
}

func (w *stubLogoOrgWriter) UpdateWithoutPublish(_ context.Context, _ string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
	w.quietUpdateCalls++
	if w.onUpdateWithoutPublish != nil {
		w.onUpdateWithoutPublish()
	}
	return w.update(input, ifMatch)
}

func (w *stubLogoOrgWriter) update(input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
	w.gotInput = input
	w.updateCalls = append(w.updateCalls, input)
	w.ifMatches = append(w.ifMatches, ifMatch)
	logoURL := inputLogoURL(input)
	if w.org != nil {
		if len(w.updateCalls) == 1 && w.racedLogoURL != "" {
			w.org.LogoURL = w.racedLogoURL
		} else {
			w.org.LogoURL = logoURL
		}
	}
	if len(w.updateCalls) == 2 && w.repointErr != nil {
		return nil, w.repointErr
	}
	if w.err != nil {
		if w.commitDespiteErr {
			// PATCH landed; only the re-fetch after it failed.
			w.committedLogoURL = logoURL
		}
		return nil, w.err
	}
	w.committedLogoURL = logoURL
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
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", inputLogoURL(orgWriter.gotInput))
	assert.True(t, orgWriter.validated, "precondition must be validated before the org write")
	assert.Equal(t, objectStore.putKeys[0], objectStore.deletedKey, "scratch key must be cleaned up best-effort")

	require.Len(t, orgWriter.updateCalls, 1, "expected a single atomic update to Salesforce with the durable shared-key URL")
	require.Len(t, objectStore.versionedURLKeys, 1)
	assert.Equal(t, deterministicLogoKey, objectStore.versionedURLKeys[0], "Update must use the durable shared key's versioned URL")
	assert.Equal(t, "", orgWriter.ifMatches[0], "update forwards the original request's if_match")
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
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.svg?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/svg+xml", strings.NewReader(validSVGBytes), "")

	require.NoError(t, err)
}

func TestLogoUploader_ContentSniffMismatch(t *testing.T) {
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
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{err: pkgerrors.NewNotFound("b2b org not found")}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected not-found error, got %T: %v", err, err)
	assert.True(t, orgWriter.validated, "precondition must have been checked (and passed) before this call")
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1", inputLogoURL(orgWriter.gotInput))
	require.Len(t, objectStore.putKeys, 1)
	assert.Regexp(t, scratchKeyPattern, objectStore.putKeys[0])
	assert.Equal(t, objectStore.putKeys[0], objectStore.deletedKey, "the scratch object must still be cleaned up after a failed update")
}

func TestLogoUploader_CommitWriteRetriesThenSucceeds(t *testing.T) {
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
	require.Len(t, orgWriter.updateCalls, 1, "expected a single atomic update with the shared key URL")
}

func TestLogoUploader_PromotionGenerationDerivedFromOrgUpdatedAt(t *testing.T) {
	committedAt := time.Date(2026, 8, 18, 7, 42, 39, 0, time.UTC)
	objectStore := &stubObjectStore{url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1"}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1", UpdatedAt: committedAt}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	_, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.Len(t, objectStore.copyCalls, 1)
	assert.Equal(t, committedAt.UnixNano(), objectStore.copyCalls[0].generation, "promotion generation must be derived from org.UpdatedAt")
}

func TestLogoUploader_CommitAbandonsWhenNewerPromotionAlreadyWon(t *testing.T) {
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: port.ErrStalePromotion,
	}
	orgWriter := &stubLogoOrgWriter{org: &model.B2BOrg{UID: "uid-1"}}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, objectStore.copyCalls, 1, "must not retry once a newer promotion is detected")
	require.Len(t, orgWriter.updateCalls, 1, "Salesforce was updated before promotion was detected stale")
}

func TestLogoUploader_PromotionFailureRollsBackSalesforce(t *testing.T) {
	objectStore := &stubObjectStore{
		url:       "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
		commitErr: errors.New("s3 copy failure"),
	}
	orgWriter := &stubLogoOrgWriter{
		org:             &model.B2BOrg{UID: "uid-1"},
		previousLogoURL: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=old",
	}
	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(context.Background(), "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.Error(t, err)
	var svcErr pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &svcErr)
	assert.Nil(t, org)
	require.Len(t, orgWriter.updateCalls, 2, "must update Salesforce then rollback on promotion failure")
	assert.Equal(t, "https://cdn.example.com/b2b_org_logos/uid-1.png?v=old", inputLogoURL(orgWriter.updateCalls[1]), "rollback must restore previous logo URL")
}

func TestLogoUploader_CanceledContextAfterCommitStillPromotes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	objectStore := &stubObjectStore{
		url: "https://cdn.example.com/b2b_org_logos/uid-1.png?v=1",
	}
	orgWriter := &stubLogoOrgWriter{
		org: &model.B2BOrg{UID: "uid-1"},
	}
	orgWriter.onUpdateWithoutPublish = func() {
		// Simulate client disconnecting immediately after Salesforce update commits.
		cancel()
	}

	uploader := svc.NewLogoUploader(objectStore, orgWriter)

	org, err := uploader.UploadB2BOrgLogo(ctx, "uid-1", "image/png", strings.NewReader(validPNGBytes), "")

	require.NoError(t, err)
	require.NotNil(t, org)
	require.Len(t, objectStore.copyCalls, 1, "promotion must have run despite canceled request context")
	require.Len(t, orgWriter.updateCalls, 1)
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
