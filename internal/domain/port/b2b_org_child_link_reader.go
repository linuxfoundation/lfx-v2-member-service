// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"errors"
)

// ErrChildLinksUncheckable marks a parent-link read that can never complete
// for this org: the reply also carries one tuple per child org, so an org
// with enough children always exceeds the reply cap and a retry cannot help.
var ErrChildLinksUncheckable = errors.New("b2b_org parent links cannot be read completely")

// B2BOrgChildLinkReader reads the b2b_org parent links the access model
// currently holds, which can differ from Salesforce after a missed reparent.
type B2BOrgChildLinkReader interface {
	// ParentsListingChild returns the UIDs of every b2b_org that holds a
	// child tuple for childUID. A failed or possibly incomplete read is an
	// error, never a partial list, because callers withdraw links absent
	// from Salesforce based on it. A read that can never be complete wraps
	// ErrChildLinksUncheckable.
	ParentsListingChild(ctx context.Context, childUID string) ([]string, error)
}
