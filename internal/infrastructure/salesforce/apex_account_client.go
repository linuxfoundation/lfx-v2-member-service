// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// AccountFindOrCreateSource is the AccountSource stamped on Accounts the Apex endpoint creates.
const AccountFindOrCreateSource = "EasyCLA"

// ApexAccountClient implements port.AccountFindOrCreator against the LF
// Salesforce Apex REST endpoint (LF_SF_CICD_Setup LFXAccountRest): POST
// {path} with {name, website, source, dryRun} answers {id, action}.
type ApexAccountClient struct {
	client *SObjectClient
	path   string
}

// NewApexAccountClient creates an ApexAccountClient; path is the Apex REST
// path, e.g. /services/apexrest/lfx/account.
func NewApexAccountClient(client *SObjectClient, path string) *ApexAccountClient {
	return &ApexAccountClient{client: client, path: path}
}

// Ensure ApexAccountClient satisfies the port at compile time.
var _ port.AccountFindOrCreator = (*ApexAccountClient)(nil)

type apexAccountRequest struct {
	Name    string `json:"name"`
	Website string `json:"website"`
	Source  string `json:"source"`
	DryRun  bool   `json:"dryRun"`
}

type apexAccountResponse struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	Message string `json:"message"`
}

// FindOrCreateAccount implements port.AccountFindOrCreator.
func (c *ApexAccountClient) FindOrCreateAccount(ctx context.Context, name, website string) (string, model.AccountAction, error) {
	name, website = strings.TrimSpace(name), strings.TrimSpace(website)
	if name == "" || website == "" {
		return "", "", errs.NewValidation("name and website are required")
	}
	body, err := json.Marshal(apexAccountRequest{Name: name, Website: website, Source: AccountFindOrCreateSource})
	if err != nil {
		return "", "", fmt.Errorf("marshalling apex account request: %w", err)
	}
	status, raw, err := c.post(ctx, body)
	if err != nil {
		return "", "", err
	}
	switch {
	case status >= 200 && status < 300:
	case status == http.StatusBadRequest:
		slog.WarnContext(ctx, "apex account find-or-create: request rejected", "status", status, "message", apexMessage(raw, status))
		return "", "", errs.NewValidation("salesforce rejected the account name or website")
	case status == http.StatusConflict:
		slog.InfoContext(ctx, "apex account find-or-create: ambiguous domain", "website", website, "message", apexMessage(raw, status))
		return "", model.AccountActionAmbiguous, errs.NewConflict("several accounts match the website domain")
	case status == http.StatusUnprocessableEntity:
		slog.WarnContext(ctx, "apex account find-or-create: account not creatable", "status", status, "message", apexMessage(raw, status))
		return "", "", errs.NewUnprocessableEntity("the account cannot be created in salesforce")
	default:
		return "", "", fmt.Errorf("apex account find-or-create: status %d: %s", status, snippet(raw))
	}
	var out apexAccountResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("apex account find-or-create: decoding response: %w", err)
	}
	action := model.AccountAction(out.Action)
	switch action {
	case model.AccountActionAmbiguous:
		slog.InfoContext(ctx, "apex account find-or-create: ambiguous domain", "website", website, "message", out.Message)
		return "", action, errs.NewConflict("several accounts match the website domain")
	case model.AccountActionMatched, model.AccountActionCreated:
		sfid, err := sfuuid.Normalize18(out.ID)
		if err != nil {
			return "", "", fmt.Errorf("apex account find-or-create: invalid account id %q: %w", out.ID, err)
		}
		slog.InfoContext(ctx, "apex account find-or-create", "action", action, "sfid", sfid)
		return sfid, action, nil
	default:
		return "", "", fmt.Errorf("apex account find-or-create: unexpected action %q: %s", out.Action, out.Message)
	}
}

// post sends the Apex request with the sObject client's session (refreshed once on 401) and returns the
// status and body unchanged, so Apex error statuses keep their typed meaning.
func (c *ApexAccountClient) post(ctx context.Context, body []byte) (int, []byte, error) {
	newReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.client.sf.GetInstanceUrl()+c.path, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build apex account request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.client.sf.GetAccessToken())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
	req, err := newReq()
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.client.sf.GetHTTPClient().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("apex account find-or-create: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close() //nolint:errcheck
		if refreshErr := c.client.refreshSession(); refreshErr != nil {
			return 0, nil, fmt.Errorf("apex account find-or-create: session refresh: %w", refreshErr)
		}
		req2, err := newReq()
		if err != nil {
			return 0, nil, err
		}
		resp, err = c.client.sf.GetHTTPClient().Do(req2)
		if err != nil {
			return 0, nil, fmt.Errorf("apex account find-or-create (retry): %w", err)
		}
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return 0, nil, fmt.Errorf("apex account find-or-create: reading response: %w", readErr)
	}
	return resp.StatusCode, raw, nil
}

// apexMessage returns the Apex error message, or the body text when it is not the {message} shape.
func apexMessage(raw []byte, status int) string {
	var out apexAccountResponse
	if json.Unmarshal(raw, &out) == nil && strings.TrimSpace(out.Message) != "" {
		return snippet([]byte(out.Message))
	}
	return fmt.Sprintf("apex account find-or-create: status %d: %s", status, snippet(raw))
}

// snippet keeps log and error text bounded whatever Salesforce or a proxy answers
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
