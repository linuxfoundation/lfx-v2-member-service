// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package project

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

type stubSlugRPC struct {
	slugByUID map[string]string
	uidBySlug map[string]string
}

func (s stubSlugRPC) GetSlug(_ context.Context, projectUID string) (string, error) {
	slug, ok := s.slugByUID[projectUID]
	if !ok {
		return "", errs.NewNotFound("project not found", nil)
	}
	return slug, nil
}

func (s stubSlugRPC) SlugToUID(_ context.Context, slug string) (string, error) {
	uid, ok := s.uidBySlug[slug]
	if !ok {
		return "", errs.NewNotFound("project not found", nil)
	}
	return uid, nil
}

type stubSlugRepo struct {
	sfidBySlug map[string]string
	calls      []string
}

func (s *stubSlugRepo) FetchSFIDBySlug(_ context.Context, slug string) (string, error) {
	s.calls = append(s.calls, slug)
	return s.sfidBySlug[slug], nil
}

type stubProjectCache struct {
	uidBySlug map[string]string
	sfidByUID map[string]string
	putUID    []struct {
		slug string
		uid  string
	}
}

func (c *stubProjectCache) GetProjectSFID(_ context.Context, projectUID string) (nats.CacheResult[string], error) {
	if sfid, ok := c.sfidByUID[projectUID]; ok {
		return nats.CacheResult[string]{Status: nats.CacheStatusFresh, Value: sfid}, nil
	}
	return nats.CacheResult[string]{Status: nats.CacheStatusMiss}, nil
}

func (c *stubProjectCache) PutProjectSFID(context.Context, string, string) error { return nil }

func (c *stubProjectCache) GetProjectUID(_ context.Context, slug string) (nats.CacheResult[string], error) {
	if uid, ok := c.uidBySlug[slug]; ok {
		return nats.CacheResult[string]{Status: nats.CacheStatusFresh, Value: uid}, nil
	}
	return nats.CacheResult[string]{Status: nats.CacheStatusMiss}, nil
}

func (c *stubProjectCache) PutProjectUID(_ context.Context, slug, uid string) error {
	c.putUID = append(c.putUID, struct {
		slug string
		uid  string
	}{slug: slug, uid: uid})
	c.uidBySlug[slug] = uid
	return nil
}

func newTestResolver(rpc slugLookupRPC, slugRepo projectSlugRepo, cache projectMappingCache) *Resolver {
	return &Resolver{
		rpc:      rpc,
		slugRepo: slugRepo,
		enricher: stubProjectEnricher{},
		cache:    cache,
	}
}

type stubProjectEnricher struct{}

func (stubProjectEnricher) fetchProjectByID(context.Context, string) (*projectMeta, error) {
	return nil, nil
}

func (stubProjectEnricher) fetchProjectsByIDs(context.Context, []string) (map[string]projectMeta, error) {
	return nil, nil
}

func TestNormalizeProjectSlug(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercase unchanged", in: "toip", want: "toip"},
		{name: "mixed case", in: "ToIP", want: "toip"},
		{name: "trim whitespace", in: "  ToIP  ", want: "toip"},
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "   ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeProjectSlug(tt.in))
		})
	}
}

func TestResolver_UIDFromSlug(t *testing.T) {
	ctx := context.Background()
	const wantUID = "54ec092e-cadd-49af-9edf-9a5c888cc283"

	tests := []struct {
		name    string
		slug    string
		cache   *stubProjectCache
		rpc     stubSlugRPC
		wantUID string
		wantErr bool
	}{
		{
			name: "mixed case resolves via normalized cache key",
			slug: "ToIP",
			cache: &stubProjectCache{
				uidBySlug: map[string]string{"toip": wantUID},
			},
			wantUID: wantUID,
		},
		{
			name: "mixed case resolves via normalized RPC lookup",
			slug: "ToIP",
			cache: &stubProjectCache{
				uidBySlug: map[string]string{},
			},
			rpc: stubSlugRPC{
				uidBySlug: map[string]string{"toip": wantUID},
			},
			wantUID: wantUID,
		},
		{
			name:    "empty slug returns not found",
			slug:    "   ",
			cache:   &stubProjectCache{uidBySlug: map[string]string{}},
			wantErr: true,
		},
		{
			name:    "unknown slug returns not found",
			slug:    "missing",
			cache:   &stubProjectCache{uidBySlug: map[string]string{}},
			rpc:     stubSlugRPC{uidBySlug: map[string]string{}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.cache == nil {
				tt.cache = &stubProjectCache{uidBySlug: map[string]string{}}
			}
			r := newTestResolver(tt.rpc, &stubSlugRepo{}, tt.cache)

			uid, err := r.UIDFromSlug(ctx, tt.slug)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, errs.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
				assert.Empty(t, uid)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantUID, uid)
		})
	}
}

func TestResolver_SFIDFromUID_CachesNormalizedSlugKey(t *testing.T) {
	ctx := context.Background()
	const (
		projectUID = "54ec092e-cadd-49af-9edf-9a5c888cc283"
		mixedSlug  = "ToIP"
		wantSFID   = "a0941000002wBz4AAE"
	)

	cache := &stubProjectCache{
		uidBySlug: map[string]string{},
		sfidByUID: map[string]string{},
	}
	repo := &stubSlugRepo{
		sfidBySlug: map[string]string{mixedSlug: wantSFID},
	}
	r := newTestResolver(
		stubSlugRPC{slugByUID: map[string]string{projectUID: mixedSlug}},
		repo,
		cache,
	)

	sfid, err := r.SFIDFromUID(ctx, projectUID)
	require.NoError(t, err)
	assert.Equal(t, wantSFID, sfid)
	require.Len(t, cache.putUID, 1)
	assert.Equal(t, "toip", cache.putUID[0].slug, "cache key must use normalized slug")
	assert.Equal(t, projectUID, cache.putUID[0].uid)
	require.Len(t, repo.calls, 1)
	assert.Equal(t, mixedSlug, repo.calls[0], "Salesforce SOQL must keep project-service slug")
}
