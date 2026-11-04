// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// FindOrCreate keeps stubLogoOrgWriter a complete svc.B2BOrgWriter.
func (w *stubLogoOrgWriter) FindOrCreate(_ context.Context, _, _ string) (*model.B2BOrg, model.AccountAction, error) {
	return w.org, model.AccountActionMatched, w.err
}

type stubAccountFinder struct {
	sfid   string
	action model.AccountAction
	err    error
	calls  []string
}

func (f *stubAccountFinder) FindOrCreateAccount(_ context.Context, name, website string) (string, model.AccountAction, error) {
	f.calls = append(f.calls, name+"|"+website)
	return f.sfid, f.action, f.err
}

// countingOrgWriter records the SFIDs registered through CreateB2BOrg.
type countingOrgWriter struct {
	seededOrgWriter
	sfids []string
}

func (w *countingOrgWriter) CreateB2BOrg(ctx context.Context, sfid string, input model.B2BOrgInput) (*model.B2BOrg, error) {
	w.sfids = append(w.sfids, sfid)
	return w.seededOrgWriter.CreateB2BOrg(ctx, sfid, input)
}

func TestB2BOrgWriter_FindOrCreate(t *testing.T) {
	const sfid = "001000000000001AAA"
	org := &model.B2BOrg{UID: sfid, Name: "Acme"}
	build := func(writer *countingOrgWriter, pub *capturingPublisher, finder *stubAccountFinder) svc.B2BOrgWriter {
		opts := []svc.B2BOrgWriterOption{
			svc.WithB2BOrgReader(&seededOrgReader{org: org}),
			svc.WithB2BOrgWriter(writer),
			svc.WithB2BOrgPublisher(pub),
			svc.WithGlobalOrgAdminTeamName("global_org_admin"),
		}
		if finder != nil {
			opts = append(opts, svc.WithAccountFindOrCreator(finder))
		}
		return svc.NewB2BOrgWriter(opts...)
	}

	t.Run("matched account is registered like Create", func(t *testing.T) {
		writer := &countingOrgWriter{seededOrgWriter: seededOrgWriter{createOrg: org}}
		pub := &capturingPublisher{}
		finder := &stubAccountFinder{sfid: sfid, action: model.AccountActionMatched}

		got, action, err := build(writer, pub, finder).FindOrCreate(context.Background(), "Acme", "https://acme.example")

		require.NoError(t, err)
		assert.Same(t, org, got)
		assert.Equal(t, model.AccountActionMatched, action)
		assert.Equal(t, []string{"Acme|https://acme.example"}, finder.calls)
		assert.Equal(t, []string{sfid}, writer.sfids)
		assert.Equal(t, 1, pub.indexerCallCount)
		assert.GreaterOrEqual(t, pub.accessCallCount, 1)
	})

	t.Run("created account reports created", func(t *testing.T) {
		writer := &countingOrgWriter{seededOrgWriter: seededOrgWriter{createOrg: org}}
		finder := &stubAccountFinder{sfid: sfid, action: model.AccountActionCreated}

		_, action, err := build(writer, &capturingPublisher{}, finder).FindOrCreate(context.Background(), "Acme", "https://acme.example")

		require.NoError(t, err)
		assert.Equal(t, model.AccountActionCreated, action)
		assert.Equal(t, []string{sfid}, writer.sfids)
	})

	t.Run("ambiguous domain registers and publishes nothing", func(t *testing.T) {
		writer := &countingOrgWriter{seededOrgWriter: seededOrgWriter{createOrg: org}}
		pub := &capturingPublisher{}
		finder := &stubAccountFinder{action: model.AccountActionAmbiguous, err: pkgerrors.NewConflict("several accounts match the website domain")}

		got, _, err := build(writer, pub, finder).FindOrCreate(context.Background(), "Acme", "https://acme.example")

		var conflict pkgerrors.Conflict
		require.True(t, errors.As(err, &conflict), "%v", err)
		assert.Nil(t, got)
		assert.Empty(t, writer.sfids)
		assert.Equal(t, 0, pub.indexerCallCount)
		assert.Equal(t, 0, pub.accessCallCount)
	})

	t.Run("not enabled answers service unavailable without any call", func(t *testing.T) {
		writer := &countingOrgWriter{seededOrgWriter: seededOrgWriter{createOrg: org}}

		got, _, err := build(writer, &capturingPublisher{}, nil).FindOrCreate(context.Background(), "Acme", "https://acme.example")

		var unavailable pkgerrors.ServiceUnavailable
		require.True(t, errors.As(err, &unavailable), "%v", err)
		assert.Nil(t, got)
		assert.Empty(t, writer.sfids)
	})

	t.Run("registration failure is returned and publishes nothing", func(t *testing.T) {
		writer := &countingOrgWriter{seededOrgWriter: seededOrgWriter{createErr: pkgerrors.NewUnprocessableEntity("account is not eligible as a b2b org")}}
		pub := &capturingPublisher{}
		finder := &stubAccountFinder{sfid: sfid, action: model.AccountActionCreated}

		got, _, err := build(writer, pub, finder).FindOrCreate(context.Background(), "Acme", "https://acme.example")

		var unprocessable pkgerrors.UnprocessableEntity
		require.True(t, errors.As(err, &unprocessable), "%v", err)
		assert.Nil(t, got)
		assert.Equal(t, []string{sfid}, writer.sfids)
		assert.Equal(t, 0, pub.indexerCallCount)
	})
}
