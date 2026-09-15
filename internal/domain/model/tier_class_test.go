// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

func TestTierClass(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Platinum Membership", TierClassPlatinum},
		{"Premier Membership", TierClassPremier},
		{"Founding Member", TierClassFounding},
		{"Strategic Membership", TierClassStrategic},
		{"Gold Corporate Membership", TierClassGold},
		{"Steering Membership", TierClassSteering},
		{"SILVER MEMBERSHIP - ANNUAL", TierClassSilver},
		{"General Membership", TierClassGeneral},
		{"Associate Membership", TierClassAssociate},
		{"End User Supporter", TierClassEndUser},
		{"Academic Membership", TierClassAcademic},
		{"Contributor Membership", TierClassContributor},
		// A name matching several keywords resolves to the highest-ranked
		// class, matching the dbt model's first-match-in-rank-order CASE.
		{"Premier Sponsor (General)", TierClassPremier},
		{"Lab Membership", TierClassOther},
		{"", TierClassOther},
	}
	for _, c := range cases {
		if got := TierClass(c.name); got != c.want {
			t.Errorf("TierClass(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTierClassRankOrdering(t *testing.T) {
	// Lowest to highest, mirroring the dbt model's rank order reversed.
	order := []string{
		TierClassOther,
		TierClassContributor,
		TierClassAcademic,
		TierClassEndUser,
		TierClassAssociate,
		TierClassGeneral,
		TierClassSilver,
		TierClassSteering,
		TierClassGold,
		TierClassStrategic,
		TierClassFounding,
		TierClassPremier,
		TierClassPlatinum,
	}
	for i := 1; i < len(order); i++ {
		if TierClassRank(order[i]) <= TierClassRank(order[i-1]) {
			t.Errorf("TierClassRank(%q) must outrank TierClassRank(%q)", order[i], order[i-1])
		}
	}
}

// The class strings are wire values in the public member-tiers response, and
// the Insights API layer collapses them further downstream; renaming one is
// an API break, so pin every value. The length check catches a class added
// to the rank table without being pinned here.
func TestTierClassWireValues(t *testing.T) {
	pinned := map[string]string{
		TierClassPlatinum:    "platinum",
		TierClassPremier:     "premier",
		TierClassFounding:    "founding",
		TierClassStrategic:   "strategic",
		TierClassGold:        "gold",
		TierClassSteering:    "steering",
		TierClassSilver:      "silver",
		TierClassGeneral:     "general",
		TierClassAssociate:   "associate",
		TierClassEndUser:     "end_user",
		TierClassAcademic:    "academic",
		TierClassContributor: "contributor",
		TierClassOther:       "other",
	}
	if len(tierClassRanks) != len(pinned) {
		t.Errorf("rank table has %d classes but %d are pinned; pin new classes here", len(tierClassRanks), len(pinned))
	}
	for class, want := range pinned {
		if class != want {
			t.Errorf("tier class wire value changed: got %q, want %q", class, want)
		}
		if _, ok := tierClassRanks[class]; !ok {
			t.Errorf("pinned class %q has no rank", class)
		}
	}
}

// Every keyword-mapped class must have a rank, and the keyword table must be
// sorted by strictly descending rank, because classification is first-match
// and relies on higher classes being checked first.
func TestTierClassKeywordsSortedByRank(t *testing.T) {
	prev := -1
	for i, kc := range tierClassKeywords {
		rank, ok := tierClassRanks[kc.class]
		if !ok {
			t.Fatalf("keyword %q maps to class %q with no rank", kc.keyword, kc.class)
		}
		if prev != -1 && rank >= prev {
			t.Errorf("keyword table out of rank order at index %d (%q)", i, kc.keyword)
		}
		prev = rank
	}
}
