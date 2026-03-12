// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// MembershipSyncer orchestrates the sync from PostgreSQL to NATS KV
type MembershipSyncer struct {
	sourceReader    port.MembershipSourceReader
	kvWriter        port.MembershipKVWriter
	projectIDMapper port.ProjectIDMapper
}

// NewMembershipSyncer creates a new MembershipSyncer
func NewMembershipSyncer(sourceReader port.MembershipSourceReader, kvWriter port.MembershipKVWriter, projectIDMapper port.ProjectIDMapper) *MembershipSyncer {
	return &MembershipSyncer{
		sourceReader:    sourceReader,
		kvWriter:        kvWriter,
		projectIDMapper: projectIDMapper,
	}
}

// Sync performs the full sync: reads from PostgreSQL and writes to NATS KV
func (s *MembershipSyncer) Sync(ctx context.Context) error {
	start := time.Now()
	slog.InfoContext(ctx, "starting membership sync")

	// Fetch PCC→Salesforce project ID mapping and build reverse map
	var sfToPCC map[string]string
	if s.projectIDMapper != nil {
		pccToSF, errMap := s.projectIDMapper.FetchProjectIDMapping(ctx)
		if errMap != nil {
			slog.ErrorContext(ctx, "failed to fetch project ID mapping", "error", errMap)
			return errMap
		}
		sfToPCC = make(map[string]string, len(pccToSF))
		for pcc, sf := range pccToSF {
			sfToPCC[sf] = pcc
		}
		slog.InfoContext(ctx, "built project ID reverse mapping", "count", len(sfToPCC))
	}

	// Purge existing data to remove stale entries and lookup keys
	for _, bucket := range []string{
		constants.KVBucketNameMemberships,
		constants.KVBucketNameMembershipContacts,
		constants.KVBucketNameMembers,
	} {
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

	// Filter memberships to only sync active/purchased
	var syncedMemberships []*model.Membership
	skipped := 0
	for _, membership := range memberships {
		if !strings.EqualFold(membership.Product.Family, "Membership") {
			skipped++
			continue
		}
		status := strings.ToLower(membership.Status)
		if status != "active" && status != "purchased" {
			skipped++
			continue
		}
		syncedMemberships = append(syncedMemberships, membership)
	}

	// Fetch members from PostgreSQL
	members, err := s.sourceReader.FetchAllMembers(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch members from source", "error", err)
		return err
	}
	slog.InfoContext(ctx, "fetched members from source", "count", len(members))

	// Group synced memberships by MemberUID to compute MembershipSummary
	membershipsByMember := make(map[string][]*model.Membership)
	for _, membership := range syncedMemberships {
		if membership.MemberUID != "" {
			membershipsByMember[membership.MemberUID] = append(membershipsByMember[membership.MemberUID], membership)
		}
	}

	// Write members with computed summaries
	memberErrors := 0
	for _, member := range members {
		memberMemberships := membershipsByMember[member.UID]
		member.MembershipSummary = buildMembershipSummary(memberMemberships)

		if err := s.kvWriter.WriteMember(ctx, member); err != nil {
			slog.ErrorContext(ctx, "failed to write member to KV",
				"error", err,
				"member_uid", member.UID,
			)
			memberErrors++
			continue
		}

	}

	slog.InfoContext(ctx, "members synced",
		"total", len(members),
		"errors", memberErrors,
	)

	// Write memberships
	synced := 0
	for _, membership := range syncedMemberships {
		if err := s.kvWriter.WriteMembership(ctx, membership); err != nil {
			slog.ErrorContext(ctx, "failed to write membership to KV",
				"error", err,
				"membership_uid", membership.UID,
			)
			membershipErrors++
			continue
		}

		// Write PCC project ID alias lookups if mapping exists
		if sfToPCC != nil && membership.Project.ID != "" {
			if pccID, ok := sfToPCC[membership.Project.ID]; ok {
				if err := s.kvWriter.WriteProjectAliasLookups(ctx, pccID, membership.UID, membership.MemberUID); err != nil {
					slog.WarnContext(ctx, "failed to write project alias lookups",
						"pcc_id", pccID,
						"membership_uid", membership.UID,
						"error", err,
					)
				}
			}
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
		"members_total", len(members),
		"members_errors", memberErrors,
		"memberships_total", len(memberships),
		"memberships_errors", membershipErrors,
		"contacts_total", len(contacts),
		"contacts_errors", contactErrors,
	)

	return nil
}

// buildMembershipSummary builds a MembershipSummary from a list of memberships
func buildMembershipSummary(memberships []*model.Membership) *model.MembershipSummary {
	if len(memberships) == 0 {
		return &model.MembershipSummary{
			ActiveCount: 0,
			TotalCount:  0,
			Memberships: []model.MembershipSummaryItem{},
		}
	}

	summary := &model.MembershipSummary{
		TotalCount:  len(memberships),
		Memberships: make([]model.MembershipSummaryItem, 0, len(memberships)),
	}

	for _, m := range memberships {
		if strings.EqualFold(m.Status, "active") {
			summary.ActiveCount++
		}

		summary.Memberships = append(summary.Memberships, model.MembershipSummaryItem{
			UID:            m.UID,
			Name:           m.Name,
			Status:         m.Status,
			Year:           m.Year,
			Tier:           m.Tier,
			MembershipType: m.MembershipType,
			AutoRenew:      m.AutoRenew,
			StartDate:      m.StartDate,
			EndDate:        m.EndDate,
			Product: model.Product{
				ID:   m.Product.ID,
				Name: m.Product.Name,
			},
			Project: model.Project{
				ID:   m.Project.ID,
				Name: m.Project.Name,
				Slug: m.Project.Slug,
			},
		})
	}

	return summary
}
