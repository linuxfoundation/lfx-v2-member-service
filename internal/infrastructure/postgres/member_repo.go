// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// MemberRepo handles PostgreSQL queries for members (accounts)
type MemberRepo struct {
	db *sqlx.DB
}

// NewMemberRepo creates a new MemberRepo
func NewMemberRepo(db *sqlx.DB) *MemberRepo {
	return &MemberRepo{db: db}
}

const memberQuery = `
SELECT DISTINCT ON (acc."Id")
    acc."Id" as sfid,
    COALESCE(acc."Name", '') as name, COALESCE(acc."Logo_URL__c", '') as logourl,
    COALESCE(acc."Website", '') as website, acc."CreatedDate" as createddate, acc."LastModifiedDate" as lastmodifieddate
FROM salesforce_b2b."Account" acc
INNER JOIN salesforce_b2b."Asset" a ON a."AccountId" = acc."Id"
INNER JOIN salesforce_b2b."Product2" p ON p."Id" = a."Product2Id"
WHERE p."Family" = 'Membership' AND a."IsDeleted" = false
`

// FetchAllMembers fetches all members (accounts with memberships) from PostgreSQL
func (r *MemberRepo) FetchAllMembers(ctx context.Context) ([]*model.Member, error) {
	slog.InfoContext(ctx, "fetching all members from PostgreSQL")

	var sqlMembers []SQLMember
	err := r.db.SelectContext(ctx, &sqlMembers, memberQuery)
	if err != nil {
		slog.ErrorContext(ctx, "failed to fetch members from PostgreSQL", "error", err)
		return nil, fmt.Errorf("failed to fetch members: %w", err)
	}

	slog.InfoContext(ctx, "fetched members from PostgreSQL", "count", len(sqlMembers))

	members := make([]*model.Member, 0, len(sqlMembers))
	for _, sqlM := range sqlMembers {
		m := convertSQLToMember(sqlM)
		members = append(members, m)
	}

	return members, nil
}

// convertSQLToMember converts a SQL result to a domain Member
func convertSQLToMember(s SQLMember) *model.Member {
	m := &model.Member{
		UID:     generateMemberUID(s.SFID),
		Name:    s.Name,
		LogoURL: s.LogoURL,
		Website: s.Website,
	}

	m.SFIDs = []string{s.SFID}

	if s.CreatedDate.Valid {
		m.CreatedAt = s.CreatedDate.Time
	} else {
		m.CreatedAt = time.Now()
	}

	if s.LastModifiedDate.Valid {
		m.UpdatedAt = s.LastModifiedDate.Time
	} else {
		m.UpdatedAt = m.CreatedAt
	}

	return m
}

// generateMemberUID generates a deterministic UUID v5 from an account Salesforce ID
func generateMemberUID(sfid string) string {
	namespace := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // DNS namespace
	return uuid.NewSHA1(namespace, []byte("lfx-member:"+sfid)).String()
}
