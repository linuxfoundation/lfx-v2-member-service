// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// AccountAction is the outcome of the Salesforce Apex account find-or-create
// call (lfx-self-serve#4997).
type AccountAction string

const (
	// AccountActionMatched means one existing Account has the website domain.
	AccountActionMatched AccountAction = "matched"
	// AccountActionCreated means a Company Account was created.
	AccountActionCreated AccountAction = "created"
	// AccountActionAmbiguous means several Accounts have the website domain; nothing was written.
	AccountActionAmbiguous AccountAction = "ambiguous"
)
