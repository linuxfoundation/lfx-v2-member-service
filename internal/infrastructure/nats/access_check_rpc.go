// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/nats-io/nats.go"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
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

	b2bOrgObjectType    = "b2b_org"
	b2bOrgChildRelation = "child"
)

// maxReadTuplesResults caps the raw tuple count accepted from a fga-sync
// read_tuples reply, ahead of the caller's post-dedup MaxMemberTierCandidates
// check. Bounded today only by the 1 MB NATS max_payload; this is
// defense-in-depth against a misbehaving trusted peer, not a real risk under
// the current trust boundary.
const maxReadTuplesResults = 1000

var errReadTuplesCapExceeded = errs.NewServiceUnavailable("fga-sync read_tuples reply exceeded result cap")

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
	data, err := r.readTuples(ctx, userTuplePrefix+username, membershipObjectType)
	if err != nil {
		return nil, err
	}
	return parseReadTuplesResponse(username, data)
}

// ParentsListingChild implements port.B2BOrgChildLinkReader by reading the
// b2b_org tuples whose user is the child org itself via the fga-sync
// read_tuples RPC. fga-sync pages through every result, so a successful reply
// is complete; anything inconclusive is ServiceUnavailable.
func (r *AccessCheckRPC) ParentsListingChild(ctx context.Context, childUID string) ([]string, error) {
	// CDC handlers carry a deadline for the whole event, which readTuples
	// would otherwise honour in place of the timeout; a stalled fga-sync must
	// not consume the time the rest of the batch needs.
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	data, err := r.readTuples(ctx, b2bOrgObjectType+":"+childUID, b2bOrgObjectType)
	if err != nil {
		return nil, err
	}
	return parseChildLinkResponse(childUID, data)
}

// readTuples sends one read_tuples request and returns the raw reply body.
func (r *AccessCheckRPC) readTuples(ctx context.Context, user, objectType string) ([]byte, error) {
	payload, err := json.Marshal(fgatypes.ReadTuplesRequest{
		User:       user,
		ObjectType: objectType,
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
	if reply == nil {
		return nil, nil
	}
	return reply.Data, nil
}

// parseReadTuplesResponse parses the fga-sync read_tuples reply body. The
// reverse index gates whose membership records get assembled, so every
// inconclusive reply (absent/empty body, malformed JSON, or an error reported
// by fga-sync, even one accompanied by partial results) is ServiceUnavailable.
func parseReadTuplesResponse(username string, data []byte) ([]string, error) {
	results, err := readTuplesResults(data)
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(results))
	for _, tuple := range results {
		if uid, ok := membershipUIDFromTuple(tuple, username); ok {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// parseChildLinkResponse returns the deduplicated UIDs of the b2b_orgs that
// list childUID as a child. Other tuples in the reply, such as the parent
// tuples the child's own children hold on it, are skipped.
func parseChildLinkResponse(childUID string, data []byte) ([]string, error) {
	results, err := readTuplesResults(data)
	if errors.Is(err, errReadTuplesCapExceeded) {
		return nil, errors.Join(port.ErrChildLinksUncheckable, err)
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	parents := make([]string, 0, len(results))
	for _, tuple := range results {
		parent, ok := parentUIDFromChildTuple(tuple, childUID)
		if !ok {
			continue
		}
		if _, dup := seen[parent]; dup {
			continue
		}
		seen[parent] = struct{}{}
		parents = append(parents, parent)
	}
	return parents, nil
}

// readTuplesResults validates a read_tuples reply envelope and returns its
// tuple strings. A capped reply may be missing tuples, so it is an error.
func readTuplesResults(data []byte) ([]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errs.NewServiceUnavailable("empty reply from fga-sync read_tuples RPC")
	}

	var resp fgatypes.ReadTuplesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, errs.NewServiceUnavailable("unmarshalling read_tuples response", err)
	}
	if resp.Error != "" {
		// The upstream text goes into the wrapped cause, which reaches server
		// logs only; the response body carries just the stable message.
		return nil, errs.NewServiceUnavailable("fga-sync read_tuples reported an error", errors.New(resp.Error))
	}
	// fga-sync always sends results (empty array for no matches), so an absent
	// or null field is a malformed reply, not "no memberships".
	if resp.Results == nil {
		return nil, errs.NewServiceUnavailable("fga-sync read_tuples reply omitted results")
	}
	if len(resp.Results) > maxReadTuplesResults {
		return nil, errReadTuplesCapExceeded
	}
	return resp.Results, nil
}

// parentUIDFromChildTuple returns P for a tuple "b2b_org:P#child@b2b_org:<childUID>".
func parentUIDFromChildTuple(tuple, childUID string) (string, bool) {
	objectRelation, user, ok := strings.Cut(tuple, "@")
	if !ok || user != b2bOrgObjectType+":"+childUID {
		return "", false
	}
	object, relation, ok := strings.Cut(objectRelation, "#")
	if !ok || relation != b2bOrgChildRelation {
		return "", false
	}
	uid, ok := strings.CutPrefix(object, b2bOrgObjectType+":")
	if !ok || uid == "" {
		return "", false
	}
	return uid, true
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
