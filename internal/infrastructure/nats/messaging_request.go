// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// userReader provides NATS RPC-based implementation of port.UserReader.
type userReader struct {
	client *NATSClient
}

// NewUserReader creates a new UserReader that resolves user information via
// NATS RPC calls to auth-service.
func NewUserReader(client *NATSClient) port.UserReader {
	return &userReader{client: client}
}

// UsernameByEmail resolves the registered LFID username for the given primary email address.
// The auth service replies with a plain-text username on success, or a JSON error envelope on miss.
func (u *userReader) UsernameByEmail(ctx context.Context, email string) (string, error) {
	msg, err := u.client.Conn().RequestWithContext(ctx, constants.AuthEmailToUsernameLookupSubject, []byte(email))
	if err != nil {
		return "", errors.NewNotFound(fmt.Sprintf("username not found for email: %s", email), err)
	}

	body := strings.TrimSpace(string(msg.Data))
	if body == "" {
		return "", errors.NewNotFound(fmt.Sprintf("username not found for email: %s", email))
	}

	// Auth-service error responses are JSON objects; success replies are plain-text usernames.
	if body[0] == '{' {
		var errorMessage ErrorMessageNATSResponse
		if err := errorMessage.CheckError(body); err != nil {
			return "", err
		}
		return "", errors.NewUnexpected(fmt.Sprintf("unexpected email_to_username success envelope: %s", body))
	}

	return body, nil
}

// UserMetadataByPrincipal resolves profile metadata via auth-service; an unsuccessful/absent body is a miss.
func (u *userReader) UserMetadataByPrincipal(ctx context.Context, principal string) (port.UserMetadata, error) {
	msg, err := u.client.Conn().RequestWithContext(ctx, constants.AuthUserMetadataReadSubject, []byte(principal))
	if err != nil {
		// Transport failure (no-responders/timeout) is unexpected, not a genuine miss — callers must be
		// able to tell an auth-service outage apart from "this user has no metadata".
		return port.UserMetadata{}, errors.NewUnexpected(fmt.Sprintf("user metadata lookup failed for principal: %s", redaction.Redact(principal)), err)
	}
	return parseUserMetadataResponse(principal, msg.Data)
}

// parseUserMetadataResponse parses the auth-service user_metadata reply body. Mirrors
// ErrorMessageNATSResponse.CheckError: malformed JSON or a non-"not found" error envelope → Unexpected
// (a real auth-service failure callers must tell apart from a miss); an absent/"not found" body →
// NotFound (a genuine miss); success+data → the denormalized subset.
func parseUserMetadataResponse(principal string, data []byte) (port.UserMetadata, error) {
	var response UserMetadataNATSResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return port.UserMetadata{}, errors.NewUnexpected("failed to parse user_metadata response", err)
	}
	if !response.Success || response.Data == nil {
		if response.Error != "" && !strings.Contains(response.Error, "not found") {
			return port.UserMetadata{}, errors.NewUnexpected(fmt.Sprintf("user metadata lookup failed for principal %s: %s", redaction.Redact(principal), response.Error))
		}
		return port.UserMetadata{}, errors.NewNotFound(fmt.Sprintf("user metadata not found for principal: %s", redaction.Redact(principal)))
	}

	meta := port.UserMetadata{}
	if response.Data.Picture != nil {
		meta.Picture = *response.Data.Picture
	}
	return meta, nil
}
