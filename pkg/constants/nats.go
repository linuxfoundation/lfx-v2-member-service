// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines shared constant values used across the service.
package constants

// NATS KV bucket constants shared across the NATS infrastructure layer.
const (
	// ProjectIDMapLookupSubject is the NATS request/reply subject for resolving
	// a v2 project UID to a Salesforce Project__c.Id. The member service handles
	// this subject.
	ProjectIDMapLookupSubject = "lfx.member.project-id-map.lookup"

	// B2BOrgLookupSubject is the NATS request/reply subject for resolving a
	// b2b_org by id. Request body: {"id":"<uid>"}. Reply: {"id":"<canonical-18-char-sfid>"}
	// on success, or {"error":"..."} when not found or invalid.
	B2BOrgLookupSubject = "lfx.member.b2b_org_lookup"
)
