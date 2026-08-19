// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// White-box tests for validateKeyContactGrant (unexported — pure validation
// logic shared by KeyContactGrantIndex.Put, kept package-internal since it is
// not part of the exported adapter surface).

func TestValidateKeyContactGrant_CompleteLivePairNoMarker_Valid(t *testing.T) {
	err := validateKeyContactGrant("kc-1", port.KeyContactGrant{MembershipUID: "asset-1", Username: "alice"})
	assert.NoError(t, err)
}

func TestValidateKeyContactGrant_MarkerOnlyCompletePendingRevoke_Valid(t *testing.T) {
	err := validateKeyContactGrant("kc-1", port.KeyContactGrant{
		PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-old", Username: "bob"},
	})
	assert.NoError(t, err)
}

func TestValidateKeyContactGrant_LivePairWithCompleteMarker_Valid(t *testing.T) {
	err := validateKeyContactGrant("kc-1", port.KeyContactGrant{
		MembershipUID: "asset-1",
		Username:      "alice",
		PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-old", Username: "bob"},
	})
	assert.NoError(t, err)
}

func TestValidateKeyContactGrant_PartialLivePair_Rejected(t *testing.T) {
	err := validateKeyContactGrant("kc-1", port.KeyContactGrant{MembershipUID: "asset-1"})
	assert.Error(t, err, "a MembershipUID with no Username would silently drop to fully empty on the next Get via omitempty")

	err = validateKeyContactGrant("kc-1", port.KeyContactGrant{Username: "alice"})
	assert.Error(t, err, "a Username with no MembershipUID would silently drop to fully empty on the next Get via omitempty")
}

func TestValidateKeyContactGrant_PartialPendingRevoke_Rejected(t *testing.T) {
	err := validateKeyContactGrant("kc-1", port.KeyContactGrant{
		MembershipUID: "asset-1",
		Username:      "alice",
		PendingRevoke: &port.KeyContactGrantRef{MembershipUID: "asset-old"},
	})
	assert.Error(t, err, "a PendingRevoke with only MembershipUID set would drop the marker's address entirely on the next Get")

	err = validateKeyContactGrant("kc-1", port.KeyContactGrant{
		MembershipUID: "asset-1",
		Username:      "alice",
		PendingRevoke: &port.KeyContactGrantRef{Username: "bob"},
	})
	assert.Error(t, err, "a PendingRevoke with only Username set would drop the marker's address entirely on the next Get")
}

func TestValidateKeyContactGrant_EmptyPairAndEmptyMarker_Rejected(t *testing.T) {
	err := validateKeyContactGrant("kc-1", port.KeyContactGrant{})
	assert.Error(t, err, "an entry with no live pair and no PendingRevoke addresses nothing")

	err = validateKeyContactGrant("kc-1", port.KeyContactGrant{PendingRevoke: &port.KeyContactGrantRef{}})
	assert.Error(t, err, "a non-nil but fully-empty PendingRevoke is not a complete marker — it must be rejected, not treated as a valid marker-only entry")
}
