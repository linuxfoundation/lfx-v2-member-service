// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/imageresize"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/svgsanitize"
)

// svgMediaType is the parsed (parameter-free) media type used to branch
// content-sniffing: http.DetectContentType has no SVG signature (SVG
// detection would require sniffing XML structure, which its mimesniff-based
// table doesn't do), so SVG uploads are validated by actually parsing and
// sanitizing them instead of comparing against a sniffed type.
const (
	svgMediaType         = "image/svg+xml"
	logoScratchKeyPrefix = "org-logos-public-scratch/"
)

// CommitPromoteAttempts and commitPromoteRetryDelay bound the retry loop that
// promotes an already-uploaded scratch object to the shared logo key once
// B2BOrgWriter.Update has already committed that key's URL to Salesforce — at
// that point a single transient failure must not be left to a future upload
// to fix (see the LFXV2-2016 Copilot review on PR #87). CommitPromoteAttempts
// is exported so tests can exercise the retry loop's boundary without
// hardcoding a number that would silently drift out of sync.
const (
	CommitPromoteAttempts   = 3
	commitPromoteRetryDelay = 200 * time.Millisecond
)

// LogoUploader uploads a B2B org logo to object storage and writes the
// resulting URL to the org's Salesforce Logo_URL__c field via B2BOrgWriter.
// Only durable logo states are published to indexer/FGA consumers.
type LogoUploader interface {
	UploadB2BOrgLogo(ctx context.Context, uid, contentType string, body io.Reader, ifMatch string) (*model.B2BOrg, error)
}

type logoUploaderOrchestrator struct {
	objectStore  port.ObjectStoreWriter
	b2bOrgWriter B2BOrgWriter
}

// NewLogoUploader constructs a LogoUploader.
func NewLogoUploader(objectStore port.ObjectStoreWriter, b2bOrgWriter B2BOrgWriter) LogoUploader {
	return &logoUploaderOrchestrator{objectStore: objectStore, b2bOrgWriter: b2bOrgWriter}
}

// UploadB2BOrgLogo validates contentType against the allow-list (PNG/JPEG/SVG,
// see pkg/constants/logo.go) and body size against MaxB2BOrgLogoSizeBytes,
// uploads to object storage, then updates the org's logo URL through the
// existing B2BOrgWriter.Update path.
func (o *logoUploaderOrchestrator) UploadB2BOrgLogo(ctx context.Context, uid, contentType string, body io.Reader, ifMatch string) (*model.B2BOrg, error) {
	mediaType, _, parseErr := mime.ParseMediaType(contentType)
	if parseErr != nil {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("unsupported logo content type %q", contentType))
	}
	ext, ok := constants.AllowedB2BOrgLogoContentTypes[mediaType]
	if !ok {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("unsupported logo content type %q", contentType))
	}

	// Read one byte past the limit so an oversized upload is rejected without
	// buffering the whole body.
	data, err := io.ReadAll(io.LimitReader(body, constants.MaxB2BOrgLogoSizeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading logo upload body for b2b org %s: %w", uid, err)
	}
	if len(data) > constants.MaxB2BOrgLogoSizeBytes {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("logo exceeds max size of %d bytes", constants.MaxB2BOrgLogoSizeBytes))
	}
	if len(data) == 0 {
		return nil, pkgerrors.NewValidation("logo upload body is empty")
	}

	// The declared Content-Type header is caller-controlled and unverified up to
	// this point. For binary types (PNG/JPEG), sniff the actual bytes so a mislabeled (or
	// malicious) upload can't reach object storage and get published as a
	// public CDN URL under a media type it doesn't have. SVG is XML, not a
	// sniffable binary signature, so it's validated by actually parsing and
	// sanitizing it below instead — that both confirms it's really an <svg>
	// document and strips anything unsafe (see pkg/svgsanitize).
	if mediaType == svgMediaType {
		sanitized, sanitizeErr := svgsanitize.Sanitize(data)
		if sanitizeErr != nil {
			return nil, pkgerrors.NewValidation(fmt.Sprintf("invalid or unsafe SVG upload: %v", sanitizeErr))
		}
		data = sanitized
	} else {
		if sniffed := http.DetectContentType(data); sniffed != mediaType {
			return nil, pkgerrors.NewValidation(fmt.Sprintf("logo content does not match declared content type %q (detected %q)", mediaType, sniffed))
		}

		// SVG is vector and skips this: raster logos larger than
		// MaxLogoDimensionPx in either dimension are downscaled to fit, not
		// rejected, preserving aspect ratio (LFXV2-2016, Eric Searcy's Monday
		// sync spec).
		resized, resizeErr := imageresize.ShrinkToMax(data, mediaType, constants.MaxLogoDimensionPx)
		if resizeErr != nil {
			return nil, pkgerrors.NewValidation(fmt.Sprintf("invalid logo image: %v", resizeErr))
		}
		data = resized
	}

	// Validate the org exists and that ifMatch is current before uploading any bytes.
	current, err := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, ifMatch)
	if err != nil {
		return nil, err
	}

	// key is deterministic and reused by every upload for this org — that's
	// what lets a copy of an old logo URL, once superseded, converge to
	// current bytes within the object's Cache-Control TTL instead of pointing
	// at permanently-frozen bytes (see object_store_writer.go's Put contract
	// and pkg/constants/logo.go's LogoCacheControl comment). It deliberately
	// excludes the content type's file extension: Content-Type is carried on
	// the object itself rather than inferred from the key.
	key := fmt.Sprintf("b2b_org_logos/%s", uid)
	scratchKey := fmt.Sprintf("%s%s/%s%s", logoScratchKeyPrefix, uid, uuid.NewString(), ext)

	if _, err := o.objectStore.Put(ctx, scratchKey, contentType, data); err != nil {
		return nil, fmt.Errorf("uploading logo for b2b org %s: %w", uid, err)
	}
	defer func() {
		// Clean up the temporary scratch object best-effort under a short bounded timeout.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cleanupCancel()
		_ = o.objectStore.Delete(cleanupCtx, scratchKey)
	}()

	// Commit the durable URL to Salesforce first under the conditional If-Match check.
	// If the conditional update fails (e.g. CAS conflict or network error), the shared
	// key in S3 is NEVER overwritten, preserving the prior image bytes completely.
	keyURL := o.objectStore.VersionedURL(key)
	updated, updateErr := o.b2bOrgWriter.Update(ctx, uid, model.B2BOrgInput{LogoURL: &keyURL}, ifMatch)
	if updateErr != nil {
		slog.ErrorContext(ctx, "failed to update b2b org logo URL in salesforce",
			"b2b_org_uid", uid, "key", key, "error", updateErr)
		return nil, updateErr
	}

	// Detach post-commit promotion from client cancellation so a disconnecting
	// client does not leave Salesforce pointing at an unpromoted key.
	promoteCtx, promoteCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer promoteCancel()

	// generation orders this promotion against any concurrent attempt.
	// CopyIfNewer refuses to let an older generation's copy land once a newer
	// one has already committed to key.
	generation := updated.UpdatedAt.UnixNano()
	var commitErr error
	for attempt := 1; attempt <= CommitPromoteAttempts; attempt++ {
		commitErr = o.objectStore.CopyIfNewer(promoteCtx, scratchKey, key, generation)
		if commitErr == nil {
			break
		}
		if errors.Is(commitErr, port.ErrStalePromotion) {
			slog.WarnContext(promoteCtx, "a newer logo upload already promoted to the shared key; abandoning this older promotion",
				"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "error", commitErr)
			return updated, nil
		}
		if attempt < CommitPromoteAttempts {
			time.Sleep(commitPromoteRetryDelay)
		}
	}
	if commitErr != nil {
		slog.ErrorContext(promoteCtx, "failed to promote logo to its shared key after salesforce update; rolling back",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "attempts", CommitPromoteAttempts, "error", commitErr)
		// Rollback Salesforce to the previous logo URL
		var restoreURL *string
		if current.LogoURL != "" && !isScratchLogoURL(current.LogoURL) {
			restoreURL = &current.LogoURL
		} else {
			empty := ""
			restoreURL = &empty
		}
		rollbackETag, _ := etag.LFXEtag(updated)
		_, rollbackErr := o.b2bOrgWriter.Update(promoteCtx, uid, model.B2BOrgInput{LogoURL: restoreURL}, rollbackETag)
		if rollbackErr != nil {
			slog.ErrorContext(promoteCtx, "failed to rollback salesforce logo URL after promotion failure",
				"b2b_org_uid", uid, "error", rollbackErr)
		}
		return nil, pkgerrors.NewServiceUnavailable("logo upload could not be completed; retry")
	}

	return updated, nil
}

// sharesSharedLogoKey reports whether rawURL addresses the same object as
// sharedKeyURL, ignoring the ?v= cache-buster.
func sharesSharedLogoKey(rawURL, sharedKeyURL string) bool {
	base := sharedKeyURL
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base = base[:i]
	}
	if base == "" || !strings.HasPrefix(rawURL, base) {
		return false
	}
	// Guard against a neighbouring key that merely shares this prefix.
	rest := rawURL[len(base):]
	return rest == "" || strings.HasPrefix(rest, "?")
}
