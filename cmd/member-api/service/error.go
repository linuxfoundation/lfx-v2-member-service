// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

func wrapError(ctx context.Context, err error) error {

	f := func(err error) error {
		switch e := err.(type) {
		case errors.Validation:
			return &membershipservice.BadRequestError{
				Message: e.Error(),
			}
		case errors.NotFound:
			return &membershipservice.NotFoundError{
				Message: e.Error(),
			}
		case errors.ServiceUnavailable:
			return &membershipservice.ServiceUnavailableError{
				Message: e.Error(),
			}
		default:
			return &membershipservice.InternalServerError{
				Message: e.Error(),
			}
		}
	}

	slog.ErrorContext(ctx, "request failed",
		"error", err,
	)
	return f(err)
}
