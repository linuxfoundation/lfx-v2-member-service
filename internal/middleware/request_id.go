// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/log"

	"github.com/google/uuid"
)

// RequestIDMiddleware creates a middleware that adds a request ID to the context
func RequestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(string(constants.RequestIDHeader))

			if requestID == "" {
				requestID = uuid.New().String()
			}

			w.Header().Set(string(constants.RequestIDHeader), requestID)

			ctx := context.WithValue(r.Context(), constants.RequestIDHeader, requestID)
			ctx = log.AppendCtx(ctx, slog.String(string(constants.RequestIDHeader), requestID))

			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)
		})
	}
}
