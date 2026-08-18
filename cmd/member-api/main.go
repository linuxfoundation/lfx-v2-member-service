// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/cmd/member-api/service"
	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce"

	logging "github.com/linuxfoundation/lfx-v2-member-service/pkg/log"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/utils"

	clueDebug "goa.design/clue/debug"
)

// Build-time variables set via ldflags.
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
		dbgF = flag.Bool("d", false, "enable debug logging")
		port = flag.String("p", defaultPort, "listen port")
		bind = flag.String("bind", "*", "interface to bind on")
	)
	flag.Usage = func() {
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()

	ctx := context.Background()

	// Set up OpenTelemetry SDK.
	if os.Getenv("OTEL_SERVICE_VERSION") == "" {
		_ = os.Setenv("OTEL_SERVICE_VERSION", Version)
	}
	otelShutdown, err := utils.SetupOTelSDK(ctx)
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

	// Register Salesforce API usage as OTEL observable gauges. This is a no-op
	// if OTEL_METRICS_EXPORTER is unset (the meter provider is a no-op).
	if err := salesforce.RegisterOTelMetrics(); err != nil {
		slog.WarnContext(ctx, "failed to register Salesforce OTEL metrics", "error", err)
	}

	defer service.CloseNATSClient()

	// RUN_MODE selects between the HTTP API server (default) and the CDC
	// consumer. A single binary runs both roles; the Kubernetes Deployment
	// for the consumer sets RUN_MODE=consumer with replicas:1 + Recreate
	// strategy so only one replica processes CDC events at any time.
	runMode := os.Getenv("RUN_MODE")
	if runMode == "consumer" {
		runConsumer(ctx)
		return
	}
	if runMode == "avatar-backfill" {
		runAvatarBackfill(ctx)
		return
	}

	runAPI(ctx, *bind, *port, *dbgF)
}

// runAvatarBackfill runs the one-off b2b_org_settings avatar backfill (RUN_MODE=avatar-backfill) then exits.
func runAvatarBackfill(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go cancelOnSignal(ctx, cancel)

	if err := service.RunAvatarBackfill(ctx); err != nil {
		slog.ErrorContext(ctx, "avatar backfill failed", "error", err)
		os.Exit(1)
	}
	slog.InfoContext(ctx, "avatar backfill complete")
}

// runAPI starts the HTTP membership API server. This is the default run mode.
func runAPI(ctx context.Context, bind, port string, debug bool) {
	slog.InfoContext(ctx, "Starting membership service (api mode)",
		"bind", bind,
		"http-port", port,
		"graceful-shutdown-seconds", gracefulShutdownSeconds,
	)

	// Register all NATS subscriptions via the provider. The provider handles
	// mock-mode skipping (project-id-map) and queue-group wiring centrally.
	if err := service.QueueSubscriptions(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to start NATS subscriptions", "error", err)
		os.Exit(1)
	}
	defer service.DrainAPISubscriptions(ctx)

	membershipServiceSvc := service.NewMembershipService(
		service.JWTAuthImpl(ctx),
		service.MemberReaderImpl(ctx),
		service.B2BOrgReaderImpl(ctx),
		service.ProjectMembershipReaderImpl(ctx),
		service.B2BOrgSettingsReaderImpl(ctx),
		service.B2BOrgWriterUseCase(ctx),
		service.LogoUploaderUseCase(ctx),
		service.KeyContactWriterUseCase(ctx),
		service.OrgSettingsWriterUseCase(ctx),
		service.WorkspaceWriterUseCase(ctx),
		service.BackfillRunnerImpl(ctx),
	)

	// A transient logo-bucket outage only degrades the logo-upload feature; it
	// must not take down the rest of the API, which /readyz already treats as
	// feature-local, and it must not delay the HTTP server binding either --
	// EnsureBucket's own retry loop can block for ~30s, long enough on its own
	// to trip the chart's startup probe (failureThreshold 30 x periodSeconds 1)
	// before the server ever starts listening (LFXV2-2016 lfx-reviewer/Copilot
	// finding on PR #87). Run it in the background instead of gating startup.
	go func() {
		if err := service.EnsureObjectStoreReady(ctx); err != nil {
			slog.WarnContext(ctx, "logo object store unreachable at startup; logo upload degraded, rest of service unaffected", "error", err)
		}
	}()

	// Wrap the services in endpoints.
	membershipServiceEndpoints := membershipservice.NewEndpoints(membershipServiceSvc)
	if debug {
		membershipServiceEndpoints.Use(clueDebug.LogPayloads())
	}

	// Create channel for error handling.
	errc := make(chan error, 1)
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)

	addr := ":" + port
	if bind != "*" {
		addr = bind + ":" + port
	}

	handleHTTPServer(ctx, addr, membershipServiceEndpoints, &wg, errc, debug)

	slog.InfoContext(ctx, "received shutdown signal, stopping servers",
		"signal", <-errc,
	)

	cancel()

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

// cancelOnSignal cancels ctx when SIGINT or SIGTERM is received. Intended to
// run in a goroutine; returns after the first signal.
func cancelOnSignal(ctx context.Context, cancel context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-c:
		slog.InfoContext(ctx, "received shutdown signal", "signal", sig.String())
		cancel()
	case <-ctx.Done():
	}
}

// runConsumer starts the CDC event consumer. It blocks until a signal is
// received, then cancels the subscription and waits up to gracefulShutdownSeconds
// for the Run loop to finish committing its last replay cursor before exiting.
//
// GET /livez on :8080 (same port + path as the API Deployment) serves as the
// K8s liveness probe. Always returns 200 while the process is alive; the probe
// is not used to signal shutdown (see handler comment for rationale).
// Recreate + replicas:1 in the Deployment ensures at most one active consumer.
func runConsumer(ctx context.Context) {
	slog.InfoContext(ctx, "Starting membership service (consumer mode)",
		"graceful-shutdown-seconds", gracefulShutdownSeconds,
	)

	consumer, replayStore, pubsubClient := service.CDCConsumerImpl(ctx)
	defer func() {
		if err := pubsubClient.Close(); err != nil {
			slog.WarnContext(ctx, "CDC Pub/Sub gRPC client close failed", "error", err)
		}
	}()
	channel := service.CDCChannelFromEnv()
	slog.InfoContext(ctx, "CDC consumer initialised", "channel", channel)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go cancelOnSignal(ctx, cancel)

	healthServer := &http.Server{
		Addr:        ":" + defaultPort,
		ReadTimeout: 5 * time.Second,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		// Always 200 while the process is alive. Kubernetes handles graceful
		// shutdown via SIGTERM + terminationGracePeriodSeconds; checking ctx here
		// would return 503 immediately after SIGTERM and can cause the liveness
		// probe to restart the pod before the replay cursor is committed.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthServer.Handler = mux

	var healthWg sync.WaitGroup
	healthWg.Add(1)
	go func() {
		defer healthWg.Done()
		slog.InfoContext(ctx, "CDC consumer health server listening", "port", defaultPort)
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.ErrorContext(ctx, "CDC consumer health server failed, stopping consumer", "error", err)
			cancel()
		}
	}()

	// Run the consumer loop; it returns when ctx is cancelled or on error.
	// defer cancel() ensures that if Run exits early (e.g. unrecoverable gRPC
	// stream failure) the <-ctx.Done() gate below unblocks so the process exits
	// and Kubernetes restarts the pod.
	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		defer cancel()
		if err := consumer.Run(ctx, channel, replayStore); err != nil {
			slog.InfoContext(ctx, "CDC consumer stopped", "reason", err)
		}
	}()

	// Block here until a SIGINT/SIGTERM cancels ctx (via cancelOnSignal).
	// Only then start the bounded graceful-shutdown window so a long-running
	// consumer is not evicted 25 s after launch.
	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gracefulShutdownSeconds*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		runWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.InfoContext(ctx, "CDC consumer Run loop exited cleanly")
	case <-shutdownCtx.Done():
		slog.WarnContext(ctx, "CDC consumer graceful shutdown timed out")
	}

	// Stop the health server.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := healthServer.Shutdown(stopCtx); err != nil {
		slog.WarnContext(ctx, "CDC consumer health server shutdown error", "error", err)
	}
	healthWg.Wait()

	slog.InfoContext(ctx, "CDC consumer exited")
}
