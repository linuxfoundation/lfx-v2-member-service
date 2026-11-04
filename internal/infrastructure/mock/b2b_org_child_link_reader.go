// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"
)

// MockB2BOrgChildLinkReader provides an in-memory port.B2BOrgChildLinkReader.
// Unknown children have no parents. Calls are recorded so tests can assert
// how many reads were made.
type MockB2BOrgChildLinkReader struct {
	mu      sync.Mutex
	parents map[string][]string
	errs    map[string]error
	calls   []string
}

// NewMockB2BOrgChildLinkReader returns a reader with no links.
func NewMockB2BOrgChildLinkReader() *MockB2BOrgChildLinkReader {
	return &MockB2BOrgChildLinkReader{
		parents: map[string][]string{},
		errs:    map[string]error{},
	}
}

// SetParents sets the orgs that list childUID as a child.
func (m *MockB2BOrgChildLinkReader) SetParents(childUID string, parents ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parents[childUID] = parents
}

// SetError makes reads for childUID fail with err.
func (m *MockB2BOrgChildLinkReader) SetError(childUID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs[childUID] = err
}

// Calls returns the child UIDs read so far, in order.
func (m *MockB2BOrgChildLinkReader) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// ParentsListingChild returns the configured parents or error for childUID.
func (m *MockB2BOrgChildLinkReader) ParentsListingChild(_ context.Context, childUID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, childUID)
	if err := m.errs[childUID]; err != nil {
		return nil, err
	}
	return append([]string{}, m.parents[childUID]...), nil
}
