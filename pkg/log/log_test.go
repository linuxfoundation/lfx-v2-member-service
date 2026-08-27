// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// InitStructureLogConfig must install the JSON stdout handler before emitting
// any startup log, otherwise Datadog reads the plain-text stderr default as
// status:error.
func TestInitStructureLogConfigWritesNothingToStderr(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("LOG_ADD_SOURCE", "true")

	prevLogger := slog.Default()
	prevStderr := os.Stderr
	prevStdout := os.Stdout
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		os.Stderr = prevStderr
		os.Stdout = prevStdout
	})

	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stderr pipe: %v", err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}
	os.Stderr = errWrite
	os.Stdout = outWrite

	InitStructureLogConfig()

	_ = errWrite.Close()
	_ = outWrite.Close()

	stderrOutput, err := io.ReadAll(errRead)
	if err != nil {
		t.Fatalf("failed to read stderr: %v", err)
	}
	stdoutOutput, err := io.ReadAll(outRead)
	if err != nil {
		t.Fatalf("failed to read stdout: %v", err)
	}

	if len(stderrOutput) != 0 {
		t.Errorf("expected no stderr output, got %q", stderrOutput)
	}
	if len(stdoutOutput) == 0 {
		t.Error("expected the logging configuration record on stdout")
	}
}

// otelhttp extracts client-supplied baggage into the request context (the
// OTEL_PROPAGATORS default includes it), so baggage enrichment must stay off:
// a caller could otherwise inject PII or shadow trusted fields.
func TestInitStructureLogConfigDropsContextBaggage(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")

	prevLogger := slog.Default()
	prevStdout := os.Stdout
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		os.Stdout = prevStdout
	})

	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("failed to create temp stdout: %v", err)
	}
	os.Stdout = out

	InitStructureLogConfig()

	member, err := baggage.NewMember("leaked_pii", "victim@example.com")
	if err != nil {
		t.Fatalf("failed to build baggage member: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("failed to build baggage: %v", err)
	}

	// slog-otel only copies baggage when a recording span is present, which
	// is always the case for records emitted under otelhttp.
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(baggage.ContextWithBaggage(context.Background(), bag), "probe")
	defer span.End()
	if !span.IsRecording() {
		t.Fatal("test span must be recording, otherwise baggage is never copied")
	}

	slog.InfoContext(ctx, "probe")

	if err := out.Close(); err != nil {
		t.Fatalf("failed to close temp stdout: %v", err)
	}
	logged, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatalf("failed to read temp stdout: %v", err)
	}

	if !strings.Contains(string(logged), `"msg":"probe"`) {
		t.Fatalf("probe record missing from stdout: %s", logged)
	}
	for _, leak := range []string{"leaked_pii", "victim@example.com"} {
		if strings.Contains(string(logged), leak) {
			t.Errorf("baggage %q leaked into the log record: %s", leak, logged)
		}
	}
}
