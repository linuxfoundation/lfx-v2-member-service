// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
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
	ctx := context.Background()

	slog.InfoContext(ctx, "starting membership sync job")

	// Connect to PostgreSQL
	rdsDB := os.Getenv("RDSDB")
	if rdsDB == "" {
		log.Fatal("RDSDB environment variable is required")
	}

	db, err := postgres.NewClient(rdsDB)
	if err != nil {
		log.Fatalf("failed to connect to PostgreSQL: %v", err)
	}
	defer db.Close()
	slog.InfoContext(ctx, "connected to PostgreSQL")

	// Connect to NATS
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
		log.Fatalf("invalid NATS timeout duration: %v", err)
	}

	natsMaxReconnect := os.Getenv("NATS_MAX_RECONNECT")
	if natsMaxReconnect == "" {
		natsMaxReconnect = "3"
	}
	natsMaxReconnectInt, err := strconv.Atoi(natsMaxReconnect)
	if err != nil {
		log.Fatalf("invalid NATS max reconnect value: %v", err)
	}

	natsReconnectWait := os.Getenv("NATS_RECONNECT_WAIT")
	if natsReconnectWait == "" {
		natsReconnectWait = "2s"
	}
	natsReconnectWaitDuration, err := time.ParseDuration(natsReconnectWait)
	if err != nil {
		log.Fatalf("invalid NATS reconnect wait duration: %v", err)
	}

	natsConfig := nats.Config{
		URL:           natsURL,
		Timeout:       natsTimeoutDuration,
		MaxReconnect:  natsMaxReconnectInt,
		ReconnectWait: natsReconnectWaitDuration,
	}

	natsClient, err := nats.NewClient(ctx, natsConfig)
	if err != nil {
		log.Fatalf("failed to connect to NATS: %v", err)
	}
	defer natsClient.Close()
	slog.InfoContext(ctx, "connected to NATS")

	auditorTeamID := os.Getenv("FGA_AUDITOR_TEAM_ID")
	if auditorTeamID == "" {
		log.Fatal("FGA_AUDITOR_TEAM_ID environment variable is required")
	}

	// Create source reader (PostgreSQL) and KV writer (NATS)
	membershipRepo := postgres.NewMembershipRepo(db)
	keyContactRepo := postgres.NewKeyContactRepo(db)
	natsStorage := nats.NewStorage(natsClient)
	fgaPublisher := nats.NewFGAPublisher(natsClient)

	// Create a combined source reader
	sourceReader := &combinedSourceReader{
		membershipRepo: membershipRepo,
		keyContactRepo: keyContactRepo,
	}

	// Create and run the syncer
	syncer := usecaseSvc.NewMembershipSyncer(sourceReader, natsStorage, fgaPublisher, auditorTeamID)
	if err := syncer.Sync(ctx); err != nil {
		log.Fatalf("sync failed: %v", err)
	}

	slog.InfoContext(ctx, "membership sync job completed successfully")
}

// combinedSourceReader combines MembershipRepo and KeyContactRepo into a single MembershipSourceReader
type combinedSourceReader struct {
	membershipRepo *postgres.MembershipRepo
	keyContactRepo *postgres.KeyContactRepo
}

func (r *combinedSourceReader) FetchAllMemberships(ctx context.Context) ([]*model.Membership, error) {
	return r.membershipRepo.FetchAllMemberships(ctx)
}

func (r *combinedSourceReader) FetchAllKeyContacts(ctx context.Context) ([]*model.KeyContact, error) {
	return r.keyContactRepo.FetchAllKeyContacts(ctx)
}
