// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package pubsub

import (
	"context"
	"fmt"

	goavro "github.com/linkedin/goavro/v2"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	pbproto "github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce/pubsub/proto"
)

// decodeEvent decodes the Avro-encoded payload of a ConsumerEvent into a
// normalized CDCEvent. Schema is fetched once and cached by schema_id.
func (c *Client) decodeEvent(ctx context.Context, event *pbproto.ConsumerEvent) (model.CDCEvent, error) {
	pe := event.GetEvent()
	if pe == nil {
		return model.CDCEvent{}, fmt.Errorf("nil ProducerEvent")
	}

	schemaJSON, err := c.fetchSchema(ctx, pe.GetSchemaId())
	if err != nil {
		return model.CDCEvent{}, err
	}

	codec, err := c.getOrCreateCodec(pe.GetSchemaId(), schemaJSON)
	if err != nil {
		return model.CDCEvent{}, fmt.Errorf("pubsub: create avro codec: %w", err)
	}

	native, _, err := codec.NativeFromBinary(pe.GetPayload())
	if err != nil {
		return model.CDCEvent{}, fmt.Errorf("pubsub: avro decode: %w", err)
	}

	record, ok := native.(map[string]interface{})
	if !ok {
		return model.CDCEvent{}, fmt.Errorf("pubsub: unexpected avro type %T", native)
	}

	// ReplayId lives on ConsumerEvent, not on the inner ProducerEvent.
	return normalizeRecord(record, event.GetReplayId())
}

// getOrCreateCodec returns a cached goavro Codec for the schemaID, creating
// and storing one if not yet cached. Codecs are reused because compilation is
// expensive and the schema does not change for a given schema_id.
func (c *Client) getOrCreateCodec(schemaID, schemaJSON string) (*goavro.Codec, error) {
	// Check codec cache (separate from schema-JSON cache in schemaCache).
	if v, ok := c.codecCache.Load(schemaID); ok {
		return v.(*goavro.Codec), nil
	}

	codec, err := goavro.NewCodec(schemaJSON)
	if err != nil {
		return nil, err
	}

	actual, _ := c.codecCache.LoadOrStore(schemaID, codec)
	return actual.(*goavro.Codec), nil
}

// normalizeRecord converts a decoded Avro record (map[string]interface{}) into
// a CDCEvent by extracting the ChangeEventHeader fields we care about.
//
// Salesforce CDC events always have a top-level "ChangeEventHeader" union field
// whose value is a map. entityName and changeType may be absent or empty for
// gap/overflow events; missing values become empty string (caller filters by entity).
func normalizeRecord(record map[string]interface{}, replayID []byte) (model.CDCEvent, error) {
	headerRaw, ok := record["ChangeEventHeader"]
	if !ok {
		return model.CDCEvent{}, fmt.Errorf("pubsub: missing ChangeEventHeader")
	}

	// Avro union fields are wrapped: map[string]interface{}{"com.salesforce...": actualValue}
	header, err := unwrapUnion(headerRaw)
	if err != nil {
		return model.CDCEvent{}, fmt.Errorf("pubsub: unwrap ChangeEventHeader: %w", err)
	}

	headerMap, ok := header.(map[string]interface{})
	if !ok {
		return model.CDCEvent{}, fmt.Errorf("pubsub: ChangeEventHeader not a map, got %T", header)
	}

	entity, _ := headerMap["entityName"].(string)
	changeType, _ := headerMap["changeType"].(string)

	recordIDs, err := extractStringSlice(headerMap, "recordIds")
	if err != nil {
		return model.CDCEvent{}, fmt.Errorf("pubsub: recordIds: %w", err)
	}

	return model.CDCEvent{
		Entity:       entity,
		RecordIDs:    recordIDs,
		ChangeType:   model.CDCChangeType(changeType),
		ReplayID:     replayID,
		MissingAsset: projectRoleCreateMissingAsset(record, entity, changeType),
	}, nil
}

// projectRoleCreateMissingAsset reports whether a Project_Role__c CREATE body
// carries Asset__c as null or empty. A Pub/Sub CREATE body holds every field,
// so empty there means the role really has no Asset; on UPDATE unchanged
// fields are also empty, so UPDATE never matches. Anything uncertain returns
// false and the event is processed normally.
func projectRoleCreateMissingAsset(record map[string]interface{}, entity, changeType string) bool {
	if entity != "Project_Role__c" || changeType != string(model.CDCChangeCreate) {
		return false
	}
	raw, ok := record["Asset__c"]
	if !ok {
		return false
	}
	v, err := unwrapUnion(raw)
	if err != nil {
		return false
	}
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	default:
		return false
	}
}

// unwrapUnion handles goavro's Avro union encoding. Non-null union values are
// returned as map[string]interface{}{"<type-name>": value}; we extract the value.
func unwrapUnion(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		if len(t) == 1 {
			for _, inner := range t {
				return inner, nil
			}
		}
		// Already a plain map (not a union wrapper) — return as-is.
		return v, nil
	default:
		return v, nil
	}
}

// extractStringSlice pulls a []string from a map field that Avro may encode as
// []interface{} (each element a string).
func extractStringSlice(m map[string]interface{}, key string) ([]string, error) {
	raw, ok := m[key]
	if !ok {
		return nil, nil
	}

	slice, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected []interface{} for %s, got %T", key, raw)
	}

	out := make([]string, 0, len(slice))
	for _, elem := range slice {
		s, ok := elem.(string)
		if !ok {
			return nil, fmt.Errorf("non-string element in %s: %T", key, elem)
		}
		out = append(out, s)
	}
	return out, nil
}
