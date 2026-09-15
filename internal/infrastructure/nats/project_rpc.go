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
// RPC (lfx.projects-api.get_slug). Returns NotFound if the project does not
// exist or the RPC times out.
func (r *ProjectRPC) GetSlug(ctx context.Context, projectUID string) (string, error) {
	reply, err := r.request(ctx, projectGetSlugSubject, projectUID)
	if err != nil {
		return "", errs.NewNotFound("project not found", err)
	}
	return reply, nil
}

// SlugToUID resolves a project slug to its v2 UID via the project-service NATS
// RPC (lfx.projects-api.slug_to_uid). Returns NotFound if the slug does not
// exist or the RPC times out.
func (r *ProjectRPC) SlugToUID(ctx context.Context, slug string) (string, error) {
	reply, err := r.request(ctx, projectSlugToUIDSubject, slug)
	if err != nil {
		return "", errs.NewNotFound("project not found", err)
	}
	return reply, nil
}

// request sends a raw UTF-8 payload to the given NATS subject and returns the
// raw UTF-8 response body. A NATS transport error or a nil reply is surfaced
// directly. A JSON error envelope from project-service
// ({"error":"not_found",...} or {"error":"internal",...}) is returned as a
// NotFound error; plain UUID/string success replies are returned as-is.
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
		return "", err
	}

	if reply == nil {
		return "", errs.NewNotFound("nil reply from project-service RPC", nil)
	}

	// Project-service returns {"error":"<code>",...} on errors; any other response
	// (UUID string, empty body) is a success value.
	if code := projectServiceErrorCode(reply.Data); code != "" {
		if code == "not_found" {
			return "", errs.NewNotFound("project not found", nil)
		}
		return "", errs.NewUnexpected("project-service error: "+code, nil)
	}

	return strings.TrimSpace(string(reply.Data)), nil
}
