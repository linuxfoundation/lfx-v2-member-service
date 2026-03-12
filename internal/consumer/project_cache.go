// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"time"

	gocache "github.com/patrickmn/go-cache"
)

const projectCacheTTL = 10 * time.Minute

// projectCache is an in-memory cache mapping Salesforce project SFIDs to resolved
// project info (UID, name, slug) with a fixed TTL. It wraps patrickmn/go-cache for
// thread-safe access with automatic expiration.
type projectCache struct {
	c *gocache.Cache
}

// newProjectCache creates a new projectCache with the standard TTL and a cleanup
// interval of twice the TTL.
func newProjectCache() *projectCache {
	return &projectCache{
		c: gocache.New(projectCacheTTL, 2*projectCacheTTL),
	}
}

// get returns the cached projectInfo for the given SFID, and whether it was found
// and not yet expired.
func (pc *projectCache) get(sfid string) (projectInfo, bool) {
	v, ok := pc.c.Get(sfid)
	if !ok {
		return projectInfo{}, false
	}
	info, valid := v.(projectInfo)
	if !valid {
		return projectInfo{}, false
	}
	return info, true
}

// set stores the given projectInfo for the SFID with the standard TTL.
func (pc *projectCache) set(sfid string, info projectInfo) {
	pc.c.Set(sfid, info, gocache.DefaultExpiration)
}

// delete removes the cache entry for the given SFID, if present.
func (pc *projectCache) delete(sfid string) {
	pc.c.Delete(sfid)
}
