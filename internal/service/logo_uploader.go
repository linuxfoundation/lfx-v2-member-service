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

	// Validate the org exists and, if ifMatch is set, that it's still current —
	// before uploading any bytes. Uploading first (against a deterministic key)
	// let a request that later failed this check still overwrite storage; see
	// the LFXV2-2016 Copilot review on PR #87.
	// previousLogoURL is what Logo_URL__c held before this upload, captured
	// from the precondition read (which happens either way). Every path below
	// that abandons after the first Update has already committed the scratch
	// URL uses it to put the field back — see rollbackLogoURL.
	current, err := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, ifMatch)
	if err != nil {
		return nil, err
	}
	previousLogoURL := current.LogoURL
	if isScratchLogoURL(previousLogoURL) {
		previousLogoURL = ""
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
	scratchKey := fmt.Sprintf("%s%s/%s%s", logoScratchKeyPrefix, uid, uuid.NewString(), ext)

	if _, err := o.objectStore.Put(ctx, scratchKey, contentType, data); err != nil {
		return nil, fmt.Errorf("uploading logo for b2b org %s: %w", uid, err)
	}

	// Persist the scratch object's URL first, not key's — the scratch object
	// already exists (Put above just completed), so Salesforce never names
	// missing bytes. This determines the race winner via optimistic
	// concurrency. The scratch URL is deliberately not published to the
	// indexer: it expires, and losing the later shared-key publish must leave
	// consumers on their previous durable value rather than this transient
	// one. Once promotion succeeds, a normal Update both repoints Salesforce
	// and publishes the durable URL.
	scratchURL := o.objectStore.VersionedURL(scratchKey)
	org, err := o.b2bOrgWriter.UpdateWithoutPublish(ctx, uid, model.B2BOrgInput{LogoURL: &scratchURL}, ifMatch)
	if err != nil {
		o.discardScratchAfterFailedUpdate(ctx, uid, scratchKey, scratchURL, previousLogoURL)
		return nil, err
	}

	// Update's PATCH is conditioned on ifMatch (If-Unmodified-Since), so it
	// only ever succeeds for the attempt that actually wins the race — but the
	// record it returns comes from an unconditional re-fetch afterward (see
	// salesforce/b2b_org_writer.go's UpdateB2BOrg), and a concurrent logo
	// upload's own PATCH can land in the gap between this attempt's PATCH and
	// that re-fetch. When that happens, org describes the other upload's
	// commit, not this one's: its UpdatedAt, ETag, and LogoURL all belong to a
	// write this attempt never made. Using it as proof this attempt won would
	// compute the repoint precondition and promotion generation from someone
	// else's state and could promote this attempt's (older) bytes over that
	// upload's newer ones (copilot-pull-request-reviewer finding on PR #87,
	// 2026-08-18). This attempt's own PATCH already durably committed
	// LogoURL: scratchURL at some point regardless of what the re-fetch shows,
	// so if org.LogoURL is anything else, a later upload has already
	// superseded it and there is nothing left for this attempt to promote.
	if org.LogoURL != scratchURL {
		slog.WarnContext(ctx, "a concurrent logo upload committed between this attempt's update and its re-fetch; abandoning this attempt",
			"b2b_org_uid", uid, "scratch_key", scratchKey)
		// Same durability rule the rollback paths apply: that upload commits
		// its own scratch URL before promoting, so the field can still name an
		// expiring object, which is not a state to report as settled.
		if isScratchLogoURL(org.LogoURL) {
			return nil, errLogoUploadIncomplete()
		}
		return org, nil
	}

	// Compute the repoint precondition now, before promoting any bytes.
	//
	// IsParent is a writer-side derived projection, not a persisted Salesforce
	// field. Clear it before hashing so this ETag matches the shape returned by
	// the fresh read in ValidatePrecondition.
	orgForEtag := *org
	orgForEtag.IsParent = false
	repointIfMatch, etagErr := etag.LFXEtag(&orgForEtag)
	if etagErr != nil {
		slog.ErrorContext(ctx, "failed to compute etag to repoint logo to its shared key after a successful update",
			"b2b_org_uid", uid, "error", etagErr)
		return o.abandonUpload(ctx, uid, previousLogoURL, scratchURL)
	}

	// Re-check that this attempt's scratch URL is still the org's current
	// logo pointer before attempting to promote it to the shared key below.
	// This is a cheap early exit that avoids even trying S3 calls once this
	// attempt is already known stale — it is not what makes promotion
	// race-safe; CopyIfNewer's generation token below is what closes that
	// window (LFXV2-2016 lfx-reviewer finding on PR #87).
	if _, precheckErr := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, repointIfMatch); precheckErr != nil {
		slog.WarnContext(ctx, "b2b org changed since this logo upload committed; abandoning promotion to shared key to avoid overwriting a newer upload",
			"b2b_org_uid", uid, "error", precheckErr)
		// Reached by *any* concurrent change to the Account — a name edit or a
		// CDC-driven sync, not only a competing logo upload — so this is not a
		// rare S3-outage path. rollbackLogoURL re-reads and only writes if
		// Logo_URL__c still names this attempt's scratch object, so a genuine
		// competing upload is left alone.
		return o.abandonUpload(ctx, uid, previousLogoURL, scratchURL)
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
	// If every retry is exhausted, or a newer promotion has already won,
	// rollbackLogoURL puts Logo_URL__c back to its pre-upload value (including
	// explicitly clearing a first-ever logo) and the call reports failure.
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
	// finding on PR #87, 2026-08-18). A second revision tried treating an
	// equal generation as harmless — reasoning that an unbreakable tie may as
	// well let whichever copy reaches S3 first win — but that let a delayed
	// attempt sharing a generation with an already-promoted one overwrite it
	// later, a live regression rather than a missed optimization
	// (copilot-pull-request-reviewer finding on PR #87, 2026-08-18). There is
	// no signal available here that can order two same-millisecond commits
	// correctly across replicas, so this no longer tries to order them at
	// all: CopyIfNewer treats an equal generation as stale, same as a
	// strictly greater one, so the first attempt to actually promote for a
	// given generation wins and every later same-generation attempt is
	// safely dropped rather than risking an overwrite.
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
			// This is the accepted cost of the same-millisecond generation tie
			// documented above, so it is not exotic either. Where a newer
			// upload has already moved Logo_URL__c on, rollbackLogoURL's
			// ownership re-check declines and this stays a soft abandon that
			// reports that upload's state.
			return o.abandonUpload(ctx, uid, previousLogoURL, scratchURL)
		}
		if attempt < CommitPromoteAttempts {
			time.Sleep(commitPromoteRetryDelay)
		}
	}
	if commitErr != nil {
		slog.ErrorContext(ctx, "failed to promote logo to its shared key after the b2b org update already committed",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "key", key, "attempts", CommitPromoteAttempts, "error", commitErr)
		return o.abandonUpload(ctx, uid, previousLogoURL, scratchURL)
	}

	// Promotion succeeded: repoint Salesforce at the shared key's URL. The
	// bytes are now at both keys, but only the shared one is durable — the
	// scratch copy is still inside the expiring prefix — so a failure here is
	// rolled back like the others rather than left pointing at it.
	keyURL := o.objectStore.VersionedURL(key)
	repointed, updateErr := o.b2bOrgWriter.Update(ctx, uid, model.B2BOrgInput{LogoURL: &keyURL}, repointIfMatch)
	if updateErr != nil {
		slog.ErrorContext(ctx, "failed to repoint b2b org logo to its shared key after a successful promotion",
			"b2b_org_uid", uid, "key", key, "error", updateErr)

		// Promotion already succeeded, so the shared key holds this upload's
		// bytes. For a first-ever logo, retry the repoint against a fresh ETag
		// instead of clearing the field. Otherwise restore the previous URL:
		// if it names the same deterministic key, it still resolves to the new
		// bytes; a foreign previous URL genuinely undoes the upload.
		safeLogoURL := previousLogoURL
		if safeLogoURL == "" {
			safeLogoURL = keyURL
		}
		recovered, outcome := o.rollbackLogoURL(ctx, uid, safeLogoURL, scratchURL)
		switch outcome {
		case rollbackSuperseded:
			if sharesSharedLogoKey(recovered.LogoURL, keyURL) {
				o.b2bOrgWriter.PublishOrgUpdated(ctx, current, recovered)
			}
			return recovered, nil
		case rollbackRestored:
			if !sharesSharedLogoKey(safeLogoURL, keyURL) {
				return nil, errLogoUploadIncomplete()
			}
			slog.WarnContext(ctx, "logo bytes were promoted and the failed repoint was recovered with a durable shared-key URL",
				"b2b_org_uid", uid, "key", key)
			return recovered, nil
		default:
			return nil, errLogoUploadIncomplete()
		}
	}

	// Deliberately not deleted here. Although the transient update is not
	// published to the indexer, a direct Salesforce reader could observe and
	// cache the scratch URL before this repoint. Deleting it immediately would
	// race that read. Cleanup is deferred to the object store's scratch-prefix
	// lifecycle rule (org-logos-public-scratch/, 2-day expiration — see
	// object-store-definitions.yaml in lfx-v2-opentofu), which gives that
	// window ample time to close before removal (LFXV2-2016 lfx-reviewer
	// finding on PR #87).
	return repointed, nil
}

// discardScratchAfterFailedUpdate cleans up this attempt's scratch object
// after the first Update returned an error — but only once it has established
// that the error really was pre-commit.
//
// An Update error does not prove the Salesforce write failed: UpdateB2BOrg
// PATCHes and then re-fetches the record (salesforce/b2b_org_writer.go), and a
// failure in that re-fetch returns an error even though the PATCH already
// committed Logo_URL__c. Deleting the scratch object on that path points the
// committed field straight at a key that no longer exists — broken
// immediately, not merely at the scratch prefix's lifecycle expiry
// (copilot-pull-request-reviewer finding on PR #87).
//
// So this re-reads first. If Logo_URL__c does not name this attempt's scratch
// object the write never landed and the object is safe to remove. If it does,
// the write committed despite the error: the object stays (it is the one being
// referenced) and the field is restored or explicitly cleared. When
// the re-read itself fails, nothing is deleted — an orphan the lifecycle rule
// will reclaim is strictly better than a dangling reference.
func (o *logoUploaderOrchestrator) discardScratchAfterFailedUpdate(ctx context.Context, uid, scratchKey, scratchURL, previousLogoURL string) {
	fresh, readErr := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, "")
	if readErr != nil {
		slog.WarnContext(ctx, "cannot determine whether a failed logo update committed; leaving the scratch object for the lifecycle rule to reclaim",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "error", readErr)
		return
	}

	if fresh.LogoURL == scratchURL {
		slog.ErrorContext(ctx, "logo update reported an error but had already committed; keeping the object it now names",
			"b2b_org_uid", uid, "scratch_key", scratchKey)
		_, _ = o.rollbackLogoURL(ctx, uid, previousLogoURL, scratchURL)
		return
	}

	if delErr := o.objectStore.Delete(ctx, scratchKey); delErr != nil {
		slog.WarnContext(ctx, "failed to clean up scratch logo object after a failed update",
			"b2b_org_uid", uid, "scratch_key", scratchKey, "error", delErr)
	}
}

// rollbackOutcome distinguishes the three ways an attempted rollback can end.
// Collapsing them into "restored or not" made a superseded upload — where
// another attempt legitimately owns Logo_URL__c — indistinguishable from a
// rollback that could not be confirmed, and callers answered both with the
// same degraded success (copilot-pull-request-reviewer finding on PR #87,
// logo_uploader.go:328).
type rollbackOutcome int

const (
	// rollbackRestored means Logo_URL__c now holds the target value.
	rollbackRestored rollbackOutcome = iota
	// rollbackSuperseded means someone else already moved the field off this
	// attempt's scratch object and onto a value that does not expire (another
	// upload's durable URL, or no logo at all), so there was nothing to roll
	// back and the record read here is the current state.
	rollbackSuperseded
	// rollbackFailed means the field could not be confirmed off the scratch
	// object, which the lifecycle rule expires after 2 days.
	rollbackFailed
)

// abandonUpload gives up on this attempt and maps the rollback outcome onto
// the response. A superseded attempt reports the org that actually owns the
// logo now; every other outcome is a failure, including one where the rollback
// itself could not be confirmed and Salesforce is left naming an expiring
// object.
func (o *logoUploaderOrchestrator) abandonUpload(ctx context.Context, uid, safeLogoURL, scratchURL string) (*model.B2BOrg, error) {
	if superseded, outcome := o.rollbackLogoURL(ctx, uid, safeLogoURL, scratchURL); outcome == rollbackSuperseded {
		return superseded, nil
	}
	return nil, errLogoUploadIncomplete()
}

// rollbackLogoURL best-effort moves Logo_URL__c off this upload's scratch URL.
// Most callers restore the pre-upload value; an empty value explicitly clears
// a first-ever logo, while a post-promotion repoint failure can use the durable
// shared-key URL as the safe target.
//
// Without this, those paths leave Salesforce naming an object under the
// scratch prefix, which the object store's lifecycle rule deletes after 2 days
// — a logo that looked fine on upload becomes a 404 in Salesforce and in every
// consumer that copied the URL, and a reindex faithfully re-publishes the dead
// link. It does not self-heal until that org happens to upload again. The
// earlier reasoning that an abandoned scratch URL is "already a normal,
// correct-looking value, not a broken reference to repair" holds only until
// that expiry.
//
// It deliberately re-reads instead of reusing an etag computed earlier: some
// callers arrive here precisely because theirs is stale. That read also
// confirms this attempt still owns the field before writing, and the Update is
// conditional on the freshly-read etag, so an upload that already moved
// Logo_URL__c on is never clobbered — it simply reports rollbackSuperseded.
//
// Returns the org to report alongside the outcome: the restored record when it
// wrote, the freshly-read record of the upload that superseded this one, or
// nil when the rollback failed.
func (o *logoUploaderOrchestrator) rollbackLogoURL(ctx context.Context, uid, safeLogoURL, scratchURL string) (*model.B2BOrg, rollbackOutcome) {
	if isScratchLogoURL(safeLogoURL) {
		safeLogoURL = ""
	}
	fresh, err := o.b2bOrgWriter.ValidatePrecondition(ctx, uid, "")
	if err != nil {
		slog.ErrorContext(ctx, "failed to re-read b2b org to roll its logo back; leaving it pointed at the scratch object",
			"b2b_org_uid", uid, "error", err)
		return nil, rollbackFailed
	}
	if fresh.LogoURL != scratchURL {
		// Only a durable target makes this a clean handover. A competing
		// upload commits its own scratch URL before promoting (see the first
		// Update above), so the field can name *its* expiring object while it
		// is still mid-flight — reporting that as the settled current state
		// would answer 200 with a URL the lifecycle rule deletes in 2 days.
		// This attempt cannot repair someone else's in-flight write either, so
		// it declines to write and reports the outcome as unconfirmed.
		if isScratchLogoURL(fresh.LogoURL) {
			slog.ErrorContext(ctx, "b2b org logo points at another attempt's scratch object; leaving it for that upload to resolve",
				"b2b_org_uid", uid, "scratch_url", scratchURL)
			return nil, rollbackFailed
		}
		slog.WarnContext(ctx, "b2b org logo no longer points at this attempt's scratch object; nothing to roll back",
			"b2b_org_uid", uid, "scratch_url", scratchURL)
		return fresh, rollbackSuperseded
	}

	freshETag, etagErr := etag.LFXEtag(fresh)
	if etagErr != nil {
		slog.ErrorContext(ctx, "failed to compute etag to roll the b2b org logo back; leaving it pointed at the scratch object",
			"b2b_org_uid", uid, "error", etagErr)
		return nil, rollbackFailed
	}

	restored, updateErr := o.b2bOrgWriter.Update(ctx, uid, model.B2BOrgInput{LogoURL: &safeLogoURL}, freshETag)
	if updateErr != nil {
		slog.ErrorContext(ctx, "failed to move the b2b org logo off its scratch URL",
			"b2b_org_uid", uid, "error", updateErr)
		return nil, rollbackFailed
	}

	slog.InfoContext(ctx, "moved b2b org logo off its scratch URL after an incomplete upload",
		"b2b_org_uid", uid)
	return restored, rollbackRestored
}

// sharesSharedLogoKey reports whether rawURL addresses the same object as
// sharedKeyURL, ignoring the ?v= cache-buster. Because the shared logo key is
// deterministic per org, a previous logo uploaded through this same endpoint
// differs from the current one only by that suffix — so "restoring" it does
// not restore the bytes underneath it.
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

// errLogoUploadIncomplete is what a caller returns once rollbackLogoURL has
// confirmed the field was restored: the upload genuinely had no effect, so
// reporting success with a stale org would misrepresent it. Retrying is the
// correct client action for every path that reaches this.
func errLogoUploadIncomplete() error {
	return pkgerrors.NewServiceUnavailable("logo upload could not be completed; the organization's logo is unchanged — retry")
}
