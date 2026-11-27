// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package pubsub

import (
	"testing"

	goavro "github.com/linkedin/goavro/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRoleID = "a0B000000000001AAA"

func cdcRecord(entity, changeType string, body map[string]interface{}) map[string]interface{} {
	record := map[string]interface{}{
		"ChangeEventHeader": map[string]interface{}{
			"entityName": entity,
			"changeType": changeType,
			"recordIds":  []interface{}{testRoleID},
		},
	}
	for k, v := range body {
		record[k] = v
	}
	return record
}

func TestNormalizeRecord_MissingAsset(t *testing.T) {
	cases := []struct {
		name       string
		entity     string
		changeType string
		body       map[string]interface{}
		want       bool
	}{
		{"role create with null asset", "Project_Role__c", "CREATE", map[string]interface{}{"Asset__c": nil}, true},
		{"role create with empty asset", "Project_Role__c", "CREATE", map[string]interface{}{"Asset__c": map[string]interface{}{"string": ""}}, true},
		{"role create with asset", "Project_Role__c", "CREATE", map[string]interface{}{"Asset__c": map[string]interface{}{"string": "02i000000000001AAA"}}, false},
		{"role create without asset field", "Project_Role__c", "CREATE", nil, false},
		{"role create with unexpected asset type", "Project_Role__c", "CREATE", map[string]interface{}{"Asset__c": 42}, false},
		{"role update with null asset", "Project_Role__c", "UPDATE", map[string]interface{}{"Asset__c": nil}, false},
		{"role gap create with null asset", "Project_Role__c", "GAP_CREATE", map[string]interface{}{"Asset__c": nil}, false},
		{"account create with null asset", "Account", "CREATE", map[string]interface{}{"Asset__c": nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, err := normalizeRecord(cdcRecord(tc.entity, tc.changeType, tc.body), []byte("r1"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, event.MissingAsset)
			assert.Equal(t, []string{testRoleID}, event.RecordIDs)
		})
	}
}

// Proves the nil / union handling against goavro's real decoding rather than
// hand-built maps.
func TestNormalizeRecord_MissingAsset_GoavroRoundTrip(t *testing.T) {
	codec, err := goavro.NewCodec(`{
		"type": "record", "name": "Project_Role__ChangeEvent",
		"fields": [
			{"name": "ChangeEventHeader", "type": {
				"type": "record", "name": "ChangeEventHeader",
				"fields": [
					{"name": "entityName", "type": "string"},
					{"name": "changeType", "type": "string"},
					{"name": "recordIds", "type": {"type": "array", "items": "string"}}
				]}},
			{"name": "Asset__c", "type": ["null", "string"], "default": null}
		]}`)
	require.NoError(t, err)

	cases := []struct {
		name  string
		asset interface{}
		want  bool
	}{
		{"null asset", nil, true},
		{"asset set", goavro.Union("string", "02i000000000001AAA"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, err := codec.BinaryFromNative(nil, map[string]interface{}{
				"ChangeEventHeader": map[string]interface{}{
					"entityName": "Project_Role__c",
					"changeType": "CREATE",
					"recordIds":  []interface{}{testRoleID},
				},
				"Asset__c": tc.asset,
			})
			require.NoError(t, err)
			native, _, err := codec.NativeFromBinary(bin)
			require.NoError(t, err)

			event, err := normalizeRecord(native.(map[string]interface{}), []byte("r1"))
			require.NoError(t, err)
			assert.Equal(t, tc.want, event.MissingAsset)
		})
	}
}
