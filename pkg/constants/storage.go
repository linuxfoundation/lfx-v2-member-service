// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// NATS Key-Value store bucket names.
const (
	// KVBucketNameMemberships is the name of the KV bucket for memberships.
	KVBucketNameMemberships = "memberships"

	// KVBucketNameMembershipContacts is the name of the KV bucket for membership contacts.
	KVBucketNameMembershipContacts = "membership-contacts"
)

// KV lookup key prefixes for indexed queries.
const (
	// KVLookupProjectPrefix is the prefix for project-based membership lookups.
	// Key format: lookup/project/{project_id}/{membership_uid}
	KVLookupProjectPrefix = "lookup/project/%s/%s"

	// KVLookupMembershipContactPrefix is the prefix for membership-based contact lookups.
	// Key format: lookup/membership/{membership_uid}/{contact_uid}
	KVLookupMembershipContactPrefix = "lookup/membership/%s/%s"
)
