// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import "time"

// Drain limits exposed to the external service_test package, so cdc_repair
// tests are sized from the real values rather than copies of them.
const (
	RepairPageCap                    = repairPageCap
	MaxKeyContactLookupDrainAttempts = maxKeyContactLookupDrainAttempts
)

// SetCDCEventTimeoutForTest shortens the per-event deadline and returns a func
// that restores it.
func SetCDCEventTimeoutForTest(d time.Duration) (restore func()) {
	old := cdcEventTimeout
	cdcEventTimeout = d
	return func() { cdcEventTimeout = old }
}
