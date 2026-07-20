// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// MockOrgWorkspaces is an in-memory implementation of port.OrgWorkspacesReader
// and port.OrgWorkspacesWriter. It supports seeding a fixed workspaces value and
// revision for read tests, and records UpdateWorkspaces calls for assertion in
// write tests. CAS semantics mirror the production NATS KV implementation.
type MockOrgWorkspaces struct {
	mu         sync.RWMutex
	workspaces map[string]*model.OrgWorkspaces
	revision   map[string]uint64
	putErr     error
}

// NewMockOrgWorkspaces returns an empty, ready-to-use mock.
func NewMockOrgWorkspaces() *MockOrgWorkspaces {
	return &MockOrgWorkspaces{
		workspaces: make(map[string]*model.OrgWorkspaces),
		revision:   make(map[string]uint64),
	}
}

// Seed pre-populates the mock with a workspaces value and revision for the given orgUID.
func (m *MockOrgWorkspaces) Seed(orgUID string, ws *model.OrgWorkspaces, rev uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workspaces[orgUID] = ws
	m.revision[orgUID] = rev
}

// SetPutError configures the mock to return err on the next UpdateWorkspaces call.
func (m *MockOrgWorkspaces) SetPutError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putErr = err
}

// GetWorkspaces returns the seeded workspaces for orgUID, or (nil, 0, nil) when absent.
func (m *MockOrgWorkspaces) GetWorkspaces(_ context.Context, orgUID string) (*model.OrgWorkspaces, uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := m.workspaces[orgUID]
	if !ok {
		return nil, 0, nil
	}
	return ws, m.revision[orgUID], nil
}

// UpdateWorkspaces stores workspaces for orgUID, mirroring production NATS semantics:
// revision == 0 → exclusive create (Conflict if already exists);
// revision > 0  → optimistic-lock update (Conflict if revision doesn't match).
func (m *MockOrgWorkspaces) UpdateWorkspaces(_ context.Context, ws *model.OrgWorkspaces, revision uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		err := m.putErr
		m.putErr = nil
		return err
	}
	orgUID := ws.OrgUID
	if revision == 0 {
		if _, exists := m.workspaces[orgUID]; exists {
			return errors.NewConflict("org workspaces were created concurrently, please retry")
		}
	} else {
		if stored, ok := m.revision[orgUID]; !ok || stored != revision {
			return errors.NewConflict("stale revision")
		}
	}
	m.workspaces[orgUID] = ws
	m.revision[orgUID] = revision + 1
	return nil
}

// --- MockWorkspaceProjects ------------------------------------------------

// MockWorkspaceProjects is an in-memory implementation of port.WorkspaceProjectsReader
// and port.WorkspaceProjectsWriter. CAS semantics mirror MockOrgWorkspaces.
type MockWorkspaceProjects struct {
	mu        sync.RWMutex
	docs      map[string]*model.WorkspaceProjects
	revision  map[string]uint64
	putErr    error
	deleteErr error
}

// NewMockWorkspaceProjects returns an empty, ready-to-use mock.
func NewMockWorkspaceProjects() *MockWorkspaceProjects {
	return &MockWorkspaceProjects{
		docs:     make(map[string]*model.WorkspaceProjects),
		revision: make(map[string]uint64),
	}
}

// Seed pre-populates the mock with a projects document and revision.
func (m *MockWorkspaceProjects) Seed(workspaceUID string, p *model.WorkspaceProjects, rev uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[workspaceUID] = p
	m.revision[workspaceUID] = rev
}

// SetPutError configures the mock to return err on the next UpdateWorkspaceProjects call.
func (m *MockWorkspaceProjects) SetPutError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putErr = err
}

// SetDeleteError configures the mock to return err on the next DeleteWorkspaceProjects call.
func (m *MockWorkspaceProjects) SetDeleteError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteErr = err
}

// GetWorkspaceProjects returns the seeded document for workspaceUID, or (nil, 0, nil) when absent.
func (m *MockWorkspaceProjects) GetWorkspaceProjects(_ context.Context, workspaceUID string) (*model.WorkspaceProjects, uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.docs[workspaceUID]
	if !ok {
		return nil, 0, nil
	}
	return p, m.revision[workspaceUID], nil
}

// UpdateWorkspaceProjects stores the document, mirroring production NATS CAS semantics.
func (m *MockWorkspaceProjects) UpdateWorkspaceProjects(_ context.Context, p *model.WorkspaceProjects, revision uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		err := m.putErr
		m.putErr = nil
		return err
	}
	wsUID := p.WorkspaceUID
	if revision == 0 {
		if _, exists := m.docs[wsUID]; exists {
			return errors.NewConflict("workspace projects were created concurrently, please retry")
		}
	} else {
		if stored, ok := m.revision[wsUID]; !ok || stored != revision {
			return errors.NewConflict("stale revision")
		}
	}
	m.docs[wsUID] = p
	m.revision[wsUID] = revision + 1
	return nil
}

// DeleteWorkspaceProjects removes the document for workspaceUID. No-op if absent.
// Returns the injected deleteErr (once) if SetDeleteError was called.
func (m *MockWorkspaceProjects) DeleteWorkspaceProjects(_ context.Context, workspaceUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		err := m.deleteErr
		m.deleteErr = nil
		return err
	}
	delete(m.docs, workspaceUID)
	delete(m.revision, workspaceUID)
	return nil
}

// --- MockProjectResolver --------------------------------------------------

// MockProjectResolver is a stub implementation of port.ProjectResolver used
// in unit tests. Configure ResolveFunc / ResolveBatchFunc to control behaviour.
type MockProjectResolver struct {
	mu               sync.RWMutex
	ResolveFunc      func(ctx context.Context, idOrSlug string) (model.ProjectInfo, error)
	ResolveBatchFunc func(ctx context.Context, idsOrSlugs []string) ([]model.ProjectInfo, []error)
	// Seed is a simple lookup table populated via SeedProject.
	seed map[string]model.ProjectInfo
}

// NewMockProjectResolver returns a MockProjectResolver backed by the seeded lookup table.
// Use SeedProject to add known projects; lookups that miss the table return a
// NewValidation("unknown project") error (matching the production behaviour).
func NewMockProjectResolver() *MockProjectResolver {
	r := &MockProjectResolver{
		seed: make(map[string]model.ProjectInfo),
	}
	r.ResolveFunc = r.defaultResolve
	r.ResolveBatchFunc = r.defaultResolveBatch
	return r
}

// SeedProject adds or replaces a project in the lookup table, keyed by both
// UID and slug so both lookup paths succeed. The slug key is normalized to
// match UIDFromSlug's and defaultResolve's lookup key.
func (r *MockProjectResolver) SeedProject(p model.ProjectInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seed[p.UID] = p
	if p.Slug != "" {
		r.seed[strings.ToLower(strings.TrimSpace(p.Slug))] = p
	}
}

func (r *MockProjectResolver) defaultResolve(_ context.Context, idOrSlug string) (model.ProjectInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.seed[idOrSlug]; ok {
		return p, nil
	}
	normalized := strings.ToLower(strings.TrimSpace(idOrSlug))
	if p, ok := r.seed[normalized]; ok {
		return p, nil
	}
	return model.ProjectInfo{}, errors.NewValidation("unknown project: " + idOrSlug)
}

func (r *MockProjectResolver) defaultResolveBatch(ctx context.Context, idsOrSlugs []string) ([]model.ProjectInfo, []error) {
	infos := make([]model.ProjectInfo, len(idsOrSlugs))
	errs := make([]error, len(idsOrSlugs))
	for i, id := range idsOrSlugs {
		infos[i], errs[i] = r.defaultResolve(ctx, id)
	}
	return infos, errs
}

// SFIDFromUID implements port.ProjectResolver.
func (r *MockProjectResolver) SFIDFromUID(_ context.Context, uid string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.seed[uid]; ok {
		return p.SFID, nil
	}
	return "", errors.NewValidation("unknown project: " + uid)
}

// UIDFromSlug implements port.ProjectResolver.
// Slugs are lowercased before lookup to mirror project.Resolver (v2 slugs are
// lowercase-only; Salesforce Slug__c may use mixed case e.g. ToIP).
func (r *MockProjectResolver) UIDFromSlug(_ context.Context, slug string) (string, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return "", errors.NewNotFound("project not found", fmt.Errorf("empty slug"))
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.seed[slug]; ok {
		return p.UID, nil
	}
	return "", errors.NewValidation("unknown project: " + slug)
}

// ResolveProject implements port.ProjectResolver.
func (r *MockProjectResolver) ResolveProject(ctx context.Context, idOrSlug string) (model.ProjectInfo, error) {
	return r.ResolveFunc(ctx, idOrSlug)
}

// ResolveProjectsBatch implements port.ProjectResolver.
func (r *MockProjectResolver) ResolveProjectsBatch(ctx context.Context, idsOrSlugs []string) ([]model.ProjectInfo, []error) {
	return r.ResolveBatchFunc(ctx, idsOrSlugs)
}
