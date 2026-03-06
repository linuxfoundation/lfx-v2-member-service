// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// NATS Key-Value store bucket names.
const (
	// KVBucketNameMemberships is the name of the KV bucket for memberships.
	KVBucketNameMemberships = "memberships"

	// KVBucketNameMembershipContacts is the name of the KV bucket for membership contacts.
	KVBucketNameMembershipContacts = "membership-contacts"

	// KVBucketNameMembers is the name of the KV bucket for members (accounts).
	KVBucketNameMembers = "members"
)

// KV lookup key prefixes for indexed queries.
const (
	// KVLookupProjectPrefix is the prefix for project-based membership lookups.
	// Key format: lookup/project/{project_id}/{membership_uid}
	KVLookupProjectPrefix = "lookup/project/%s/%s"

	// KVLookupMembershipContactPrefix is the prefix for membership-based contact lookups.
	// Key format: lookup/membership/{membership_uid}/{contact_uid}
	KVLookupMembershipContactPrefix = "lookup/membership/%s/%s"

	// KVLookupMemberByProjectPrefix is the prefix for project-based member lookups.
	// Key format: lookup/member-project/{project_id}/{member_uid}
	KVLookupMemberByProjectPrefix = "lookup/member-project/%s/%s"

	// KVLookupMemberBySFIDPrefix is the prefix for SFID-based member lookups.
	// Key format: lookup/member-sfid/{sfid}
	KVLookupMemberBySFIDPrefix = "lookup/member-sfid/%s"

	// KVLookupMembershipByMemberPrefix is the prefix for member-based membership lookups.
	// Key format: lookup/member-membership/{member_uid}/{membership_uid}
	KVLookupMembershipByMemberPrefix = "lookup/member-membership/%s/%s"
)
