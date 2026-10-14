// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

func TestProcessB2BOrgLookupByWebsiteRequest_found(t *testing.T) {
	reader := stubB2BOrgReader{websiteOrg: &model.B2BOrg{UID: "0014100000Te2ovAAB"}, websiteFound: true}
	got := processB2BOrgLookupByWebsiteRequest(context.Background(), []byte(`{"name":"Acme Corp","website":"acme.com"}`), reader)

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

func TestProcessB2BOrgLookupByWebsiteRequest_notFound(t *testing.T) {
	reader := stubB2BOrgReader{websiteFound: false}
	got := processB2BOrgLookupByWebsiteRequest(context.Background(), []byte(`{"name":"Nonexistent","website":"nowhere.example"}`), reader)

	data, _ := json.Marshal(got)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "b2b org not found" {
		t.Fatalf("error = %q, want %q", resp.Error, "b2b org not found")
	}
}

func TestProcessB2BOrgLookupByWebsiteRequest_readerError(t *testing.T) {
	reader := stubB2BOrgReader{websiteErr: errors.New("soql query failed")}
	got := processB2BOrgLookupByWebsiteRequest(context.Background(), []byte(`{"website":"acme.com"}`), reader)

	data, _ := json.Marshal(got)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "b2b org lookup failed" {
		t.Fatalf("error = %q, want %q", resp.Error, "b2b org lookup failed")
	}
}

func TestProcessB2BOrgLookupByWebsiteRequest_missingNameAndWebsite(t *testing.T) {
	reader := stubB2BOrgReader{}
	got := processB2BOrgLookupByWebsiteRequest(context.Background(), []byte(`{"name":"","website":""}`), reader)

	data, _ := json.Marshal(got)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "name or website is required" {
		t.Fatalf("error = %q", resp.Error)
	}
}

func TestProcessB2BOrgLookupByWebsiteRequest_invalidBody(t *testing.T) {
	reader := stubB2BOrgReader{}
	got := processB2BOrgLookupByWebsiteRequest(context.Background(), []byte(`not json`), reader)

	data, _ := json.Marshal(got)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "invalid request body" {
		t.Fatalf("error = %q", resp.Error)
	}
}
