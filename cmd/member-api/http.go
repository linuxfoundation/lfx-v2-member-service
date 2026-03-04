// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	membershipservicesvr "github.com/linuxfoundation/lfx-v2-member-service/gen/http/membership_service/server"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/middleware"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"goa.design/clue/debug"
	goahttp "goa.design/goa/v3/http"
)

// handleHTTPServer starts configures and starts a HTTP server on the given URL.
func handleHTTPServer(ctx context.Context, host string, membershipServiceEndpoints *membershipservice.Endpoints, wg *sync.WaitGroup, errc chan error, dbg bool) {

	var (
		dec = goahttp.RequestDecoder
		enc = goahttp.ResponseEncoder
	)

	var mux goahttp.Muxer
	{
		mux = goahttp.NewMuxer()
		if dbg {
			debug.MountPprofHandlers(debug.Adapt(mux))
			debug.MountDebugLogEnabler(debug.Adapt(mux))
		}
	}

	koDataPath := os.Getenv("KO_DATA_PATH")
	if koDataPath == "" {
		koDataPath = "../../gen/http/"
	}

	koDataDir := http.Dir(koDataPath)

	var (
		membershipServiceServer *membershipservicesvr.Server
	)
	{
		eh := errorHandler(ctx)
		membershipServiceServer = membershipservicesvr.New(membershipServiceEndpoints, mux, dec, enc, eh, nil, koDataDir, koDataDir, koDataDir, koDataDir)
	}

	// Configure the mux
	membershipservicesvr.Mount(mux, membershipServiceServer)

	var handler http.Handler = mux

	// Add RequestID middleware first
	handler = middleware.RequestIDMiddleware()(handler)
	// Add Authorization middleware
	handler = middleware.AuthorizationMiddleware()(handler)
	if dbg {
		handler = debug.HTTP()(handler)
	}
	// Wrap the handler with OpenTelemetry instrumentation
	handler = otelhttp.NewHandler(handler, "membership-service")

	srv := &http.Server{Addr: host, Handler: handler, ReadHeaderTimeout: time.Second * 60}
	for _, m := range membershipServiceServer.Mounts {
		slog.InfoContext(ctx, "HTTP endpoint mounted",
			"method", m.Method,
			"verb", m.Verb,
			"pattern", m.Pattern,
		)
	}

	(*wg).Add(1)
	go func() {
		defer (*wg).Done()

		go func() {
			slog.InfoContext(ctx, "HTTP server listening", "host", host)
			errc <- srv.ListenAndServe()
		}()

		<-ctx.Done()
		slog.InfoContext(ctx, "shutting down HTTP server", "host", host)

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(gracefulShutdownSeconds-5)*time.Second)
		defer cancel()

		err := srv.Shutdown(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "failed to shutdown HTTP server", "error", err)
		}
	}()
}

// errorHandler returns a function that writes and logs the given error.
func errorHandler(logCtx context.Context) func(context.Context, http.ResponseWriter, error) {
	return func(ctx context.Context, w http.ResponseWriter, err error) {
		slog.ErrorContext(logCtx, "HTTP error occurred", "error", err)
	}
}
