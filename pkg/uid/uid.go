// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package uid provides canonical deterministic UUID v5 generation functions for LFX
// Salesforce-sourced objects. Functions in this package are used by the API response
// layer to derive stable v2 UIDs from raw Salesforce SFIDs stored in domain model
// fields, ensuring that v1 identifiers are never exposed directly in v2 API responses.
//
// Namespace note: the DNS namespace (6ba7b810-9dad-11d1-80b4-00c04fd430c8) is used
// here to match the original PostgreSQL sync job's UID generation logic. Data already
// written to NATS KV was generated with this namespace and prefix scheme, so the
// results are stable across syncs.
package uid

import "github.com/google/uuid"

// dnsNamespace is the standard DNS UUID namespace (RFC 4122). It was used by the
// original PostgreSQL sync job for generating deterministic member and membership UIDs.
// It must not be changed — doing so would produce different UIDs for already-stored data.
var dnsNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

// ForMember returns the deterministic v2 member UID for the given Salesforce Account
// SFID. Uses the DNS namespace with an "lfx-member:" seed prefix, matching the original
// sync job's generateMemberUID function. The result is stable across syncs.
func ForMember(accountSFID string) string {
	return uuid.NewSHA1(dnsNamespace, []byte("lfx-member:"+accountSFID)).String()
}

// ForMembershipTier returns the deterministic v2 membership tier UID for the given
// Salesforce Product2 SFID. Uses the DNS namespace with an "lfx-membership:" seed
// prefix, consistent with the sync job's generateDeterministicUID convention.
// Product2 SFIDs are globally unique and do not collide with Asset SFIDs in practice.
func ForMembershipTier(product2SFID string) string {
	return uuid.NewSHA1(dnsNamespace, []byte("lfx-membership:"+product2SFID)).String()
}
