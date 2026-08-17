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

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
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

// UploadB2BOrgLogo validates contentType against the allow-list (PNG/JPEG —
// SVG intentionally excluded for now, see pkg/constants/logo.go) and body size
// against MaxB2BOrgLogoSizeBytes, uploads to object storage, then updates the
// org's logo URL through the existing B2BOrgWriter.Update path.
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
	// this point — sniff the actual bytes so a mislabeled (or malicious) upload
	// can't reach object storage and get published as a public CDN URL under a
	// PNG/JPEG media type it doesn't have.
	if sniffed := http.DetectContentType(data); sniffed != mediaType {
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
	// then only writes to the real, shared key once B2BOrgWriter.Update has
	// confirmed — via its own optimistic-concurrency check — that this attempt
	// actually won.
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

	// This attempt has won: commit the bytes to the real key. A failure here
	// leaves Salesforce pointing at url slightly ahead of the object's actual
	// bytes at key — self-corrects on the next successful upload to the same
	// org, same as any other stale-object case this key strategy is built to
	// heal from.
	if _, err := o.objectStore.Put(ctx, key, contentType, data); err != nil {
		return nil, fmt.Errorf("committing logo for b2b org %s: %w", uid, err)
	}

	if delErr := o.objectStore.Delete(ctx, scratchKey); delErr != nil {
		slog.WarnContext(ctx, "failed to clean up scratch logo object after a successful update",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "error", delErr)
	}

	return org, nil
}
