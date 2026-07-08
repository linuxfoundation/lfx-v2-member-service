// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

type stubB2BOrgReader struct {
	org *model.B2BOrg
	err error
}

func (s stubB2BOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.org, nil
}

func (s stubB2BOrgReader) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s stubB2BOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return nil, nil
}

func TestProcessB2BOrgLookupRequest_found(t *testing.T) {
	reader := stubB2BOrgReader{org: &model.B2BOrg{UID: "0014100000Te2ovAAB"}}
	got := processB2BOrgLookupRequest(context.Background(), []byte(`{"id":"0014100000Te2ovAAB"}`), reader)

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var resp struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.ID != "0014100000Te2ovAAB" {
		t.Fatalf("id = %q", resp.ID)
	}
}

func TestProcessB2BOrgLookupRequest_notFound(t *testing.T) {
	reader := stubB2BOrgReader{err: errs.NewNotFound("b2b org not found")}
	got := processB2BOrgLookupRequest(context.Background(), []byte(`{"id":"51fde723-67df-4e0e-91c6-936d01d59559"}`), reader)

	data, _ := json.Marshal(got)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error == "" {
		t.Fatal("expected error response")
	}
}

func TestProcessB2BOrgLookupRequest_missingID(t *testing.T) {
	reader := stubB2BOrgReader{}
	got := processB2BOrgLookupRequest(context.Background(), []byte(`{"id":""}`), reader)

	data, _ := json.Marshal(got)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "id is required" {
		t.Fatalf("error = %q", resp.Error)
	}
}
