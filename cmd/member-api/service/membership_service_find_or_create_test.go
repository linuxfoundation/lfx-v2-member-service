// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// FindOrCreate keeps stubB2BOrgWriterUC a complete usecaseSvc.B2BOrgWriter.
func (s stubB2BOrgWriterUC) FindOrCreate(_ context.Context, _, _ string) (*model.B2BOrg, model.AccountAction, error) {
	return s.org, model.AccountActionMatched, s.err
}

type stubFindOrCreateWriterUC struct {
	stubB2BOrgWriterUC
	action model.AccountAction
	calls  []string
}

func (s *stubFindOrCreateWriterUC) FindOrCreate(_ context.Context, name, website string) (*model.B2BOrg, model.AccountAction, error) {
	s.calls = append(s.calls, name+"|"+website)
	return s.org, s.action, s.err
}

func TestFindOrCreateB2bOrg(t *testing.T) {
	payload := &membershipservice.FindOrCreateB2bOrgPayload{Name: "Linux Foundation", Website: "https://linuxfoundation.org"}

	t.Run("an org the etag cannot be computed for fails instead of leaving the header pointers nil", func(t *testing.T) {
		uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{org: &model.B2BOrg{UID: "001410000FindOrAQI", Name: "Linux Foundation", UpdatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}, action: model.AccountActionMatched}
		result, err := newTestSvc(withB2BOrgWriterUC(uc)).FindOrCreateB2bOrg(context.Background(), payload)
		require.Error(t, err)
		assert.Nil(t, result)
		var serviceErr *goa.ServiceError
		require.True(t, errors.As(err, &serviceErr), "%T: %v", err, err)
		assert.Equal(t, "InternalServerError", serviceErr.Name)
	})

	for _, action := range []model.AccountAction{model.AccountActionCreated, model.AccountActionMatched} {
		t.Run(string(action), func(t *testing.T) {
			uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{org: sampleB2BOrg}, action: action}
			svc := newTestSvc(withB2BOrgWriterUC(uc))

			result, err := svc.FindOrCreateB2bOrg(context.Background(), payload)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.B2bOrg)
			assert.Equal(t, "lf-uid-001", *result.B2bOrg.UID)
			assert.Equal(t, string(action), result.Action)
			assert.NotNil(t, result.Etag, "ETag must be set")
			assert.NotNil(t, result.LastModified, "Last-Modified must be set")
			assert.Equal(t, []string{"Linux Foundation|https://linuxfoundation.org"}, uc.calls)
		})
	}

	errorCases := map[string]struct {
		err  error
		want string
	}{
		"disabled":     {pkgerrors.NewServiceUnavailable("b2b org find-or-create is not enabled"), "ServiceUnavailable"},
		"ambiguous":    {pkgerrors.NewConflict("several accounts match the website domain"), "Conflict"},
		"not eligible": {pkgerrors.NewUnprocessableEntity("account is not eligible as a b2b org"), "UnprocessableEntity"},
		"mock source":  {pkgerrors.NewNotImplemented("find-or-create-b2b-org not implemented in mock"), "NotImplemented"},
		"apex failure": {errors.New("apex account find-or-create: status 500"), "InternalServerError"},
		"bad input":    {pkgerrors.NewValidation("name and website are required"), "BadRequest"},
	}
	for name, tc := range errorCases {
		t.Run(name, func(t *testing.T) {
			uc := &stubFindOrCreateWriterUC{stubB2BOrgWriterUC: stubB2BOrgWriterUC{err: tc.err}}
			svc := newTestSvc(withB2BOrgWriterUC(uc))

			result, err := svc.FindOrCreateB2bOrg(context.Background(), payload)

			require.Error(t, err)
			assert.Nil(t, result)
			var serviceErr *goa.ServiceError
			require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
			assert.Equal(t, tc.want, serviceErr.Name)
		})
	}
}
