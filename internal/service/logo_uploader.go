// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/svgsanitize"
)

// svgMediaType is the parsed (parameter-free) media type used to branch
// content-sniffing: http.DetectContentType has no SVG signature (SVG
// detection would require sniffing XML structure, which its mimesniff-based
// table doesn't do), so SVG uploads are validated by actually parsing and
// sanitizing them instead of comparing against a sniffed type.
const svgMediaType = "image/svg+xml"

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
// resulting URL to the org's Salesforce Logo_URL__c field via B2BOrgWriter —
// reusing its existing PATCH + indexer/FGA publish path (LFXV2-2016).
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
	} else if sniffed := http.DetectContentType(data); sniffed != mediaType {
		return nil, pkgerrors.NewValidation(fmt.Sprintf("logo content does not match declared content type %q (detected %q)", mediaType, sniffed))
	}

	// Validate the org exists and, if ifMatch is set, that it's still current —
	// before uploading any bytes. Uploading first (against a deterministic key)
	// let a request that later failed this check still overwrite storage; see
	// the LFXV2-2016 Copilot review on PR #87.
	if err := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, ifMatch); err != nil {
		return nil, err
	}

	// key is deterministic and reused by every upload for this org — that's
	// what lets a copy of an old logo URL, once superseded, converge to
	// current bytes within the object's Cache-Control TTL instead of pointing
	// at permanently-frozen bytes (see object_store_writer.go's Put contract
	// and pkg/constants/logo.go's LogoCacheControl comment).
	//
	// A racing/losing upload must never be the one to write here, though — two
	// concurrent uploads both writing key directly can leave it holding the
	// loser's bytes even after the winner's Update call has already returned
	// success (see the LFXV2-2016 Copilot review on PR #87). So each attempt
	// first writes to its own scratch key (catching an upload failure before
	// touching Salesforce, same rationale as the precondition check above),
	// then only promotes to the real, shared key once B2BOrgWriter.Update has
	// confirmed — via its own optimistic-concurrency check, which this
	// endpoint requires (if_match is Required in the Goa design, unlike the
	// sibling update-b2b-org method) specifically because it's the only
	// B2BOrgWriter caller that also writes shared object-storage bytes — that
	// this attempt actually won.
	key := fmt.Sprintf("b2b_org_logos/%s%s", uid, ext)
	scratchKey := fmt.Sprintf("b2b_org_logos/%s/tmp-%s%s", uid, uuid.NewString(), ext)

	if _, err := o.objectStore.Put(ctx, scratchKey, contentType, data); err != nil {
		return nil, fmt.Errorf("uploading logo for b2b org %s: %w", uid, err)
	}

	url := o.objectStore.VersionedURL(key)
	org, err := o.b2bOrgWriter.Update(ctx, uid, model.B2BOrgInput{LogoURL: url}, ifMatch)
	if err != nil {
		if delErr := o.objectStore.Delete(ctx, scratchKey); delErr != nil {
			slog.WarnContext(ctx, "failed to clean up scratch logo object after a failed update",
				"b2b_org_uid", uid, "scratch_key", scratchKey, "error", delErr)
		}
		return nil, err
	}

	// This attempt has won and Salesforce already points at url: promote the
	// already-uploaded scratch bytes to key via a server-side copy (not a
	// fresh Put from data) and retry a few times. Unlike the pre-Update
	// scratch write, a failure here can't self-heal on its own — Salesforce
	// now names a key with no matching object (or stale bytes, if key already
	// existed) and nothing will retry it unless this org gets a logo uploaded
	// again (see the LFXV2-2016 Copilot review on PR #87).
	var commitErr error
	for attempt := 1; attempt <= CommitPromoteAttempts; attempt++ {
		if commitErr = o.objectStore.Copy(ctx, scratchKey, key); commitErr == nil {
			break
		}
		if attempt < CommitPromoteAttempts {
			time.Sleep(commitPromoteRetryDelay)
		}
	}
	if commitErr != nil {
		slog.ErrorContext(ctx, "failed to promote logo to its shared key after the b2b org update already committed",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "attempts", CommitPromoteAttempts, "error", commitErr)
		return nil, fmt.Errorf("committing logo for b2b org %s: %w", uid, commitErr)
	}

	if delErr := o.objectStore.Delete(ctx, scratchKey); delErr != nil {
		slog.WarnContext(ctx, "failed to clean up scratch logo object after a successful update",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "error", delErr)
	}

	return org, nil
}
