// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// MembershipSyncer orchestrates the sync from PostgreSQL to NATS KV
type MembershipSyncer struct {
	sourceReader  port.MembershipSourceReader
	kvWriter      port.MembershipKVWriter
	fgaPublisher  port.FGAPublisher
	auditorTeamID string
}

// NewMembershipSyncer creates a new MembershipSyncer
func NewMembershipSyncer(sourceReader port.MembershipSourceReader, kvWriter port.MembershipKVWriter, fgaPublisher port.FGAPublisher, auditorTeamID string) *MembershipSyncer {
	return &MembershipSyncer{
		sourceReader:  sourceReader,
		kvWriter:      kvWriter,
		fgaPublisher:  fgaPublisher,
		auditorTeamID: auditorTeamID,
	}
}

// Sync performs the full sync: reads from PostgreSQL and writes to NATS KV
func (s *MembershipSyncer) Sync(ctx context.Context) error {
	start := time.Now()
	slog.InfoContext(ctx, "starting membership sync")

	// Purge existing data to remove stale entries and lookup keys
	for _, bucket := range []string{constants.KVBucketNameMemberships, constants.KVBucketNameMembershipContacts} {
		slog.InfoContext(ctx, "purging bucket before sync", "bucket", bucket)
		if err := s.kvWriter.PurgeBucket(ctx, bucket); err != nil {
			slog.ErrorContext(ctx, "failed to purge bucket", "bucket", bucket, "error", err)
			return err
		}
	}

	// Sync memberships
	membershipErrors := 0
	memberships, err := s.sourceReader.FetchAllMemberships(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch memberships from source", "error", err)
		return err
	}
	slog.InfoContext(ctx, "fetched memberships from source", "count", len(memberships))

	synced := 0
	skipped := 0
	for _, membership := range memberships {
		// Only sync memberships with product family "Membership" and status Active or Purchased
		if !strings.EqualFold(membership.Product.Family, "Membership") {
			skipped++
			continue
		}
		status := strings.ToLower(membership.Status)
		if status != "active" && status != "purchased" {
			skipped++
			continue
		}

		if err := s.kvWriter.WriteMembership(ctx, membership); err != nil {
			slog.ErrorContext(ctx, "failed to write membership to KV",
				"error", err,
				"membership_uid", membership.UID,
			)
			membershipErrors++
			continue
		}

		if err := s.fgaPublisher.UpdateMemberAccess(ctx, membership.UID, s.auditorTeamID); err != nil {
			slog.ErrorContext(ctx, "failed to publish fga-sync access for membership",
				"error", err,
				"membership_uid", membership.UID,
			)
			membershipErrors++
			continue
		}
		synced++
	}

	slog.InfoContext(ctx, "memberships synced",
		"total", len(memberships),
		"synced", synced,
		"skipped", skipped,
		"errors", membershipErrors,
	)

	// Sync key contacts
	contactErrors := 0
	contacts, err := s.sourceReader.FetchAllKeyContacts(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch key contacts from source", "error", err)
		return err
	}
	slog.InfoContext(ctx, "fetched key contacts from source", "count", len(contacts))

	for _, contact := range contacts {
		if err := s.kvWriter.WriteKeyContact(ctx, contact); err != nil {
			slog.ErrorContext(ctx, "failed to write key contact to KV",
				"error", err,
				"contact_uid", contact.UID,
			)
			contactErrors++
			continue
		}
	}

	duration := time.Since(start)
	slog.InfoContext(ctx, "membership sync completed",
		"duration", duration,
		"memberships_total", len(memberships),
		"memberships_errors", membershipErrors,
		"contacts_total", len(contacts),
		"contacts_errors", contactErrors,
	)

	return nil
}
