// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utils

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// samplerFromEnv creates a trace.Sampler from OTEL_TRACES_SAMPLER and
// OTEL_TRACES_SAMPLER_ARG environment variables.
// Defaults to parentbased_traceidratio with ratio 1.0 when unset.
func samplerFromEnv() trace.Sampler {
	sampler := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))

	parseRatio := func() float64 {
		if arg == "" {
			return 1.0
		}
		r, err := strconv.ParseFloat(arg, 64)
		if err != nil || r < 0.0 || r > 1.0 {
			slog.Warn("invalid OTEL_TRACES_SAMPLER_ARG, defaulting to 1.0", "value", arg)
			return 1.0
		}
		return r
	}

	switch sampler {
	case "always_on":
		return trace.AlwaysSample()
	case "always_off":
		return trace.NeverSample()
	case "traceidratio":
		return trace.TraceIDRatioBased(parseRatio())
	case "parentbased_always_on":
		return trace.ParentBased(trace.AlwaysSample())
	case "parentbased_always_off":
		return trace.ParentBased(trace.NeverSample())
	case "parentbased_traceidratio":
		return trace.ParentBased(trace.TraceIDRatioBased(parseRatio()))
	default:
		if sampler != "" {
			slog.Warn("unknown OTEL_TRACES_SAMPLER, falling back to parentbased_traceidratio", "value", sampler)
		}
		return trace.ParentBased(trace.TraceIDRatioBased(parseRatio()))
	}
}

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// Exporters are configured via OTEL_TRACES_EXPORTER, OTEL_METRICS_EXPORTER, and
// OTEL_LOGS_EXPORTER environment variables (autoexport library default: "otlp";
// Helm chart default: "none").
// Sampler is configured via OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG.
// Propagators are configured via OTEL_PROPAGATORS (default: "tracecontext,baggage").
func SetupOTelSDK(ctx context.Context) (shutdown func(context.Context) error, err error) {
	var shutdownFuncs []func(context.Context) error

	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = constants.ServiceName
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(os.Getenv("OTEL_SERVICE_VERSION")),
		),
	)
	if err != nil {
		handleErr(err)
		return
	}

	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator())

	spanExporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		handleErr(err)
		return
	}
	tracerProvider := trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithSampler(samplerFromEnv()),
		trace.WithBatcher(spanExporter),
	)
	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	metricReader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		handleErr(err)
		return
	}
	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metricReader),
	)
	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	logExporter, err := autoexport.NewLogExporter(ctx)
	if err != nil {
		handleErr(err)
		return
	}
	loggerProvider := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(logExporter)),
	)
	shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	global.SetLoggerProvider(loggerProvider)

	return
}
