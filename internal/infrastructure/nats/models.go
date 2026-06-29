// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"encoding/json"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// ErrorMessageNATSResponse is a JSON response body that may contain an error
// message from an auth-service NATS RPC. If Success is false, callers should
// check the Error field for details.
type ErrorMessageNATSResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// CheckError unmarshals the given JSON message into e and returns an error if
// Success is false. Returns nil if the message is not JSON or if Success is true.
func (e *ErrorMessageNATSResponse) CheckError(message string) error {
	if errUnmarshal := json.Unmarshal([]byte(message), e); errUnmarshal == nil {
		if !e.Success {
			if isUserMissError(e.Error) {
				return errors.NewNotFound(e.Error)
			}
			return errors.NewUnexpected(e.Error, nil)
		}
	}
	return nil
}

// isUserMissError reports whether an auth-service error envelope denotes a genuine "no such user"
// miss (so callers skip the principal) rather than a transient failure they must surface/retry.
// Auth-service phrases the miss differently per lookup path: a username search returns "user not
// found", while an auth0| sub get-by-id returns "The user does not exist."; both mean the principal
// has no resolvable account. Transient errors (e.g. Auth0 "too_many"/rate limits) match neither.
func isUserMissError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist")
}

// UserMetadataNATSResponse is the reply envelope from lfx.auth-service.user_metadata.read.
type UserMetadataNATSResponse struct {
	Success bool                      `json:"success"`
	Error   string                    `json:"error,omitempty"`
	Data    *UserMetadataNATSDataBody `json:"data,omitempty"`
}

// UserMetadataNATSDataBody holds the profile fields the member-service consumes from the
// auth-service user_metadata response. Only the denormalized subset is parsed.
type UserMetadataNATSDataBody struct {
	Picture *string `json:"picture,omitempty"`
}
