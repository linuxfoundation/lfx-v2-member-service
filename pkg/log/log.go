// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package log

import (
	"context"
	"log"
	"log/slog"
	"os"

	slogotel "github.com/remychantenay/slog-otel"
)

type ctxKey string

const (
	slogFields      ctxKey = "slog_fields"
	logLevelDefault        = slog.LevelDebug

	debug = "debug"
	warn  = "warn"
	info  = "info"

	priorityCritical = "critical"
)

type contextHandler struct {
	slog.Handler
}

// Handle adds contextual attributes to the Record before calling the underlying handler
func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(slogFields).([]slog.Attr); ok {
		for _, v := range attrs {
			r.AddAttrs(v)
		}
	}

	return h.Handler.Handle(ctx, r)
}

// AppendCtx adds an slog attribute to the provided context so that it will be
// included in any Record created with such context
func AppendCtx(parent context.Context, attr slog.Attr) context.Context {
	if parent == nil {
		parent = context.Background()
	}

	if v, ok := parent.Value(slogFields).([]slog.Attr); ok {
		newAttrs := make([]slog.Attr, len(v)+1)
		copy(newAttrs, v)
		newAttrs[len(v)] = attr
		return context.WithValue(parent, slogFields, newAttrs)
	}

	return context.WithValue(parent, slogFields, []slog.Attr{attr})
}

// InitStructureLogConfig sets the structured log behavior
func InitStructureLogConfig() {

	logOptions := &slog.HandlerOptions{}
	var h slog.Handler

	configurations := map[string]func(){
		"options-logLevel": func() {
			logLevel := os.Getenv("LOG_LEVEL")
			slog.Info("log config",
				"logLevel", logLevel,
			)
			switch logLevel {
			case debug:
				logOptions.Level = slog.LevelDebug
			case warn:
				logOptions.Level = slog.LevelWarn
			case info:
				logOptions.Level = slog.LevelInfo
			default:
				logOptions.Level = logLevelDefault
			}
		},
		"options-addSource": func() {
			addSourceBool := false
			addSource := os.Getenv("LOG_ADD_SOURCE")
			if addSource == "true" || addSource == "false" {
				addSourceBool = addSource == "true"
			}
			slog.Info("log config",
				"LOG_ADD_SOURCE", addSourceBool,
			)
			logOptions.AddSource = addSourceBool
		},
	}

	for name, f := range configurations {
		slog.Info("setting logging configuration",
			"name", name,
		)
		f()
	}
	h = slog.NewJSONHandler(os.Stdout, logOptions)
	log.SetFlags(log.Llongfile)

	// Wrap with slog-otel handler to add trace_id and span_id from context
	otelHandler := slogotel.OtelHandler{Next: h}

	// Wrap with contextHandler to support context-based attributes
	logger := contextHandler{otelHandler}
	slog.SetDefault(slog.New(logger))
}

// Priority creates a slog.Attr for error priority classification
func Priority(level string) slog.Attr {
	return slog.String("priority", level)
}

// PriorityCritical creates a slog.Attr for critical errors
func PriorityCritical() slog.Attr {
	return Priority(priorityCritical)
}
