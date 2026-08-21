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
	"net/url"
	"strings"
	"time"

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
	svgMediaType          = "image/svg+xml"
	logoScratchKeyPrefix  = "org-logos-public-scratch/"
	logoVersionQueryParam = "v"
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
	// promotionArbiterRounds bounds how many times a contended promotion may
	// re-consult Salesforce and re-attempt above the winning stamp. Only the
	// single attempt Salesforce currently points at can take this path, so it
	// converges; the bound exists so a pathological churn of concurrent
	// uploads cannot spin here.
	promotionArbiterRounds = 3
	promoteTimeout         = 15 * time.Second
	rollbackTimeout        = 15 * time.Second
	scratchCleanupTimeout  = 3 * time.Second
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
	if _, ok := constants.AllowedB2BOrgLogoContentTypes[mediaType]; !ok {
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

	// keyURL is minted before the scratch write because the scratch key is
	// derived from it: the version token Salesforce ends up storing is what
	// makes a pending promotion recoverable. Given only the committed
	// Logo_URL__c, the exact scratch object holding the not-yet-promoted bytes
	// is derivable, so a process that dies between the Salesforce commit and
	// the promotion leaves behind a complete, self-describing state rather
	// than an orphan. Like the shared key, it carries no file extension —
	// Content-Type lives on the object.
	keyURL := o.objectStore.VersionedURL(key)
	scratchKey, scratchKeyErr := logoScratchKey(uid, keyURL)
	if scratchKeyErr != nil {
		return nil, fmt.Errorf("deriving scratch key for b2b org %s: %w", uid, scratchKeyErr)
	}

	if _, err := o.objectStore.Put(ctx, scratchKey, contentType, data); err != nil {
		return nil, fmt.Errorf("uploading logo for b2b org %s: %w", uid, err)
	}

	// While Salesforce may still point at keyURL without the shared key
	// holding its bytes, the scratch object is the only copy that can complete
	// the promotion — deleting it then would strand the record permanently.
	// It is therefore cleaned up only once that ambiguity is resolved: the
	// promotion landed, another upload demonstrably owns the URL, the commit
	// demonstrably did not land, or the rollback restored the previous value.
	scratchResolved := false
	defer func() {
		if !scratchResolved {
			slog.WarnContext(ctx, "retaining logo scratch object so the pending promotion stays recoverable",
				"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "logo_url", keyURL)
			return
		}
		// Clean up the temporary scratch object best-effort under a short bounded timeout.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), scratchCleanupTimeout)
		defer cleanupCancel()
		_ = o.objectStore.Delete(cleanupCtx, scratchKey)
	}()

	// Commit the durable URL to Salesforce first under the conditional If-Match check.
	// If the conditional update fails (e.g. CAS conflict or network error), the shared
	// key in S3 is NEVER overwritten, preserving the prior image bytes completely.
	// The commit is deliberately quiet: indexer/FGA consumers must not observe the
	// new URL until the bytes behind it actually exist at the shared key, so the
	// publish happens after promotion succeeds (and a failed promotion is rolled
	// back without any consumer ever having seen the intermediate state).
	updated, updateErr := o.b2bOrgWriter.UpdateWithoutPublish(ctx, uid, model.B2BOrgInput{LogoURL: &keyURL}, ifMatch)
	if updateErr != nil {
		// UpdateB2BOrg PATCHes and then re-fetches, so a returned error does
		// not prove the write was rejected — a failed re-fetch leaves
		// Logo_URL__c already holding keyURL. Treating that as uncommitted
		// would delete the scratch bytes while Salesforce points at a shared
		// key that was never promoted, so re-read and reconcile instead.
		reread, rereadErr := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, "")
		switch {
		case rereadErr != nil:
			// Outcome unknown: keep the scratch object, since the commit may have landed.
			slog.ErrorContext(ctx, "failed to update b2b org logo URL in salesforce and could not determine whether the write landed",
				"b2b_org_uid", uid, "key", key, "error", updateErr, "reread_error", rereadErr)
			return nil, updateErr
		case reread.LogoURL != keyURL:
			// The write did not land; nothing references the scratch object.
			scratchResolved = true
			slog.ErrorContext(ctx, "failed to update b2b org logo URL in salesforce",
				"b2b_org_uid", uid, "key", key, "error", updateErr)
			return nil, updateErr
		default:
			slog.WarnContext(ctx, "b2b org logo URL update reported an error but the write landed; continuing to promotion",
				"b2b_org_uid", uid, "key", key, "error", updateErr)
			updated = reread
		}
	}

	// Detach post-commit promotion from client cancellation so a disconnecting
	// client does not leave Salesforce pointing at an unpromoted key.
	promoteCtx, promoteCancel := context.WithTimeout(context.WithoutCancel(ctx), promoteTimeout)
	defer promoteCancel()

	// generation orders this promotion against any concurrent attempt.
	// CopyIfNewer refuses to let an older generation's copy land once a newer
	// one has already committed to key.
	generation := updated.UpdatedAt.UnixNano()
	var commitErr error
	for round := 1; round <= promotionArbiterRounds; round++ {
		commitErr = o.promote(promoteCtx, scratchKey, key, generation)
		if commitErr == nil {
			break
		}

		var stale *port.StalePromotionError
		if !errors.As(commitErr, &stale) {
			break
		}

		// Another attempt's bytes own the shared key. Generations come from
		// Salesforce's coarse LastModifiedDate, so "at least as new" does not
		// prove that attempt won — two commits in the same tick tie. The
		// conditional Salesforce update is the single global serialization
		// point for logo commits, so it, not the stamp, decides the owner.
		holder, holderErr := o.b2bOrgWriter.ValidatePrecondition(promoteCtx, uid, "")
		if holderErr != nil {
			slog.ErrorContext(promoteCtx, "failed to re-read b2b org to arbitrate a contended logo promotion",
				"b2b_org_uid", uid, "key", key, "error", holderErr)
			break
		}
		if holder.LogoURL != keyURL {
			slog.WarnContext(promoteCtx, "a newer logo upload owns the shared key; abandoning this promotion",
				"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key)
			// Nothing points at these bytes any more, and the winner publishes
			// its own state, so this attempt must not.
			scratchResolved = true
			return updated, nil
		}

		// Salesforce still points at this attempt's URL, so these are the bytes
		// that must end up at the shared key. Re-attempt strictly above the
		// stamp that beat us.
		generation = stale.ExistingGeneration + 1
	}
	if commitErr != nil {
		slog.ErrorContext(promoteCtx, "failed to promote logo to its shared key after salesforce update; rolling back",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "error", commitErr)
		// Only a successful rollback takes Salesforce off keyURL; until then
		// the scratch bytes remain the record's only recovery path.
		scratchResolved = o.rollback(ctx, uid, current, updated)
		return nil, pkgerrors.NewServiceUnavailable("logo upload could not be completed; retry")
	}

	scratchResolved = true

	// The shared key now holds these bytes, so the committed URL is safe to expose.
	o.b2bOrgWriter.PublishOrgUpdated(promoteCtx, current, updated)

	return updated, nil
}

// logoScratchKey derives the scratch object key from the versioned URL that
// will be committed to Salesforce, so the pending bytes of an interrupted
// promotion can be located from the persisted record alone.
func logoScratchKey(uid, keyURL string) (string, error) {
	parsed, err := url.Parse(keyURL)
	if err != nil {
		return "", fmt.Errorf("parsing versioned logo URL: %w", err)
	}
	version := parsed.Query().Get(logoVersionQueryParam)
	if version == "" || strings.ContainsRune(version, '/') {
		return "", fmt.Errorf("versioned logo URL has no usable %s token", logoVersionQueryParam)
	}
	return fmt.Sprintf("%s%s/%s", logoScratchKeyPrefix, uid, version), nil
}

// promote copies the scratch object onto the shared key, retrying transient
// failures. A stale-promotion loss is returned immediately for the caller to
// arbitrate — it is a contention outcome, not a transient error.
func (o *logoUploaderOrchestrator) promote(ctx context.Context, scratchKey, key string, generation int64) error {
	var err error
	for attempt := 1; attempt <= CommitPromoteAttempts; attempt++ {
		err = o.objectStore.CopyIfNewer(ctx, scratchKey, key, generation)
		if err == nil || errors.Is(err, port.ErrStalePromotion) {
			return err
		}
		if attempt < CommitPromoteAttempts {
			time.Sleep(commitPromoteRetryDelay)
		}
	}
	return err
}

// rollback quietly restores the org's pre-upload logo URL after a promotion
// failure, reporting whether Salesforce is known to be off the failed URL. It runs on its own context derived from the request's: an expired
// promotion deadline must not also deny the compensating write, and a
// disconnected client must not skip it. The restore is unpublished because the
// commit it compensates was never published either.
func (o *logoUploaderOrchestrator) rollback(ctx context.Context, uid string, current, updated *model.B2BOrg) bool {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	restoreURL := ""
	if current.LogoURL != "" && !isScratchLogoURL(current.LogoURL) {
		restoreURL = current.LogoURL
	}
	rollbackETag, _ := etag.LFXEtag(updated)
	if _, err := o.b2bOrgWriter.UpdateWithoutPublish(rollbackCtx, uid, model.B2BOrgInput{LogoURL: &restoreURL}, rollbackETag); err != nil {
		slog.ErrorContext(rollbackCtx, "failed to rollback salesforce logo URL after promotion failure",
			"b2b_org_uid", uid, "error", err)
		return false
	}
	return true
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
