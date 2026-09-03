// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "strings"

// Normalized membership tier classes exposed by the member-tiers endpoint. Raw
// Salesforce product names (e.g. "Gold Corporate Membership") collapse into
// these classes for cross-project comparison.
//
// The class list, keywords, and rank order stay in lockstep with the canonical
// Org Lens taxonomy in the lf-dbt model
// platinum_lfx_one_org_lens_account_membership_tier: a class added there must
// be added here with the same relative rank. That model's title-case classes
// ("End User") map to the snake_case wire values here ("end_user").
const (
	TierClassPlatinum    = "platinum"
	TierClassPremier     = "premier"
	TierClassFounding    = "founding"
	TierClassStrategic   = "strategic"
	TierClassGold        = "gold"
	TierClassSteering    = "steering"
	TierClassSilver      = "silver"
	TierClassGeneral     = "general"
	TierClassAssociate   = "associate"
	TierClassEndUser     = "end_user"
	TierClassAcademic    = "academic"
	TierClassContributor = "contributor"
	TierClassOther       = "other"
)

// tierClassKeywords maps case-insensitive substrings of raw tier product
// names to normalized classes. Checked in rank order, first match wins, so a
// name matching several keywords (e.g. "Premier Sponsor") resolves to the
// highest-ranked class, mirroring the dbt CASE expression.
var tierClassKeywords = []struct {
	keyword string
	class   string
}{
	{"platinum", TierClassPlatinum},
	{"premier", TierClassPremier},
	{"founding", TierClassFounding},
	{"strategic", TierClassStrategic},
	{"gold", TierClassGold},
	{"steering", TierClassSteering},
	{"silver", TierClassSilver},
	{"general", TierClassGeneral},
	{"associate", TierClassAssociate},
	{"end user", TierClassEndUser},
	{"academic", TierClassAcademic},
	{"contributor", TierClassContributor},
}

// tierClassRanks orders tier classes for highest-tier-per-org selection;
// higher ranks win. The relative order matches the dbt model's tier_rank
// (which counts the other way, 1 = highest).
var tierClassRanks = map[string]int{
	TierClassPlatinum:    12,
	TierClassPremier:     11,
	TierClassFounding:    10,
	TierClassStrategic:   9,
	TierClassGold:        8,
	TierClassSteering:    7,
	TierClassSilver:      6,
	TierClassGeneral:     5,
	TierClassAssociate:   4,
	TierClassEndUser:     3,
	TierClassAcademic:    2,
	TierClassContributor: 1,
	TierClassOther:       0,
}

// TierClass collapses a raw membership tier product name into a normalized
// tier class. Unrecognized or empty names map to TierClassOther.
func TierClass(tierName string) string {
	name := strings.ToLower(tierName)
	for _, kc := range tierClassKeywords {
		if strings.Contains(name, kc.keyword) {
			return kc.class
		}
	}
	return TierClassOther
}

// TierClassRank returns the ordering rank of a normalized tier class; higher
// ranks win the per-organization selection.
func TierClassRank(class string) int {
	return tierClassRanks[class]
}
