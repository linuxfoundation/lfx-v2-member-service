// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/nats-io/nats.go"

	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// Constituents of the FGA tuple strings returned by the fga-sync read_tuples
// RPC for project-membership key contacts, in the canonical
// object#relation@user format (e.g.
// "project_membership:02i...#key_contact@user:jdoe"). key_contact is the only
// directly-assigned user relation on project_membership in the FGA model, so
// the parser accepts nothing else.
const (
	membershipObjectType = "project_membership"
	keyContactRelation   = "key_contact"
	userTuplePrefix      = "user:"
)

// AccessCheckRPC provides NATS request/reply calls to the fga-sync service's
// access-check API.
type AccessCheckRPC struct {
	conn    *nats.Conn
	timeout time.Duration
}

// NewAccessCheckRPC creates a new AccessCheckRPC using the given NATS
// connection and request timeout.
func NewAccessCheckRPC(conn *nats.Conn, timeout time.Duration) *AccessCheckRPC {
	return &AccessCheckRPC{
		conn:    conn,
		timeout: timeout,
	}
}

// MembershipUIDsForUser implements port.UserMembershipReader by reading the
// user's direct project_membership tuples from OpenFGA via the fga-sync NATS
// RPC (lfx.access_check.read_tuples). The tuples are a reverse index only:
// callers must verify the returned candidate UIDs against the membership
// records themselves. An unknown user yields an empty slice; any RPC or
// decode failure is a ServiceUnavailable so callers fail closed.
func (r *AccessCheckRPC) MembershipUIDsForUser(ctx context.Context, username string) ([]string, error) {
	payload, err := json.Marshal(fgatypes.ReadTuplesRequest{
		User:       userTuplePrefix + username,
		ObjectType: membershipObjectType,
	})
	if err != nil {
		return nil, errs.NewUnexpected("marshalling read_tuples request", err)
	}

	// If the context already carries a deadline, honour it directly; otherwise
	// apply the configured timeout so the call never hangs indefinitely.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	msg := &nats.Msg{
		Subject: fgaconstants.ReadTuplesSubject,
		Data:    payload,
	}

	// Unlike this file's other fga-sync calls (fire-and-forget publishes),
	// this blocks for a reply: fga-sync must be live and responsive, and a
	// timeout or no-responder failure fails closed as ServiceUnavailable.
	reply, err := requestMsgWithSpan(ctx, r.conn, msg)
	if err != nil {
		return nil, errs.NewServiceUnavailable("fga-sync read_tuples RPC failed", err)
	}
	var data []byte
	if reply != nil {
		data = reply.Data
	}
	return parseReadTuplesResponse(username, data)
}

// parseReadTuplesResponse parses the fga-sync read_tuples reply body. The
// reverse index gates whose membership records get assembled, so every
// inconclusive reply (absent/empty body, malformed JSON, or an error reported
// by fga-sync, even one accompanied by partial results) is ServiceUnavailable
func parseReadTuplesResponse(username string, data []byte) ([]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errs.NewServiceUnavailable("empty reply from fga-sync read_tuples RPC")
	}

	var resp fgatypes.ReadTuplesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, errs.NewServiceUnavailable("unmarshalling read_tuples response", err)
	}
	if resp.Error != "" {
		return nil, errs.NewServiceUnavailable(fmt.Sprintf("fga-sync read_tuples reported an error: %s", resp.Error))
	}
	// fga-sync always sends results (empty array for no matches), so an absent
	// or null field is a malformed reply, not "no memberships".
	if resp.Results == nil {
		return nil, errs.NewServiceUnavailable("fga-sync read_tuples reply omitted results")
	}

	uids := make([]string, 0, len(resp.Results))
	for _, tuple := range resp.Results {
		if uid, ok := membershipUIDFromTuple(tuple, username); ok {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// membershipUIDFromTuple parses a canonical object#relation@user tuple string
// and returns the membership UID when the tuple is a key_contact grant on a
// project_membership for the given username. Any other tuple shape is
// silently skipped.
func membershipUIDFromTuple(tuple, username string) (string, bool) {
	objectRelation, user, ok := strings.Cut(tuple, "@")
	if !ok || user != userTuplePrefix+username {
		return "", false
	}
	object, relation, ok := strings.Cut(objectRelation, "#")
	if !ok || relation != keyContactRelation {
		return "", false
	}
	uid, ok := strings.CutPrefix(object, membershipObjectType+":")
	if !ok || uid == "" {
		return "", false
	}
	return uid, true
}
