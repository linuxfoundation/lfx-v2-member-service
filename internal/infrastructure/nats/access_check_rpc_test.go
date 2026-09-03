// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"testing"
	"time"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transport failure (no reply received at all) must fail closed as
// ServiceUnavailable: the reverse index gates whose membership records get
// assembled, so an fga-sync outage must never read as "this user has no
// memberships" (an empty slice) or as a definitive miss.
func TestAccessCheckRPC_MembershipUIDsForUser_TransportFailureIsServiceUnavailable(t *testing.T) {
	const username = "jdoe"
	rpc := NewAccessCheckRPC(nil, time.Second)

	uids, err := rpc.MembershipUIDsForUser(context.Background(), username)

	require.Error(t, err)
	assert.Nil(t, uids, "a transport failure must not produce a (possibly empty) result")
	var unavailable pkgerrors.ServiceUnavailable
	assert.ErrorAs(t, err, &unavailable, "transport failures must map to ServiceUnavailable so the endpoint returns 503, not 200 []")
	assert.False(t, pkgerrors.IsNotFound(err), "a transport failure must not look like a definitive miss")
	assert.NotContains(t, err.Error(), username, "transport errors must not expose the queried username")
}

// TestParseReadTuplesResponse pins the fail-closed contract of the reverse
// index: only a well-formed reply with no error envelope may produce a result
// (including the empty result for unknown users); every inconclusive body is
// ServiceUnavailable so an fga-sync fault surfaces as 503 rather than being
// mistaken for "no memberships". Tuple filtering is also pinned here: fga-sync
// scopes the query by user and object type, but the parser re-verifies each
// tuple rather than trusting that filtering blindly.
func TestParseReadTuplesResponse(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantUIDs []string
		wantErr  func(t *testing.T, err error)
	}{
		{
			name: "well-formed reply yields the user's membership UIDs",
			body: `{"results":[
				"project_membership:02i55000005xClOAAU#key_contact@user:jdoe",
				"project_membership:02i55000005xClPBBV#key_contact@user:jdoe"
			]}`,
			wantUIDs: []string{"02i55000005xClOAAU", "02i55000005xClPBBV"},
			wantErr:  func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name: "tuples for other users, relations, or object types are filtered out, not fatal",
			body: `{"results":[
				"project_membership:02i55000005xClOAAU#key_contact@user:jdoe",
				"project_membership:02i55000005xClQCCW#key_contact@user:other",
				"project_membership:02i55000005xClRDDX#writer@user:jdoe",
				"b2b_org:001B000000IqhSLIAZ#key_contact@user:jdoe",
				"not-a-tuple"
			]}`,
			wantUIDs: []string{"02i55000005xClOAAU"},
			wantErr:  func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:     "no matching tuples is a genuine empty result, not an error",
			body:     `{"results":[]}`,
			wantUIDs: []string{},
			wantErr:  func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:     "absent results field is a genuine empty result, not an error",
			body:     `{}`,
			wantUIDs: []string{},
			wantErr:  func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name: "error envelope is ServiceUnavailable, not an empty result",
			body: `{"error":"openfga unavailable"}`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				var unavailable pkgerrors.ServiceUnavailable
				assert.ErrorAs(t, err, &unavailable, "an fga-sync-reported error must fail closed, got %v", err)
			},
		},
		{
			name: "error envelope wins over partial results (fail closed, never partial data)",
			body: `{"results":["project_membership:02i55000005xClOAAU#key_contact@user:jdoe"],"error":"partial read"}`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				var unavailable pkgerrors.ServiceUnavailable
				assert.ErrorAs(t, err, &unavailable, "partial results alongside an error must not be served, got %v", err)
			},
		},
		{
			name: "empty body is inconclusive, not an empty result",
			body: "",
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				var unavailable pkgerrors.ServiceUnavailable
				assert.ErrorAs(t, err, &unavailable, "an absent reply body must fail closed, got %v", err)
			},
		},
		{
			name: "whitespace-only body is inconclusive, not an empty result",
			body: "   \n\t",
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				var unavailable pkgerrors.ServiceUnavailable
				assert.ErrorAs(t, err, &unavailable, "a whitespace-only reply body must fail closed, got %v", err)
			},
		},
		{
			name: "malformed JSON is inconclusive, not an empty result",
			body: `not json`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				var unavailable pkgerrors.ServiceUnavailable
				assert.ErrorAs(t, err, &unavailable, "an unparseable reply must fail closed, got %v", err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uids, err := parseReadTuplesResponse("jdoe", []byte(tt.body))
			tt.wantErr(t, err)
			assert.Equal(t, tt.wantUIDs, uids)
		})
	}
}

func TestMembershipUIDFromTuple(t *testing.T) {
	cases := []struct {
		name     string
		tuple    string
		username string
		wantUID  string
		wantOK   bool
	}{
		{
			name:     "key contact tuple for the queried user",
			tuple:    "project_membership:02i55000005xClOAAU#key_contact@user:jdoe",
			username: "jdoe",
			wantUID:  "02i55000005xClOAAU",
			wantOK:   true,
		},
		{
			name:     "tuple for a different user",
			tuple:    "project_membership:02i55000005xClOAAU#key_contact@user:other",
			username: "jdoe",
		},
		{
			name:     "non key_contact relation",
			tuple:    "project_membership:02i55000005xClOAAU#writer@user:jdoe",
			username: "jdoe",
		},
		{
			name:     "different object type",
			tuple:    "b2b_org:001B000000IqhSLIAZ#key_contact@user:jdoe",
			username: "jdoe",
		},
		{
			name:     "empty membership uid",
			tuple:    "project_membership:#key_contact@user:jdoe",
			username: "jdoe",
		},
		{
			name:     "malformed tuple",
			tuple:    "not-a-tuple",
			username: "jdoe",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid, ok := membershipUIDFromTuple(c.tuple, c.username)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantUID, uid)
		})
	}
}
