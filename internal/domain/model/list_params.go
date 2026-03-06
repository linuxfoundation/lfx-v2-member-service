// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// ListParams represents pagination and filter parameters
type ListParams struct {
	PageSize int
	Offset   int
	Filters  map[string]string
	Search   string
}
