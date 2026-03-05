// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// FGASyncMessage is the envelope sent to the fga-sync service over NATS.
type FGASyncMessage struct {
	ObjectType string      `json:"object_type"`
	Operation  string      `json:"operation"`
	Data       FGASyncData `json:"data"`
}

// FGASyncData is the payload for an update_access operation.
type FGASyncData struct {
	UID        string              `json:"uid"`
	Public     bool                `json:"public"`
	Relations  map[string][]string `json:"relations,omitempty"`
	References map[string][]string `json:"references,omitempty"`
}
