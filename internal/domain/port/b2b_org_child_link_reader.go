// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// B2BOrgChildLinkReader reads the b2b_org parent links the access model
// currently holds, which can differ from Salesforce after a missed reparent.
type B2BOrgChildLinkReader interface {
	// ParentsListingChild returns the UIDs of every b2b_org that holds a
	// child tuple for childUID. A failed or possibly incomplete read is an
	// error, never a partial list, because callers withdraw links absent
	// from Salesforce based on it.
	ParentsListingChild(ctx context.Context, childUID string) ([]string, error)
}
