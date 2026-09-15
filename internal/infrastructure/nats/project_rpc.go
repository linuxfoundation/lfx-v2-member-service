// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats provides NATS JetStream KV-backed implementations of the domain
// storage ports.
package nats

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// projectServiceErrorCode returns the error code from a project-service error
// envelope ({"error":"not_found",...} or {"error":"internal",...}), or "" if
// data is a normal success payload.
func projectServiceErrorCode(data []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &env) != nil {
		return ""
	}
	return env.Error
}

// Project-service NATS RPC subjects.
const (
	// projectGetSlugSubject is the NATS request/reply subject for resolving a
	// v2 project UID to its slug via the project-service.
	projectGetSlugSubject = "lfx.projects-api.get_slug"

	// projectSlugToUIDSubject is the NATS request/reply subject for resolving
	// a project slug to its v2 UID via the project-service.
	projectSlugToUIDSubject = "lfx.projects-api.slug_to_uid"
)

// ProjectRPC provides NATS request/reply calls to the project-service.
type ProjectRPC struct {
	conn    *nats.Conn
	timeout time.Duration
}

// NewProjectRPC creates a new ProjectRPC using the given NATS connection and
// request timeout.
func NewProjectRPC(conn *nats.Conn, timeout time.Duration) *ProjectRPC {
	return &ProjectRPC{
		conn:    conn,
		timeout: timeout,
	}
}

// GetSlug resolves a v2 project UID to its slug via the project-service NATS
// RPC (lfx.projects-api.get_slug). Returns NotFound only when the project
// definitively does not exist (project-service replied with code "not_found").
// A transport failure or an internal error from project-service is returned as
// an Unexpected error so callers can distinguish a confirmed absence from a
// transient or service-side failure.
func (r *ProjectRPC) GetSlug(ctx context.Context, projectUID string) (string, error) {
	return r.request(ctx, projectGetSlugSubject, projectUID)
}

// SlugToUID resolves a project slug to its v2 UID via the project-service NATS
// RPC (lfx.projects-api.slug_to_uid). Returns NotFound only when the slug
// definitively does not exist (project-service replied with code "not_found").
// A transport failure or an internal error from project-service is returned as
// an Unexpected error so callers can distinguish a confirmed absence from a
// transient or service-side failure.
func (r *ProjectRPC) SlugToUID(ctx context.Context, slug string) (string, error) {
	return r.request(ctx, projectSlugToUIDSubject, slug)
}

// request sends a raw UTF-8 payload to the given NATS subject and returns the
// raw UTF-8 response body.
//
// Error classification:
//   - NATS transport error (timeout, no responder, etc.) → Unexpected; the
//     caller cannot tell whether the resource exists.
//   - nil reply body → Unexpected; an absent reply is ambiguous, not a
//     confirmed absence.
//   - project-service {"error":"not_found",...} → NotFound.
//   - project-service {"error":"<any other code>",...} → Unexpected.
//   - plain success payload → (value, nil).
func (r *ProjectRPC) request(ctx context.Context, subject, payload string) (string, error) {
	// If the context already carries a deadline, honour it directly; otherwise
	// apply the configured timeout so the call never hangs indefinitely.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	msg := &nats.Msg{
		Subject: subject,
		Data:    []byte(payload),
	}

	reply, err := requestMsgWithSpan(ctx, r.conn, msg)
	if err != nil {
		// Transport failure: cannot confirm whether the resource exists.
		return "", errs.NewUnexpected("project-service RPC request failed", err)
	}

	if reply == nil {
		// Nil reply is ambiguous — not a confirmed absence.
		return "", errs.NewUnexpected("nil reply from project-service RPC", nil)
	}

	return parseProjectRPCReply(reply.Data)
}

// parseProjectRPCReply decodes a raw project-service RPC reply body.
// A JSON error envelope ({"error":"<code>",...}) is mapped to a typed error.
// A plain success payload (UUID string, slug, etc.) is returned as-is.
func parseProjectRPCReply(data []byte) (string, error) {
	if code := projectServiceErrorCode(data); code != "" {
		if code == "not_found" {
			return "", errs.NewNotFound("project not found", nil)
		}
		return "", errs.NewUnexpected("project-service error: "+code, nil)
	}
	return strings.TrimSpace(string(data)), nil
}
