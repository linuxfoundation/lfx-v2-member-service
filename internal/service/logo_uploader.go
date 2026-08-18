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
	// and pkg/constants/logo.go's LogoCacheControl comment). It deliberately
	// excludes the content type's file extension: Content-Type is carried on
	// the object itself (Put's explicit parameter, preserved across Copy by
	// S3's default COPY metadata directive) rather than inferred from the
	// key, so an extension here would change key whenever a re-upload's
	// content type does — breaking convergence for exactly the case (e.g. a
	// PNG replaced by an SVG) that most needs an old, cached, or copied URL to
	// resolve to current bytes (see the LFXV2-2016 lfx-reviewer finding on PR
	// #87, logo_uploader.go:131).
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
	key := fmt.Sprintf("b2b_org_logos/%s", uid)
	// A distinct top-level prefix (not nested under key's own path) lets an S3
	// lifecycle rule expire abandoned scratch objects by prefix alone, with no
	// risk of ever matching the deterministic shared key above (LFXV2-2016
	// lfx-reviewer finding on PR #87). Named after the object-storage
	// definition's own key (org-logos-public), not the domain model, per
	// Antonia's naming question on lfx-v2-opentofu#246.
	scratchKey := fmt.Sprintf("org-logos-public-scratch/%s/%s%s", uid, uuid.NewString(), ext)

	if _, err := o.objectStore.Put(ctx, scratchKey, contentType, data); err != nil {
		return nil, fmt.Errorf("uploading logo for b2b org %s: %w", uid, err)
	}

	// Update is called with the scratch object's URL first, not key's — the
	// scratch object already exists (Put above just completed), so whatever
	// this call publishes always resolves to real bytes immediately. This is
	// what determines the race winner, via the same optimistic-concurrency
	// check as before; only after it succeeds does promotion to the prettier
	// shared key happen, and only a second Update — once that promotion has
	// actually succeeded — repoints Salesforce there. Publishing key's URL
	// before its bytes were guaranteed to exist left a gap where a reader
	// could hit a fresh ?v= URL between this call returning and the Copy
	// below completing, get a cache-miss (404 or stale bytes), and have the
	// CDN pin that miss under the new querystring for its full TTL even
	// though promotion later succeeded (see the LFXV2-2016 lfx-reviewer
	// finding on PR #87, logo_uploader.go:147).
	scratchURL := o.objectStore.VersionedURL(scratchKey)
	org, err := o.b2bOrgWriter.Update(ctx, uid, model.B2BOrgInput{LogoURL: scratchURL}, ifMatch)
	if err != nil {
		if delErr := o.objectStore.Delete(ctx, scratchKey); delErr != nil {
			slog.WarnContext(ctx, "failed to clean up scratch logo object after a failed update",
				"b2b_org_uid", uid, "scratch_key", scratchKey, "error", delErr)
		}
		return nil, err
	}

	// Compute the repoint precondition now, before promoting any bytes.
	//
	// org.IsParent was populated in place by the first Update's publishEvents
	// call (b2b_org_writer.go) — a writer-only derived field that the plain
	// reader behind validateForUpdate's precondition check never sets. Left
	// as-is, that asymmetry alone makes etag.LFXEtag's JSON marshal disagree
	// with the fresh read the repoint check below compares against (`omitempty`
	// drops IsParent only when false), so every repoint for a parent org would
	// fail its precondition check even though nothing else changed. Clear it
	// before hashing so this etag reflects the same shape GetB2BOrg returns
	// (LFXV2-2016 lfx-reviewer finding on PR #87).
	orgForEtag := *org
	orgForEtag.IsParent = false
	repointIfMatch, etagErr := etag.LFXEtag(&orgForEtag)
	if etagErr != nil {
		slog.ErrorContext(ctx, "failed to compute etag to repoint logo to its shared key after a successful update; leaving it pointed at the scratch object",
			"b2b_org_uid", uid, "error", etagErr)
		return org, nil
	}

	// Re-check that this attempt's scratch URL is still the org's current
	// logo pointer before attempting to promote it to the shared key below.
	// This is a cheap early exit that avoids even trying S3 calls once this
	// attempt is already known stale — it is not what makes promotion
	// race-safe; CopyIfNewer's generation token below is what closes that
	// window (LFXV2-2016 lfx-reviewer finding on PR #87).
	if precheckErr := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, repointIfMatch); precheckErr != nil {
		slog.WarnContext(ctx, "b2b org changed since this logo upload committed; abandoning promotion to shared key to avoid overwriting a newer upload",
			"b2b_org_uid", uid, "error", precheckErr)
		return org, nil
	}

	// This attempt has won and Salesforce already durably points at the
	// scratch object's URL, which exists. Promote those bytes to the shared
	// key via a server-side conditional copy (not a fresh Put from data) and
	// retry a few times. generation orders this promotion against any other
	// concurrent attempt's: CopyIfNewer refuses to let an older generation's
	// copy land once a newer one has already committed to key, which is what
	// actually closes the race the precheck above only narrows — without it,
	// a copy delayed by this loop's sleeps could land after a second, faster
	// upload has already promoted, physically clobbering key with this
	// (older, losing) attempt's bytes even though that upload's own repoint
	// Update already succeeded (LFXV2-2016 lfx-reviewer finding on PR #87).
	//
	// It is org.UpdatedAt — Salesforce's LastModifiedDate for the very Update
	// call that just won this attempt's race, fixed the instant that write
	// committed — not a local time.Now() read taken here. A wall-clock read at
	// this point is not ordered by the Salesforce commits it's supposed to
	// protect: attempt A could pass the precheck above and then stall (GC,
	// scheduler, anything) before reaching this line; attempt B could fully
	// commit and promote in the meantime; A would then sample a timestamp
	// larger than B's and be allowed to clobber B's already-promoted bytes
	// even though A is the older, losing attempt (LFXV2-2016 lfx-reviewer
	// finding on PR #87). Keying off org.UpdatedAt instead fixes each
	// attempt's generation at the moment its own winning Update landed, so a
	// later stall in this goroutine can no longer change it.
	// If every retry is exhausted, or a newer promotion has already won, org
	// is simply left pointing at the scratch object — already a normal,
	// correct-looking Logo_URL__c value, not a broken reference to repair —
	// and the next upload for this org promotes a fresh scratch object to key
	// as usual, making this one an ordinary orphan for that promotion to
	// replace.
	//
	// org.UpdatedAt.UnixNano() is not always strictly increasing across
	// distinct attempts: Salesforce's LastModifiedDate is reported at
	// millisecond precision, so two genuinely concurrent commits to the same
	// org can land in the same millisecond and produce an identical
	// generation. An earlier revision of this fix tried to break that tie
	// with a process-local monotonic counter, but the API chart runs
	// multiple replicas with no shared counter between them, so it could
	// invert real ordering instead of fixing it — a later-stalled attempt on
	// one replica could still receive a larger offset than an
	// already-promoted attempt on another (copilot-pull-request-reviewer
	// finding on PR #87, 2026-08-18). There is no signal available here that
	// can order two same-millisecond commits correctly across replicas, so
	// this no longer tries: CopyIfNewer treats an equal generation as not
	// stale and lets whichever attempt's copy physically reaches S3 first
	// win, same as it always has for any other tie.
	generation := org.UpdatedAt.UnixNano()
	var commitErr error
	for attempt := 1; attempt <= CommitPromoteAttempts; attempt++ {
		commitErr = o.objectStore.CopyIfNewer(ctx, scratchKey, key, generation)
		if commitErr == nil {
			break
		}
		if errors.Is(commitErr, port.ErrStalePromotion) {
			slog.WarnContext(ctx, "a newer logo upload already promoted to the shared key; abandoning this older promotion",
				"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "error", commitErr)
			return org, nil
		}
		if attempt < CommitPromoteAttempts {
			time.Sleep(commitPromoteRetryDelay)
		}
	}
	if commitErr != nil {
		slog.ErrorContext(ctx, "failed to promote logo to its shared key after the b2b org update already committed; leaving it pointed at the scratch object",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "attempts", CommitPromoteAttempts, "error", commitErr)
		return org, nil
	}

	// Promotion succeeded: repoint Salesforce at the shared key's URL. A
	// failure here leaves org pointing at the scratch object, which — same
	// as the Copy-failure case above — is already a valid, resolvable URL,
	// so this is logged and tolerated rather than surfaced as an upload
	// failure.
	keyURL := o.objectStore.VersionedURL(key)
	repointed, updateErr := o.b2bOrgWriter.Update(ctx, uid, model.B2BOrgInput{LogoURL: keyURL}, repointIfMatch)
	if updateErr != nil {
		slog.ErrorContext(ctx, "failed to repoint b2b org logo to its shared key after a successful promotion; leaving it pointed at the scratch object",
			"b2b_org_uid", uid, "key", key, "error", updateErr)
		return org, nil
	}

	// Deliberately not deleted here. This scratch object was already published
	// as Logo_URL__c by the first Update above (before Copy even ran), which
	// fired an async indexer publish — a reader that resolved that message (or
	// a CDN edge that cached the scratch URL) before this repoint could still
	// be mid-flight against the scratch key's bytes. Deleting it immediately
	// would race that propagation lag and turn a cached/in-flight reference
	// into a broken image with nothing to self-heal it, since the object would
	// be gone. Cleanup is deferred to the object store's own scratch-prefix
	// lifecycle rule (org-logos-public-scratch/, 2-day expiration — see
	// object-store-definitions.yaml in lfx-v2-opentofu), which gives that
	// window ample time to close before removal (LFXV2-2016 lfx-reviewer
	// finding on PR #87).
	return repointed, nil
}
