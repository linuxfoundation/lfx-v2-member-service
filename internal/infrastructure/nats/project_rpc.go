// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats provides NATS JetStream KV-backed implementations of the domain
// storage ports.
package nats

import (
	"context"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

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
// raw UTF-8 response body. A NATS error, a nil reply, or an empty reply body
// are all treated as not-found conditions; the caller wraps the returned error
// appropriately. The context deadline is honoured via RequestMsgWithContext; if
// the context has no deadline, r.timeout is used instead.
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

	if reply == nil || len(reply.Data) == 0 {
		return "", errs.NewNotFound("empty reply from project-service RPC", nil)
	}

	body := strings.TrimSpace(string(reply.Data))
	if body == "" {
		return "", errs.NewNotFound("empty reply body from project-service RPC", nil)
	}

	return body, nil
}
