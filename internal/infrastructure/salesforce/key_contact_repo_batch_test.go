// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

func TestKeyContactRepo_FetchKeyContactsByAssetSFIDs(t *testing.T) {
	t.Run("groups contacts and batches email lookup", func(t *testing.T) {
		firstRawAsset := "02i000000000001"
		secondRawAsset := "02i000000000002"
		firstAsset, err := sfuuid.Normalize18(firstRawAsset)
		require.NoError(t, err)
		secondAsset, err := sfuuid.Normalize18(secondRawAsset)
		require.NoError(t, err)
		firstRole := "a0F000000000001"
		secondRole := "a0F000000000002"
		firstContact := "003000000000001"
		secondContact := "003000000000002"
		roleResponse := fmt.Sprintf(
			`{"totalSize":2,"done":true,"records":[`+
				`{"Id":%q,"Asset__c":%q,"Contact__c":%q,"Contact__r":{"Id":%q,"Email":"one@example.com"}},`+
				`{"Id":%q,"Asset__c":%q,"Contact__c":%q,"Contact__r":{"Id":%q,"Email":"two@example.com"}}]}`,
			firstRole, firstAsset, firstContact, firstContact,
			secondRole, secondAsset, secondContact, secondContact,
		)
		transport := &seqQueryTransport{responses: []string{
			roleResponse,
			`{"totalSize":0,"done":true,"records":[]}`,
		}}
		repo := NewKeyContactRepo(fakeSalesforce(t, transport))

		grouped, err := repo.FetchKeyContactsByAssetSFIDs(
			context.Background(),
			[]string{firstRawAsset, secondRawAsset},
		)

		require.NoError(t, err)
		require.Len(t, grouped[firstAsset], 1)
		require.Len(t, grouped[secondAsset], 1)
		assert.Equal(t, "one@example.com", grouped[firstAsset][0].Email)
		assert.Equal(t, "two@example.com", grouped[secondAsset][0].Email)
		assert.Equal(t, 2, transport.queryCalls, "one roles query plus one combined email query")
	})

	t.Run("chunks more than two hundred memberships", func(t *testing.T) {
		assetSFIDs := make([]string, 201)
		for i := range assetSFIDs {
			assetSFID, err := sfuuid.Normalize18(fmt.Sprintf("02i%012d", i))
			require.NoError(t, err)
			assetSFIDs[i] = assetSFID
		}
		firstRole := "a0F000000000003"
		secondRole := "a0F000000000004"
		transport := &seqQueryTransport{responses: []string{
			fmt.Sprintf(`{"totalSize":1,"done":true,"records":[{"Id":%q,"Asset__c":%q}]}`,
				firstRole, assetSFIDs[0]),
			fmt.Sprintf(`{"totalSize":1,"done":true,"records":[{"Id":%q,"Asset__c":%q}]}`,
				secondRole, assetSFIDs[200]),
		}}
		repo := NewKeyContactRepo(fakeSalesforce(t, transport))

		grouped, err := repo.FetchKeyContactsByAssetSFIDs(context.Background(), assetSFIDs)

		require.NoError(t, err)
		assert.Len(t, grouped, 201)
		require.Len(t, grouped[assetSFIDs[0]], 1)
		require.Len(t, grouped[assetSFIDs[200]], 1)
		assert.Equal(t, firstRole, grouped[assetSFIDs[0]][0].UID[:15])
		assert.Equal(t, secondRole, grouped[assetSFIDs[200]][0].UID[:15])
		assert.Equal(t, 2, transport.queryCalls)
	})

	t.Run("fails when authoritative email lookup fails", func(t *testing.T) {
		assetSFID, err := sfuuid.Normalize18("02i000000000005")
		require.NoError(t, err)
		transport := &seqQueryTransport{
			responses: []string{
				fmt.Sprintf(
					`{"totalSize":1,"done":true,"records":[{"Id":"a0F000000000005","Asset__c":%q,"Contact__c":"003000000000005","Contact__r":{"Id":"003000000000005","Email":"fallback@example.com"}}]}`,
					assetSFID,
				),
				`[{"message":"temporarily unavailable","errorCode":"SERVER_UNAVAILABLE"}]`,
			},
			queryStatuses: []int{200, 500},
		}
		repo := NewKeyContactRepo(fakeSalesforce(t, transport))

		grouped, err := repo.FetchKeyContactsByAssetSFIDs(context.Background(), []string{assetSFID})

		require.Error(t, err)
		assert.Nil(t, grouped)
		assert.Equal(t, 2, transport.queryCalls)
	})

	for name, assetSFID := range map[string]string{
		"malformed": "not-an-sfid",
		"empty":     "",
	} {
		t.Run("rejects "+name+" membership ID before querying Salesforce", func(t *testing.T) {
			transport := &seqQueryTransport{}
			repo := NewKeyContactRepo(fakeSalesforce(t, transport))

			grouped, err := repo.FetchKeyContactsByAssetSFIDs(context.Background(), []string{assetSFID})

			require.Error(t, err)
			assert.Nil(t, grouped)
			assert.Zero(t, transport.queryCalls)
		})
	}
}
