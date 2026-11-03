// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	confirmOrgUID       = "001000000000051AAA"
	confirmParentUID    = "001000000000052AAA"
	confirmOldParentUID = "001000000000053AAA"
	confirmOtherOldUID  = "001000000000054AAA"
	confirmSiblingUID   = "001000000000055AAA"
)

// countingChildReader is a b2b_org reader whose child lists are configurable
// per parent and whose child reads are counted.
type countingChildReader struct {
	configurableB2BOrgReaderForRepair
	childReads []string
	childErrs  map[string]error
}

func (r *countingChildReader) FetchChildUIDsByParentUID(_ context.Context, parentUID string) ([]string, error) {
	r.childReads = append(r.childReads, parentUID)
	if err := r.childErrs[parentUID]; err != nil {
		return nil, err
	}
	return r.children[parentUID], nil
}

func newCountingChildReader(org *model.B2BOrg, children map[string][]string) *countingChildReader {
	return &countingChildReader{
		configurableB2BOrgReaderForRepair: configurableB2BOrgReaderForRepair{
			orgs:     map[string]*model.B2BOrg{org.UID: org},
			children: children,
		},
		childErrs: map[string]error{},
	}
}

func confirmRunner(reader *countingChildReader, pub *subjectCapturingPublisher, store *mock.MockCDCRepairStore, links *mock.MockB2BOrgChildLinkReader) *svc.Runner {
	opts := []svc.RunnerOption{
		svc.WithRepairStore(store),
		svc.WithQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}),
		svc.WithB2BOrgEligibilityReader(mock.MockB2BOrgEligibilityReader{}),
	}
	if links != nil {
		opts = append(opts, svc.WithChildLinkReader(links))
	}
	return svc.NewRunner(&mock.MockBackfillIterator{}, reader, mock.NewMockProjectMembershipReader(), nil, nil, pub, nil,
		"lf-org-admins", nil, opts...)
}

// accessKind names a captured b2b_org access message relative to orgUID:
// "grants" and "parent" are the org's own messages, "children:<P>" is a
// child-list replacement for parent P.
func accessKind(orgUID string, msg any) string {
	m, ok := msg.(fgatypes.GenericFGAMessage)
	if !ok {
		return "other"
	}
	data, ok := m.Data.(fgatypes.GenericAccessData)
	if !ok {
		return "other"
	}
	if data.UID != orgUID {
		return "children:" + data.UID
	}
	if _, ok := data.References["parent"]; ok {
		return "parent"
	}
	return "grants"
}

func childListsSent(pub *subjectCapturingPublisher, orgUID string) map[string][]string {
	out := map[string][]string{}
	for _, msg := range pub.accessMessages {
		if !strings.HasPrefix(accessKind(orgUID, msg), "children:") {
			continue
		}
		data := msg.(fgatypes.GenericFGAMessage).Data.(fgatypes.GenericAccessData)
		children := []string{}
		for _, ref := range data.References["child"] {
			children = append(children, strings.TrimPrefix(ref, "b2b_org:"))
		}
		sort.Strings(children)
		out[data.UID] = children
	}
	return out
}

func repairOrg(t *testing.T, runner *svc.Runner) {
	t.Helper()
	runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: "b2b_org", CDCRepair: true},
		repairMarkers("b2b_org", confirmOrgUID))
}

// TestBackfillRunner_RunRepair_B2BOrg_PublishFailureKeepsMarker: each of an
// org's messages must be accepted, and the flush confirmed, before its marker
// is cleared.
func TestBackfillRunner_RunRepair_B2BOrg_PublishFailureKeepsMarker(t *testing.T) {
	org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
	children := map[string][]string{confirmParentUID: {confirmOrgUID}}
	failAccess := func(kind string) func(string, any) error {
		return func(_ string, msg any) error {
			if accessKind(confirmOrgUID, msg) == kind {
				return errors.New("nats unavailable")
			}
			return nil
		}
	}

	cases := map[string]*subjectCapturingPublisher{
		"index publish fails":       {indexerErr: errors.New("nats unavailable")},
		"staff grant publish fails": {beforeAccess: failAccess("grants")},
		"parent link publish fails": {beforeAccess: failAccess("parent")},
		"flush fails":               {flushErr: errors.New("flush timeout")},
	}
	for name, pub := range cases {
		t.Run(name, func(t *testing.T) {
			store := &mock.MockCDCRepairStore{}
			repairOrg(t, confirmRunner(newCountingChildReader(org, children), pub, store, mock.NewMockB2BOrgChildLinkReader()))

			assert.Empty(t, store.Deletes, "the marker is kept for the next drain")
		})
	}

	t.Run("all published and confirmed clears the marker", func(t *testing.T) {
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}
		repairOrg(t, confirmRunner(newCountingChildReader(org, children), pub, store, mock.NewMockB2BOrgChildLinkReader()))

		require.Len(t, store.Deletes, 1)
		assert.Equal(t, 1, pub.flushCount, "one flush confirms the item")
	})
}

func TestBackfillRunner_RunRepair_MembershipAndKeyContact_PublishFailureKeepsMarker(t *testing.T) {
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: "resolved-uid", Slug: "my-project"})
	pm := &model.ProjectMembership{UID: "02i000000000051AAA", ProjectSlug: "my-project", TierFamily: "Membership"}
	kc := &model.KeyContact{UID: "003000000000051AAA", ProjectSlug: "my-project", MembershipUID: "pm-1", Username: "kc-user"}

	run := func(t *testing.T, reindexType, uid string, pub *subjectCapturingPublisher) *mock.MockCDCRepairStore {
		t.Helper()
		store := &mock.MockCDCRepairStore{}
		runner := svc.NewRunner(&mock.MockBackfillIterator{}, mock.NewMockB2BOrgReader(), &seededPMReaderForResolver{pm: pm},
			&seededKCReaderForResolver{kc: kc}, nil, pub, nil, "", resolver,
			svc.WithRepairStore(store), svc.WithQuotaGauge(&mock.MockSalesforceQuotaGauge{Current: 10, Limit: 100}),
			svc.WithB2BOrgEligibilityReader(mock.MockB2BOrgEligibilityReader{}))
		runner.RunRepair(context.Background(), svc.BackfillRequest{RunID: "r", Type: reindexType}, repairMarkers(reindexType, uid))
		return store
	}

	for _, tc := range []struct{ reindexType, uid string }{
		{"project_membership", pm.UID},
		{"key_contact", kc.UID},
	} {
		t.Run(tc.reindexType+"/index publish fails", func(t *testing.T) {
			assert.Empty(t, run(t, tc.reindexType, tc.uid, &subjectCapturingPublisher{indexerErr: assert.AnError}).Deletes)
		})
		t.Run(tc.reindexType+"/access publish fails", func(t *testing.T) {
			assert.Empty(t, run(t, tc.reindexType, tc.uid, &subjectCapturingPublisher{accessErr: assert.AnError}).Deletes)
		})
		t.Run(tc.reindexType+"/flush fails", func(t *testing.T) {
			assert.Empty(t, run(t, tc.reindexType, tc.uid, &subjectCapturingPublisher{flushErr: assert.AnError}).Deletes)
		})
		t.Run(tc.reindexType+"/confirmed clears the marker", func(t *testing.T) {
			pub := &subjectCapturingPublisher{}
			assert.Len(t, run(t, tc.reindexType, tc.uid, pub).Deletes, 1)
			assert.Equal(t, 1, pub.flushCount)
		})
	}
}

func TestBackfillRunner_RunRepair_Settings_PublishFailureKeepsMarker(t *testing.T) {
	uid := "001000000000056AAA"
	for name, pub := range map[string]*subjectCapturingPublisher{
		"index publish fails": {indexerErr: assert.AnError},
		"flush fails":         {flushErr: assert.AnError},
	} {
		t.Run(name, func(t *testing.T) {
			store := &mock.MockCDCRepairStore{}
			settingsRunner(&pageEligibility{}, pub, store, uid).RunRepair(context.Background(),
				svc.BackfillRequest{RunID: "r", Type: "b2b_org_settings"}, repairMarkers("b2b_org_settings", uid))
			assert.Empty(t, store.Deletes)
		})
	}
}

// TestBackfillRunner_Targeted_PublishFailureNotCounted: a per-item targeted
// reindex leaves an unconfirmed item out of the published count, and a dry
// run neither publishes nor flushes.
func TestBackfillRunner_Targeted_PublishFailureNotCounted(t *testing.T) {
	org := &model.B2BOrg{UID: confirmOrgUID}

	t.Run("failed flush publishes no confirmation", func(t *testing.T) {
		pub := &subjectCapturingPublisher{flushErr: assert.AnError}
		store := &mock.MockCDCRepairStore{}
		require.NoError(t, confirmRunner(newCountingChildReader(org, nil), pub, store, nil).Run(context.Background(),
			svc.BackfillRequest{RunID: "r", Type: "b2b_org", Items: []string{org.UID}}))
		assert.Equal(t, 1, pub.flushCount)
		assert.Empty(t, store.Deletes, "targeted runs never touch markers")
	})

	t.Run("dry run publishes and flushes nothing", func(t *testing.T) {
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID)
		confirmRunner(newCountingChildReader(org, nil), pub, store, links).RunRepair(context.Background(),
			svc.BackfillRequest{RunID: "r", Type: "b2b_org", DryRun: true, CDCRepair: true}, repairMarkers("b2b_org", org.UID))

		assert.Empty(t, pub.indexerMessages)
		assert.Empty(t, pub.accessMessages)
		assert.Zero(t, pub.flushCount)
		assert.Equal(t, []string{org.UID}, links.Calls(), "a dry run still exercises the read")
	})
}

// TestBackfillRunner_RunRepair_B2BOrg_StaleParents: every org the access model
// lists the repaired org under, other than its Salesforce parent, gets its
// child list replaced from Salesforce.
func TestBackfillRunner_RunRepair_B2BOrg_StaleParents(t *testing.T) {
	t.Run("move from A to B rebuilds A without the org", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		reader := newCountingChildReader(org, map[string][]string{
			confirmParentUID:    {confirmOrgUID},
			confirmOldParentUID: {confirmSiblingUID},
		})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID, confirmParentUID)
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(reader, pub, store, links))

		assert.Equal(t, map[string][]string{
			confirmParentUID:    {confirmOrgUID},
			confirmOldParentUID: {confirmSiblingUID},
		}, childListsSent(pub, org.UID))
		assert.Len(t, store.Deletes, 1)
	})

	t.Run("move to no parent rebuilds the old parent", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID}
		reader := newCountingChildReader(org, map[string][]string{confirmOldParentUID: {confirmSiblingUID}})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID)
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(reader, pub, store, links))

		assert.Equal(t, map[string][]string{confirmOldParentUID: {confirmSiblingUID}}, childListsSent(pub, org.UID))
		assert.Equal(t, 1, parentClearsFor(t, pub, org.UID), "the org's own parent tuple is withdrawn too")
		assert.Len(t, store.Deletes, 1)
	})

	t.Run("a drain after the withdrawal landed still clears the parent", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID}
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(newCountingChildReader(org, nil), pub, store, mock.NewMockB2BOrgChildLinkReader()))

		assert.Equal(t, 1, parentClearsFor(t, pub, org.UID))
		assert.Len(t, store.Deletes, 1)
	})

	t.Run("an uncheckable read still publishes the org and clears the marker", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		reader := newCountingChildReader(org, map[string][]string{confirmParentUID: {confirmOrgUID}})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetError(org.UID, errors.Join(port.ErrChildLinksUncheckable, errors.New("reply exceeded cap")))
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(reader, pub, store, links))

		assert.NotEmpty(t, pub.indexerMessages)
		assert.Equal(t, map[string][]string{confirmParentUID: {confirmOrgUID}}, childListsSent(pub, org.UID),
			"only the current parent's list; nothing is withdrawn")
		assert.Len(t, store.Deletes, 1, "retrying cannot complete the read, so the marker is not held")
	})

	t.Run("a failed parent clear keeps the marker", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID}
		reader := newCountingChildReader(org, map[string][]string{confirmOldParentUID: {confirmSiblingUID}})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID)
		store := &mock.MockCDCRepairStore{}
		pub := &subjectCapturingPublisher{beforeAccess: func(_ string, msg any) error {
			if m, ok := msg.(fgatypes.GenericFGAMessage); ok {
				if data, ok := m.Data.(fgatypes.GenericAccessData); ok && data.UID == org.UID && len(data.References) == 0 {
					return errors.New("nats unavailable")
				}
			}
			return nil
		}}

		repairOrg(t, confirmRunner(reader, pub, store, links))

		assert.Empty(t, store.Deletes)
	})

	t.Run("two old parents are both rebuilt, a deleted one with no children", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		reader := newCountingChildReader(org, map[string][]string{
			confirmParentUID:    {confirmOrgUID},
			confirmOldParentUID: {confirmSiblingUID},
		})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID, confirmOtherOldUID)
		pub := &subjectCapturingPublisher{}

		repairOrg(t, confirmRunner(reader, pub, &mock.MockCDCRepairStore{}, links))

		sent := childListsSent(pub, org.UID)
		assert.Equal(t, []string{confirmSiblingUID}, sent[confirmOldParentUID])
		assert.Contains(t, sent, confirmOtherOldUID)
		assert.Empty(t, sent[confirmOtherOldUID], "a parent with no Salesforce children is sent an empty list")
	})

	t.Run("listed only under the current parent costs no extra child read", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		reader := newCountingChildReader(org, map[string][]string{confirmParentUID: {confirmOrgUID}})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmParentUID)
		pub := &subjectCapturingPublisher{}

		repairOrg(t, confirmRunner(reader, pub, &mock.MockCDCRepairStore{}, links))

		assert.Equal(t, []string{confirmOrgUID, confirmParentUID}, reader.childReads, "only the reads made before this change")
		assert.Equal(t, map[string][]string{confirmParentUID: {confirmOrgUID}}, childListsSent(pub, org.UID))
	})

	t.Run("access model read failure keeps the marker and sends nothing", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetError(org.UID, errors.New("fga-sync unavailable"))
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(newCountingChildReader(org, nil), pub, store, links))

		assert.Empty(t, pub.indexerMessages)
		assert.Empty(t, pub.accessMessages)
		assert.Empty(t, store.Deletes)
	})

	t.Run("stale parent child read failure keeps the marker and sends nothing", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		reader := newCountingChildReader(org, map[string][]string{confirmParentUID: {confirmOrgUID}})
		reader.childErrs[confirmOldParentUID] = errors.New("soql timeout")
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID)
		pub, store := &subjectCapturingPublisher{}, &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(reader, pub, store, links))

		assert.Empty(t, pub.accessMessages)
		assert.Empty(t, store.Deletes)
	})

	t.Run("failed child list publish keeps the marker", func(t *testing.T) {
		org := &model.B2BOrg{UID: confirmOrgUID, ParentUID: confirmParentUID}
		reader := newCountingChildReader(org, map[string][]string{confirmParentUID: {confirmOrgUID}})
		links := mock.NewMockB2BOrgChildLinkReader()
		links.SetParents(org.UID, confirmOldParentUID)
		pub := &subjectCapturingPublisher{beforeAccess: func(_ string, msg any) error {
			if accessKind(confirmOrgUID, msg) == "children:"+confirmOldParentUID {
				return errors.New("nats unavailable")
			}
			return nil
		}}
		store := &mock.MockCDCRepairStore{}

		repairOrg(t, confirmRunner(reader, pub, store, links))

		assert.Empty(t, store.Deletes)
	})
}
