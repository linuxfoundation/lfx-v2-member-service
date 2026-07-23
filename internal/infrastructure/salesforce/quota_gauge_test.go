// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIUsageGauge_Snapshot_MirrorsCoherentObservation(t *testing.T) {
	recordQuotaObservation(111, 9000)

	g := NewAPIUsageGauge(nil)
	snap := g.Snapshot()

	assert.Equal(t, int64(111), snap.Current)
	assert.Equal(t, int64(9000), snap.Limit)
	assert.True(t, snap.Observed())
	assert.InDelta(t, 111.0/9000.0, snap.Ratio(), 0.0001)
}

func TestAPIUsageGauge_Refresh_NotConfigured(t *testing.T) {
	g := NewAPIUsageGauge(nil)

	_, err := g.Refresh(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "not configured")
}

func TestAPIUsageGauge_Refresh_RequestFails(t *testing.T) {
	wantErr := errors.New("network unreachable")
	g := NewAPIUsageGauge(func(_ context.Context) error {
		return wantErr
	})

	_, err := g.Refresh(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "request failed")
	assert.ErrorIs(t, err, wantErr)
}

func TestAPIUsageGauge_Refresh_NoNewerObservation(t *testing.T) {
	// refreshFn succeeds but never calls recordQuotaObservation — simulates a
	// response with a missing/malformed Sforce-Limit-Info header.
	g := NewAPIUsageGauge(func(_ context.Context) error {
		return nil
	})

	_, err := g.Refresh(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "no newer observation")
}

func TestAPIUsageGauge_Refresh_InvalidObservation(t *testing.T) {
	g := NewAPIUsageGauge(func(_ context.Context) error {
		// Advances the generation but with an invalid limit (<=0).
		recordQuotaObservation(5, 0)
		return nil
	})

	_, err := g.Refresh(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid observation")
}

func TestAPIUsageGauge_Refresh_Success(t *testing.T) {
	g := NewAPIUsageGauge(func(_ context.Context) error {
		recordQuotaObservation(42, 15000)
		return nil
	})

	before := g.Snapshot()
	snap, err := g.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(42), snap.Current)
	assert.Equal(t, int64(15000), snap.Limit)
	assert.Greater(t, snap.Generation, before.Generation)
}

func TestAPIUsageGauge_Refresh_ConcurrentObservationIsAcceptable(t *testing.T) {
	// A newer observation produced by a concurrent Salesforce response (not the
	// active refresh itself) still satisfies Refresh — it represents the same
	// org-wide quota. Simulate this by recording an observation *before*
	// refreshFn runs its own recordQuotaObservation call.
	g := NewAPIUsageGauge(func(_ context.Context) error {
		recordQuotaObservation(1, 100) // "concurrent" passive observation
		recordQuotaObservation(2, 200) // refreshFn's own observation
		return nil
	})

	snap, err := g.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), snap.Current)
	assert.Equal(t, int64(200), snap.Limit)
}

// TestNewLimitsRefreshFunc_RequestsSinglyPrefixedPath guards against a
// regression where the refresh function's uri argument to client.DoRequest
// duplicates the "/services/data/{apiVersion}" prefix that go-salesforce's
// doRequest already adds internally (requests.go: endpoint := auth.InstanceUrl
// + "/services/data/" + config.apiVersion + payload.uri). Passing the
// full-prefixed path here would 404 against a real Salesforce org — as
// observed live against SIT during manual verification.
func TestNewLimitsRefreshFunc_RequestsSinglyPrefixedPath(t *testing.T) {
	rt := &routingTransport{}
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, map[string]string{
		"Sforce-Limit-Info": "api-usage=7/2000",
	}))

	client := fakeSalesforce(t, rt)
	refresh := NewLimitsRefreshFunc(client)

	err := refresh(context.Background())
	require.NoError(t, err)

	require.NotEmpty(t, rt.requests, "expected at least one recorded request")
	last := rt.requests[len(rt.requests)-1]
	wantPath := fmt.Sprintf("/services/data/%s/limits", client.GetAPIVersion())
	assert.Equal(t, wantPath, last.URL.Path,
		"uri passed to DoRequest must be relative; DoRequest already adds the /services/data/{version} prefix")
}
