// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeCursor(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		want    PageCursor
		wantErr bool
	}{
		{
			name: "empty token decodes to zero cursor",
			want: PageCursor{},
		},
		{
			name:  "round-trips a cursor produced by EncodeCursor",
			token: EncodeCursor(PageCursor{BatchIndex: 2, BatchOffset: 50, PageSize: 50, NextBatchIterator: "iterabc1"}),
			want:  PageCursor{BatchIndex: 2, BatchOffset: 50, PageSize: 50, NextBatchIterator: "iterabc1"},
		},
		{
			name:    "invalid base64 is rejected",
			token:   "not-valid-base64!!!",
			wantErr: true,
		},
		{
			name:    "valid base64 but invalid JSON is rejected",
			token:   base64.RawURLEncoding.EncodeToString([]byte("not json")),
			wantErr: true,
		},
		{
			// A forged token with a negative BatchOffset must never reach
			// sliceCachedBatch, which only clamps the high end of the slice
			// bounds and panics on a negative start index.
			name:    "negative BatchOffset is rejected",
			token:   EncodeCursor(PageCursor{BatchIndex: 0, BatchOffset: -5, PageSize: 10}),
			wantErr: true,
		},
		{
			name:    "negative BatchIndex is rejected",
			token:   EncodeCursor(PageCursor{BatchIndex: -1, PageSize: 10}),
			wantErr: true,
		},
		{
			name:    "negative PageSize is rejected",
			token:   EncodeCursor(PageCursor{PageSize: -10}),
			wantErr: true,
		},
		{
			// A forged NextBatchIterator that doesn't match the shape the
			// service itself generates must never reach member_reader.go's
			// cache lookup, where it would force a guaranteed cache miss and
			// a full synchronous Salesforce sweep on every request.
			name:    "malformed NextBatchIterator is rejected",
			token:   EncodeCursor(PageCursor{PageSize: 10, NextBatchIterator: "not-an-iterator"}),
			wantErr: true,
		},
		{
			name:  "well-formed alphanumeric NextBatchIterator is accepted",
			token: EncodeCursor(PageCursor{PageSize: 10, NextBatchIterator: "aB3dE7gH"}),
			want:  PageCursor{PageSize: 10, NextBatchIterator: "aB3dE7gH"},
		},
		{
			// newBatchIterator base64url-encodes 6 random bytes, so '-' and
			// '_' are expected characters, not just alphanumerics.
			name:  "well-formed base64url NextBatchIterator with - and _ is accepted",
			token: EncodeCursor(PageCursor{PageSize: 10, NextBatchIterator: "WU-hvkhZ"}),
			want:  PageCursor{PageSize: 10, NextBatchIterator: "WU-hvkhZ"},
		},
		{
			name: "zero PageSize (unset) is accepted",
			want: PageCursor{PageSize: 0},
		},
		{
			// PageSize is not covered by FingerprintCursor's digest, so a
			// tampered-but-fingerprint-valid token could otherwise request an
			// off-menu page size; reject anything outside ValidPageSizes here
			// instead of letting NormalizePageSize silently absorb it.
			name:    "a PageSize outside ValidPageSizes is rejected",
			token:   EncodeCursor(PageCursor{PageSize: 1}),
			wantErr: true,
		},
		{
			name:  "a PageSize in ValidPageSizes is accepted",
			token: EncodeCursor(PageCursor{PageSize: 500}),
			want:  PageCursor{PageSize: 500},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeCursor(tc.token)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// Guards against iteratorPattern drifting out of sync with
	// newBatchIterator's actual output alphabet (as happened when it was
	// first written against the stale "alphanumeric" prose in
	// nats/storage.go instead of the real base64url generator).
	t.Run("accepts 1000 generated iterators", func(t *testing.T) {
		for i := 0; i < 1000; i++ {
			c := PageCursor{PageSize: 50, NextBatchIterator: newBatchIterator()}
			got, err := DecodeCursor(EncodeCursor(c))
			require.NoError(t, err, "iterator %q", c.NextBatchIterator)
			assert.Equal(t, c, got)
		}
	})
}

func TestFingerprintCursor(t *testing.T) {
	base := FingerprintCursor([]string{"project-sfid-1", "name_asc", "tier-sfid-1"}, 2, "iterabc1")

	tests := []struct {
		name        string
		batchParams []string
		batchIndex  int
		iterator    string
		wantEqual   bool
	}{
		{
			name:        "identical inputs produce identical fingerprints",
			batchParams: []string{"project-sfid-1", "name_asc", "tier-sfid-1"},
			batchIndex:  2,
			iterator:    "iterabc1",
			wantEqual:   true,
		},
		{
			// A page_token minted under one tier_uid must not validate
			// against a request with a different tier_uid.
			name:        "different tier_uid produces a different fingerprint",
			batchParams: []string{"project-sfid-1", "name_asc", "tier-sfid-2"},
			batchIndex:  2,
			iterator:    "iterabc1",
			wantEqual:   false,
		},
		{
			// ...or an omitted one.
			name:        "omitted tier_uid produces a different fingerprint",
			batchParams: []string{"project-sfid-1", "name_asc"},
			batchIndex:  2,
			iterator:    "iterabc1",
			wantEqual:   false,
		},
		{
			// A decoded token with its BatchIndex tampered with (without
			// re-minting) must fail validation rather than silently miss
			// the cache and trigger a full re-sweep.
			name:        "different batch index produces a different fingerprint",
			batchParams: []string{"project-sfid-1", "name_asc", "tier-sfid-1"},
			batchIndex:  99,
			iterator:    "iterabc1",
			wantEqual:   false,
		},
		{
			// Same for NextBatchIterator.
			name:        "different iterator produces a different fingerprint",
			batchParams: []string{"project-sfid-1", "name_asc", "tier-sfid-1"},
			batchIndex:  2,
			iterator:    "other-iter",
			wantEqual:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FingerprintCursor(tt.batchParams, tt.batchIndex, tt.iterator)
			if tt.wantEqual {
				assert.Equal(t, base, got)
			} else {
				assert.NotEqual(t, base, got)
			}
		})
	}
}
