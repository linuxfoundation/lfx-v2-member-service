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
	"time"

	"go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

const (
	OTelProtocolGRPC = "grpc"
	OTelProtocolHTTP = "http"
	OTelExporterOTLP = "otlp"
	OTelExporterNone = "none"
)

// OTelConfig holds OpenTelemetry configuration options.
type OTelConfig struct {
	ServiceName       string
	ServiceVersion    string
	Protocol          string
	Endpoint          string
	Insecure          bool
	TracesExporter    string
	TracesSampleRatio float64
	MetricsExporter   string
	LogsExporter      string
	Propagators       string
}

// OTelConfigFromEnv creates an OTelConfig from environment variables.
func OTelConfigFromEnv() OTelConfig {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "lfx-v2-member-service"
	}

	serviceVersion := os.Getenv("OTEL_SERVICE_VERSION")

	protocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	if protocol == "" {
		protocol = OTelProtocolGRPC
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	insecure := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true"

	tracesExporter := os.Getenv("OTEL_TRACES_EXPORTER")
	if tracesExporter == "" {
		tracesExporter = OTelExporterNone
	}

	metricsExporter := os.Getenv("OTEL_METRICS_EXPORTER")
	if metricsExporter == "" {
		metricsExporter = OTelExporterNone
	}

	logsExporter := os.Getenv("OTEL_LOGS_EXPORTER")
	if logsExporter == "" {
		logsExporter = OTelExporterNone
	}

	propagators := os.Getenv("OTEL_PROPAGATORS")
	if propagators == "" {
		propagators = "tracecontext,baggage"
	}

	tracesSampleRatio := 1.0
	if ratio := os.Getenv("OTEL_TRACES_SAMPLE_RATIO"); ratio != "" {
		if parsed, err := strconv.ParseFloat(ratio, 64); err == nil {
			if parsed >= 0.0 && parsed <= 1.0 {
				tracesSampleRatio = parsed
			}
		}
	}

	return OTelConfig{
		ServiceName:       serviceName,
		ServiceVersion:    serviceVersion,
		Protocol:          protocol,
		Endpoint:          endpoint,
		Insecure:          insecure,
		TracesExporter:    tracesExporter,
		TracesSampleRatio: tracesSampleRatio,
		MetricsExporter:   metricsExporter,
		LogsExporter:      logsExporter,
		Propagators:       propagators,
	}
}

// SetupOTelSDK bootstraps the OpenTelemetry pipeline with OTLP exporters.
func SetupOTelSDK(ctx context.Context) (shutdown func(context.Context) error, err error) {
	return SetupOTelSDKWithConfig(ctx, OTelConfigFromEnv())
}

// SetupOTelSDKWithConfig bootstraps the OpenTelemetry pipeline with the provided configuration.
func SetupOTelSDKWithConfig(ctx context.Context, cfg OTelConfig) (shutdown func(context.Context) error, err error) {
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

	if cfg.Endpoint != "" {
		cfg.Endpoint = endpointURL(cfg.Endpoint, cfg.Insecure)
	}

	res, err := newResource(cfg)
	if err != nil {
		handleErr(err)
		return
	}

	prop := newPropagator(cfg)
	otel.SetTextMapPropagator(prop)

	if cfg.TracesExporter != OTelExporterNone {
		var tracerProvider *trace.TracerProvider
		tracerProvider, err = newTraceProvider(ctx, cfg, res)
		if err != nil {
			handleErr(err)
			return
		}
		shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
		otel.SetTracerProvider(tracerProvider)
	}

	if cfg.MetricsExporter != OTelExporterNone {
		var metricsProvider *metric.MeterProvider
		metricsProvider, err = newMetricsProvider(ctx, cfg, res)
		if err != nil {
			handleErr(err)
			return
		}
		shutdownFuncs = append(shutdownFuncs, metricsProvider.Shutdown)
		otel.SetMeterProvider(metricsProvider)
	}

	if cfg.LogsExporter != OTelExporterNone {
		var loggerProvider *log.LoggerProvider
		loggerProvider, err = newLoggerProvider(ctx, cfg, res)
		if err != nil {
			handleErr(err)
			return
		}
		shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
		global.SetLoggerProvider(loggerProvider)
	}

	return
}

func newResource(cfg OTelConfig) (*resource.Resource, error) {
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
}

func newPropagator(cfg OTelConfig) propagation.TextMapPropagator {
	var propagators []propagation.TextMapPropagator

	for _, p := range strings.Split(cfg.Propagators, ",") {
		switch strings.TrimSpace(p) {
		case "tracecontext":
			propagators = append(propagators, propagation.TraceContext{})
		case "baggage":
			propagators = append(propagators, propagation.Baggage{})
		case "jaeger":
			propagators = append(propagators, jaeger.Jaeger{})
		default:
			slog.Warn("unknown propagator, skipping", "propagator", p)
		}
	}

	if len(propagators) == 0 {
		propagators = []propagation.TextMapPropagator{
			propagation.TraceContext{},
			propagation.Baggage{},
		}
	}

	return propagation.NewCompositeTextMapPropagator(propagators...)
}

func endpointURL(raw string, insecure bool) string {
	if strings.Contains(raw, "://") {
		return raw
	}
	if insecure {
		return "http://" + raw
	}
	return "https://" + raw
}

func newTraceProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*trace.TracerProvider, error) {
	var exporter trace.SpanExporter
	var err error

	if cfg.Protocol == OTelProtocolHTTP {
		var opts []otlptracehttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	} else {
		var opts []otlptracegrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, err
	}

	return trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithSampler(trace.TraceIDRatioBased(cfg.TracesSampleRatio)),
		trace.WithBatcher(exporter, trace.WithBatchTimeout(time.Second)),
	), nil
}

func newMetricsProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*metric.MeterProvider, error) {
	var exporter metric.Exporter
	var err error

	if cfg.Protocol == OTelProtocolHTTP {
		var opts []otlpmetrichttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exporter, err = otlpmetrichttp.New(ctx, opts...)
	} else {
		var opts []otlpmetricgrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exporter, err = otlpmetricgrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, err
	}

	return metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter, metric.WithInterval(30*time.Second))),
	), nil
}

func newLoggerProvider(ctx context.Context, cfg OTelConfig, res *resource.Resource) (*log.LoggerProvider, error) {
	var exporter log.Exporter
	var err error

	if cfg.Protocol == OTelProtocolHTTP {
		var opts []otlploghttp.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlploghttp.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		exporter, err = otlploghttp.New(ctx, opts...)
	} else {
		var opts []otlploggrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlploggrpc.WithEndpointURL(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		exporter, err = otlploggrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, err
	}

	return log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter)),
	), nil
}
