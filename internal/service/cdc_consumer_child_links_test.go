// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// childListsFor returns every child list sent for parentUID, in order.
func childListsFor(t *testing.T, pub *subjectCapturingPublisher, parentUID string) [][]string {
	t.Helper()
	var lists [][]string
	for _, m := range pub.fgaMessages(t) {
		data, ok := m.Data.(fgatypes.GenericAccessData)
		if m.ObjectType != "b2b_org" || !ok || data.UID != parentUID {
			continue
		}
		lists = append(lists, append([]string{}, data.References["child"]...))
	}
	return lists
}

// parentClearsFor counts the update_access messages that replace orgUID's own
// parent relation with nothing, leaving every other relation alone.
func parentClearsFor(t *testing.T, pub *subjectCapturingPublisher, orgUID string) int {
	t.Helper()
	n := 0
	for _, m := range pub.fgaMessages(t) {
		data, ok := m.Data.(fgatypes.GenericAccessData)
		if m.ObjectType != "b2b_org" || !ok || data.UID != orgUID || len(data.References) != 0 {
			continue
		}
		if slices.Contains(data.ExcludeRelations, "child") && !slices.Contains(data.ExcludeRelations, "parent") {
			n++
		}
	}
	return n
}

func runAccountEvent(t *testing.T, changeType model.CDCChangeType, ids []string, reader *fakeB2BOrgReader,
	accounts map[string]*model.B2BOrg, pub *subjectCapturingPublisher, opts ...svc.CDCConsumerOption) {
	t.Helper()
	opts = append(opts, svc.WithCDCAccountBatchReader(&recordingAccountBatch{orgs: accounts}))
	consumer := newTestCDCConsumer(
		&fakeCDCSubscriber{events: []model.CDCEvent{{Entity: "Account", ChangeType: changeType, RecordIDs: ids, ReplayID: []byte("links")}}},
		reader, &mock.MockCacheInvalidator{}, pub, "", opts...)
	require.NoError(t, consumer.Run(context.Background(), "/data/AccountChangeEvent", &fakeReplayStore{}))
}

// TestCDCConsumer_Account_ColdCacheMove_WithdrawsOldParentLink: with no
// earlier copy cached, the previous-record diff cannot see the old parent, so
// the access model's child link is what finds it. Its child list comes from
// the batch child read, adding no Salesforce call.
func TestCDCConsumer_Account_ColdCacheMove_WithdrawsOldParentLink(t *testing.T) {
	org, oldParent, newParent, sibling := sfid("cold-org"), sfid("cold-old"), sfid("cold-new"), sfid("cold-sib")
	moved := &model.B2BOrg{UID: org, ParentUID: newParent}
	reader := &fakeB2BOrgReader{
		org:      moved, // cold cache: the "previous" read already returns the new record
		childMap: map[string][]string{oldParent: {sibling}, newParent: {org}},
	}
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, oldParent)
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUpdate, []string{org}, reader, map[string]*model.B2BOrg{org: moved}, pub,
		svc.WithCDCChildLinkReader(links))

	assert.Equal(t, [][]string{{"b2b_org:" + sibling}}, childListsFor(t, pub, oldParent))
	assert.Equal(t, []string{org}, links.Calls())
	assert.Zero(t, reader.childCallCount.Load(), "no single-parent Salesforce read")
	assert.Equal(t, int32(1), reader.batchCallCount.Load(), "stale parents ride the existing batch read")
	assert.Zero(t, parentClearsFor(t, pub, org), "the new parent message already replaces the org's parent")
}

// TestCDCConsumer_Account_ColdCacheMoveToNoParent_ClearsOwnParent: with no new
// parent, nothing else replaces the org's own parent tuple, which would keep
// granting the old parent's writers and auditors access to the org.
func TestCDCConsumer_Account_ColdCacheMoveToNoParent_ClearsOwnParent(t *testing.T) {
	org, oldParent, sibling := sfid("np-org"), sfid("np-old"), sfid("np-sib")
	moved := &model.B2BOrg{UID: org}
	reader := &fakeB2BOrgReader{org: moved, childMap: map[string][]string{oldParent: {sibling}}}
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, oldParent)
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUpdate, []string{org}, reader, map[string]*model.B2BOrg{org: moved}, pub,
		svc.WithCDCChildLinkReader(links))

	assert.Equal(t, [][]string{{"b2b_org:" + sibling}}, childListsFor(t, pub, oldParent))
	assert.Equal(t, 1, parentClearsFor(t, pub, org))
}

// TestCDCConsumer_Account_WarmCacheMove_OldParentReadFails_StillWithdrawn: the
// previous-record diff skips the old parent when its child read fails and
// queues nothing, so the access-model read must still rebuild it.
func TestCDCConsumer_Account_WarmCacheMove_OldParentReadFails_StillWithdrawn(t *testing.T) {
	org, oldParent, newParent, sibling := sfid("warm-org"), sfid("warm-old"), sfid("warm-new"), sfid("warm-sib")
	reader := &fakeB2BOrgReader{
		org:      &model.B2BOrg{UID: org, ParentUID: oldParent},
		childErr: errors.New("soql timeout"),
		childMap: map[string][]string{oldParent: {sibling}, newParent: {org}},
	}
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, oldParent)
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUpdate, []string{org}, reader,
		map[string]*model.B2BOrg{org: {UID: org, ParentUID: newParent}}, pub, svc.WithCDCChildLinkReader(links))

	assert.Equal(t, [][]string{{"b2b_org:" + sibling}}, childListsFor(t, pub, oldParent))
}

// TestCDCConsumer_Account_NotAMove_SendsTodaysMessages: an org listed only
// under its current parent sends exactly what it sent before the read existed.
func TestCDCConsumer_Account_NotAMove_SendsTodaysMessages(t *testing.T) {
	org, parent := sfid("stay-org"), sfid("stay-parent")
	current := &model.B2BOrg{UID: org, ParentUID: parent}
	run := func(opts ...svc.CDCConsumerOption) *subjectCapturingPublisher {
		pub := &subjectCapturingPublisher{}
		runAccountEvent(t, model.CDCChangeUpdate, []string{org},
			&fakeB2BOrgReader{org: current, childMap: map[string][]string{parent: {org}}},
			map[string]*model.B2BOrg{org: current}, pub, opts...)
		return pub
	}
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, parent)

	without, with := run(), run(svc.WithCDCChildLinkReader(links))

	assert.Equal(t, without.fgaMessages(t), with.fgaMessages(t))
	assert.Equal(t, without.indexer, with.indexer)
	assert.Equal(t, []string{org}, links.Calls(), "one access-model read per changed org")
}

// TestCDCConsumer_Account_ChildLinkReadFails_QueuesUncheckedOrgs: an fga-sync
// outage costs one failed read per batch. Every org still publishes its own
// updates, the stream advances, and the unchecked orgs go to the repair run.
func TestCDCConsumer_Account_ChildLinkReadFails_QueuesUncheckedOrgs(t *testing.T) {
	first, second := sfid("fail-first"), sfid("fail-second")
	accounts := map[string]*model.B2BOrg{first: {UID: first}, second: {UID: second}}
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetError(first, errors.New("fga-sync unavailable"))
	links.SetError(second, errors.New("fga-sync unavailable"))
	repair := &mock.MockCDCRepairStore{}
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUpdate, []string{first, second}, &fakeB2BOrgReader{orgErr: errors.New("cold")},
		accounts, pub, svc.WithCDCChildLinkReader(links), svc.WithCDCRepairStore(repair))

	assert.Len(t, links.Calls(), 1, "reads stop at the first failure")
	assert.Equal(t, 2, pub.indexerCount("lfx.index.b2b_org"), "both orgs still publish their own updates")
	assert.ElementsMatch(t, []string{first, second}, repairPutsOfType(repair, "b2b_org"))
}

// TestCDCConsumer_Account_UncheckableOrg_KeepsCheckingTheBatch: a read that
// can never complete is not an outage, so it neither stops the batch nor
// queues a marker that retrying could not resolve.
func TestCDCConsumer_Account_UncheckableOrg_KeepsCheckingTheBatch(t *testing.T) {
	big, moved, oldParent, sibling := sfid("uc-big"), sfid("uc-moved"), sfid("uc-old"), sfid("uc-sib")
	accounts := map[string]*model.B2BOrg{big: {UID: big}, moved: {UID: moved}}
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetError(big, errors.Join(port.ErrChildLinksUncheckable, errors.New("reply exceeded cap")))
	links.SetParents(moved, oldParent)
	repair := &mock.MockCDCRepairStore{}
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUpdate, []string{big, moved},
		&fakeB2BOrgReader{orgErr: errors.New("cold"), childMap: map[string][]string{oldParent: {sibling}}},
		accounts, pub, svc.WithCDCChildLinkReader(links), svc.WithCDCRepairStore(repair))

	assert.ElementsMatch(t, []string{big, moved}, links.Calls())
	assert.Equal(t, [][]string{{"b2b_org:" + sibling}}, childListsFor(t, pub, oldParent))
	assert.Empty(t, repairPutsOfType(repair, "b2b_org"))
}

// TestCDCConsumer_Account_ChildQueryFails_SendsNoStaleParentList: without a
// complete child read nothing is withdrawn; the org is queued instead.
func TestCDCConsumer_Account_ChildQueryFails_SendsNoStaleParentList(t *testing.T) {
	org, oldParent := sfid("cq-org"), sfid("cq-old")
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, oldParent)
	repair := &mock.MockCDCRepairStore{}
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUpdate, []string{org},
		&fakeB2BOrgReader{org: &model.B2BOrg{UID: org}, batchErr: errors.New("soql timeout")},
		map[string]*model.B2BOrg{org: {UID: org}}, pub, svc.WithCDCChildLinkReader(links), svc.WithCDCRepairStore(repair))

	assert.Empty(t, childListsFor(t, pub, oldParent))
	assert.Contains(t, repairPutsOfType(repair, "b2b_org"), org)
}

// TestCDCConsumer_Account_StaleParentPublishFails_QueuesOrg: a lost withdrawal
// would leave the access leak in place, so the org goes to the repair run.
func TestCDCConsumer_Account_StaleParentPublishFails_QueuesOrg(t *testing.T) {
	org, oldParent := sfid("pf-org"), sfid("pf-old")
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, oldParent)
	repair := &mock.MockCDCRepairStore{}
	pub := &subjectCapturingPublisher{beforeAccess: func(_ string, msg any) error {
		if m, ok := msg.(fgatypes.GenericFGAMessage); ok {
			if data, ok := m.Data.(fgatypes.GenericAccessData); ok && data.UID == oldParent {
				return errors.New("nats unavailable")
			}
		}
		return nil
	}}

	runAccountEvent(t, model.CDCChangeUpdate, []string{org}, &fakeB2BOrgReader{org: &model.B2BOrg{UID: org}},
		map[string]*model.B2BOrg{org: {UID: org}}, pub, svc.WithCDCChildLinkReader(links), svc.WithCDCRepairStore(repair))

	assert.Equal(t, []string{org}, repairPutsOfType(repair, "b2b_org"))
}

// TestCDCConsumer_Account_Restore_ReadsChildLinks: a restored org can carry a
// stale parent link like any other change.
func TestCDCConsumer_Account_Restore_ReadsChildLinks(t *testing.T) {
	org, oldParent := sfid("rs-org"), sfid("rs-old")
	links := mock.NewMockB2BOrgChildLinkReader()
	links.SetParents(org, oldParent)
	pub := &subjectCapturingPublisher{}

	runAccountEvent(t, model.CDCChangeUndelete, []string{org}, &fakeB2BOrgReader{org: &model.B2BOrg{UID: org}},
		map[string]*model.B2BOrg{org: {UID: org}}, pub, svc.WithCDCChildLinkReader(links))

	assert.Equal(t, []string{org}, links.Calls())
	assert.Len(t, childListsFor(t, pub, oldParent), 1)
}
