// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
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
	msg, err := requestWithSpan(ctx, u.client.Conn(), constants.AuthEmailToUsernameLookupSubject, []byte(email))
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
