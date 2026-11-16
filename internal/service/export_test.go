// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

// Drain limits exposed to the external service_test package, so cdc_repair
// tests are sized from the real values rather than copies of them.
const (
	RepairPageCap                    = repairPageCap
	MaxKeyContactLookupDrainAttempts = maxKeyContactLookupDrainAttempts
)
