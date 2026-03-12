// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/cmd/member-api/service"
	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/consumer"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/postgres"

	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"

	logging "github.com/linuxfoundation/lfx-v2-member-service/pkg/log"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/utils"

	"goa.design/clue/debug"
)

// Build-time variables set via ldflags
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

const (
	defaultPort             = "8080"
	gracefulShutdownSeconds = 25
)

func init() {
	logging.InitStructureLogConfig()
}

func main() {
	var (
		dbgF   = flag.Bool("d", false, "enable debug logging")
		port   = flag.String("p", defaultPort, "listen port")
		bind   = flag.String("bind", "*", "interface to bind on")
		reload = flag.Bool("reload", false, "destroy all b2b consumers and re-consume in dependency order, then resume normal operation")
	)
	flag.Usage = func() {
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()

	ctx := context.Background()

	// Set up JWT validator needed by the JWTAuth security handler.
	jwtAuthConfig := auth.JWTAuthConfig{
		JWKSURL:            os.Getenv("JWKS_URL"),
		Audience:           os.Getenv("AUDIENCE"),
		MockLocalPrincipal: os.Getenv("JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL"),
	}
	jwtAuth, err := auth.NewJWTAuth(jwtAuthConfig)
	if err != nil {
		slog.ErrorContext(ctx, "error setting up JWT authentication", "error", err)
		os.Exit(1)
	}

	// Set up OpenTelemetry SDK
	otelConfig := utils.OTelConfigFromEnv()
	if otelConfig.ServiceVersion == "" {
		otelConfig.ServiceVersion = Version
	}
	otelShutdown, err := utils.SetupOTelSDKWithConfig(ctx, otelConfig)
	if err != nil {
		slog.ErrorContext(ctx, "error setting up OpenTelemetry SDK", "error", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownSeconds*time.Second)
		defer cancel()
		if shutdownErr := otelShutdown(ctx); shutdownErr != nil {
			slog.ErrorContext(ctx, "error shutting down OpenTelemetry SDK", "error", shutdownErr)
		}
	}()

	slog.InfoContext(ctx, "Starting membership service",
		"bind", *bind,
		"http-port", *port,
		"graceful-shutdown-seconds", gracefulShutdownSeconds,
	)

	// Initialize the repositories based on configuration
	memberReader := service.MemberReaderImpl(ctx)
	defer service.CloseNATSClient()

	// Start the b2b KV consumer when the NATS repository source is active.
	// The consumer subscribes to salesforce_b2b-* keys in the v1-objects KV bucket
	// and publishes denormalized indexer messages for project_membership_tier,
	// project_membership, and key_contact resource types.
	natsClient := service.NATSClientInstance()
	if natsClient != nil {
		consumerCfg := consumer.Config{
			NATSConn: natsClient.Conn(),
		}

		// Wire up the optional PostgreSQL fallback for dependency resolvers.
		// When RDSDB is set, the consumer falls back to point-lookup queries
		// against salesforce_b2b when a dependency record is not yet in the
		// v1-objects KV bucket (expected during multi-hour Meltano backfills).
		if rdsDB := os.Getenv("RDSDB"); rdsDB != "" {
			db, dbErr := postgres.NewClient(rdsDB)
			if dbErr != nil {
				slog.ErrorContext(ctx, "failed to connect to PostgreSQL for b2b consumer fallback", "error", dbErr)
				os.Exit(1)
			}
			defer db.Close()
			consumerCfg.DB = db
			slog.InfoContext(ctx, "PostgreSQL fallback configured for b2b consumer dependency resolvers")
		}

		b2bConsumer, consumerErr := consumer.New(ctx, consumerCfg)
		if consumerErr != nil {
			slog.ErrorContext(ctx, "failed to initialize b2b KV consumer", "error", consumerErr)
			os.Exit(1)
		}

		// Use a dedicated cancellable context for the consumer so that reload can
		// restart it independently of the top-level application context.
		consumerCtx, consumerCancel := context.WithCancel(ctx)

		// When reload is requested, destroy all consumers and re-consume every
		// table in dependency order before switching to normal consumption.
		if *reload {
			slog.InfoContext(ctx, "reload flag set — re-consuming all b2b tables in dependency order")
			if reloadErr := b2bConsumer.Reload(consumerCtx); reloadErr != nil {
				slog.ErrorContext(ctx, "b2b KV consumer reload failed", "error", reloadErr)
				consumerCancel()
				os.Exit(1)
			}
		}

		var wgConsumer sync.WaitGroup
		wgConsumer.Add(1)
		go func() {
			defer wgConsumer.Done()
			if runErr := b2bConsumer.Run(consumerCtx); runErr != nil {
				slog.ErrorContext(ctx, "b2b KV consumer exited with error", "error", runErr)
			}
		}()
		defer func() {
			consumerCancel()
			b2bConsumer.Stop()
			wgConsumer.Wait()
		}()
	} else {
		slog.InfoContext(ctx, "NATS client not available (mock mode), b2b KV consumer not started")
	}

	// Initialize the service with use cases
	readMemberUseCase := usecaseSvc.NewMemberReaderOrchestrator(
		usecaseSvc.WithMemberReader(memberReader),
	)

	membershipServiceSvc := service.NewMembershipService(readMemberUseCase, memberReader, jwtAuth)

	// Wrap the services in endpoints
	membershipServiceEndpoints := membershipservice.NewEndpoints(membershipServiceSvc)
	if *dbgF {
		membershipServiceEndpoints.Use(debug.LogPayloads())
	}

	// Create channel for error handling
	errc := make(chan error, 1)

	// Setup interrupt handler
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)

	// Start the HTTP server
	addr := ":" + *port
	if *bind != "*" {
		addr = *bind + ":" + *port
	}

	handleHTTPServer(ctx, addr, membershipServiceEndpoints, &wg, errc, *dbgF)

	// Wait for signal
	slog.InfoContext(ctx, "received shutdown signal, stopping servers",
		"signal", <-errc,
	)

	cancel()

	// Create a timeout context for graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gracefulShutdownSeconds*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.InfoContext(ctx, "graceful shutdown completed")
	case <-shutdownCtx.Done():
		slog.WarnContext(ctx, "graceful shutdown timed out")
	}

	slog.InfoContext(ctx, "exited")
}
