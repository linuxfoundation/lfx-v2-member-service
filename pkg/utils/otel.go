// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utils

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"

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

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// Exporters are configured via OTEL_TRACES_EXPORTER, OTEL_METRICS_EXPORTER, and
// OTEL_LOGS_EXPORTER environment variables (autoexport library default: "otlp";
// Helm chart default: "none").
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

	sampler := trace.ParentBased(trace.AlwaysSample())
	if ratio := os.Getenv("OTEL_TRACES_SAMPLE_RATIO"); ratio != "" {
		if parsed, parseErr := strconv.ParseFloat(ratio, 64); parseErr == nil {
			if parsed >= 0.0 && parsed <= 1.0 {
				sampler = trace.ParentBased(trace.TraceIDRatioBased(parsed))
			} else {
				slog.Warn("OTEL_TRACES_SAMPLE_RATIO out of range [0,1], using 1.0", "value", ratio)
			}
		} else {
			slog.Warn("invalid OTEL_TRACES_SAMPLE_RATIO, using 1.0", "value", ratio)
		}
	}

	spanExporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		handleErr(err)
		return
	}
	tracerProvider := trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithSampler(sampler),
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
