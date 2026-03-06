// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/postgres"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"

	logging "github.com/linuxfoundation/lfx-v2-member-service/pkg/log"
)

func init() {
	logging.InitStructureLogConfig()
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	slog.InfoContext(ctx, "starting membership sync job")

	// Parse all config up front before connecting to any services.
	rdsDB := os.Getenv("RDSDB")
	if rdsDB == "" {
		return fmt.Errorf("RDSDB environment variable is required")
	}

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	natsTimeout := os.Getenv("NATS_TIMEOUT")
	if natsTimeout == "" {
		natsTimeout = "10s"
	}
	natsTimeoutDuration, err := time.ParseDuration(natsTimeout)
	if err != nil {
		return fmt.Errorf("invalid NATS timeout duration: %w", err)
	}

	natsMaxReconnect := os.Getenv("NATS_MAX_RECONNECT")
	if natsMaxReconnect == "" {
		natsMaxReconnect = "3"
	}
	natsMaxReconnectInt, err := strconv.Atoi(natsMaxReconnect)
	if err != nil {
		return fmt.Errorf("invalid NATS max reconnect value: %w", err)
	}

	natsReconnectWait := os.Getenv("NATS_RECONNECT_WAIT")
	if natsReconnectWait == "" {
		natsReconnectWait = "2s"
	}
	natsReconnectWaitDuration, err := time.ParseDuration(natsReconnectWait)
	if err != nil {
		return fmt.Errorf("invalid NATS reconnect wait duration: %w", err)
	}

	auditorTeamID := os.Getenv("FGA_AUDITOR_TEAM_ID")
	if auditorTeamID == "" {
		return fmt.Errorf("FGA_AUDITOR_TEAM_ID environment variable is required")
	}

	// Connect to PostgreSQL
	db, err := postgres.NewClient(rdsDB)
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer db.Close()
	slog.InfoContext(ctx, "connected to PostgreSQL")

	// Connect to NATS
	natsConfig := nats.Config{
		URL:           natsURL,
		Timeout:       natsTimeoutDuration,
		MaxReconnect:  natsMaxReconnectInt,
		ReconnectWait: natsReconnectWaitDuration,
	}

	natsClient, err := nats.NewClient(ctx, natsConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}
	defer natsClient.Close()
	slog.InfoContext(ctx, "connected to NATS")

	// Create source reader (PostgreSQL) and KV writer (NATS)
	membershipRepo := postgres.NewMembershipRepo(db)
	keyContactRepo := postgres.NewKeyContactRepo(db)
	memberRepo := postgres.NewMemberRepo(db)
	projectRepo := postgres.NewProjectRepo(db)
	natsStorage := nats.NewStorage(natsClient)
	fgaPublisher := nats.NewFGAPublisher(natsClient)

	// Create a combined source reader
	sourceReader := &combinedSourceReader{
		membershipRepo: membershipRepo,
		keyContactRepo: keyContactRepo,
		memberRepo:     memberRepo,
	}

	// Create and run the syncer
	syncer := usecaseSvc.NewMembershipSyncer(sourceReader, natsStorage, fgaPublisher, projectRepo, auditorTeamID)
	if err := syncer.Sync(ctx); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	slog.InfoContext(ctx, "membership sync job completed successfully")
	return nil
}

// combinedSourceReader combines MembershipRepo, KeyContactRepo, and MemberRepo into a single MembershipSourceReader
type combinedSourceReader struct {
	membershipRepo *postgres.MembershipRepo
	keyContactRepo *postgres.KeyContactRepo
	memberRepo     *postgres.MemberRepo
}

func (r *combinedSourceReader) FetchAllMemberships(ctx context.Context) ([]*model.Membership, error) {
	return r.membershipRepo.FetchAllMemberships(ctx)
}

func (r *combinedSourceReader) FetchAllKeyContacts(ctx context.Context) ([]*model.KeyContact, error) {
	return r.keyContactRepo.FetchAllKeyContacts(ctx)
}

func (r *combinedSourceReader) FetchAllMembers(ctx context.Context) ([]*model.Member, error) {
	return r.memberRepo.FetchAllMembers(ctx)
}
