// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package log

import (
	"io"
	"log/slog"
	"os"
	"testing"
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
