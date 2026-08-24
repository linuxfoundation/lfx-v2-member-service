// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// Callers derive further keys from VersionedURL before ever calling Put, so an
// unusable URL would fail them earlier and hide the typed NotImplemented error
// mock mode exists to return.
func TestMockObjectStoreWriter_VersionedURLIsUsable(t *testing.T) {
	store := &MockObjectStoreWriter{}

	raw := store.VersionedURL("b2b_org_logos/uid-1")

	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	assert.NotEmpty(t, parsed.Host)
	assert.Contains(t, parsed.Path, "b2b_org_logos/uid-1")
	assert.NotEmpty(t, parsed.Query().Get("v"), "the version token is what scratch keys are derived from")
	assert.Equal(t, raw, store.VersionedURL("b2b_org_logos/uid-1"), "must be deterministic")

	_, putErr := store.Put(context.Background(), "b2b_org_logos/uid-1", "image/png", []byte("x"))
	var notImplemented errors.NotImplemented
	assert.ErrorAs(t, putErr, &notImplemented, "mock mode must surface NotImplemented from Put")
}
