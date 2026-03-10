// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"sync"
	"time"
)

const projectCacheTTL = 10 * time.Minute

// projectCacheEntry holds a resolved project info value with an expiration time.
type projectCacheEntry struct {
	info      projectInfo
	expiresAt time.Time
}

// projectCache is a thread-safe in-memory cache mapping Salesforce project SFIDs
// to resolved project info (UID, name, slug) with a fixed TTL.
type projectCache struct {
	mu      sync.RWMutex
	entries map[string]projectCacheEntry
}

// newProjectCache creates a new empty projectCache.
func newProjectCache() *projectCache {
	return &projectCache{
		entries: make(map[string]projectCacheEntry),
	}
}

// get returns the cached projectInfo for the given SFID, and whether it was found
// and not yet expired.
func (c *projectCache) get(sfid string) (projectInfo, bool) {
	c.mu.RLock()
	entry, ok := c.entries[sfid]
	c.mu.RUnlock()

	if !ok {
		return projectInfo{}, false
	}

	if time.Now().After(entry.expiresAt) {
		// Entry has expired; evict it lazily.
		c.mu.Lock()
		delete(c.entries, sfid)
		c.mu.Unlock()
		return projectInfo{}, false
	}

	return entry.info, true
}

// set stores the given projectInfo for the SFID with the standard TTL.
func (c *projectCache) set(sfid string, info projectInfo) {
	c.mu.Lock()
	c.entries[sfid] = projectCacheEntry{
		info:      info,
		expiresAt: time.Now().Add(projectCacheTTL),
	}
	c.mu.Unlock()
}

// delete removes the cache entry for the given SFID, if present.
func (c *projectCache) delete(sfid string) {
	c.mu.Lock()
	delete(c.entries, sfid)
	c.mu.Unlock()
}
