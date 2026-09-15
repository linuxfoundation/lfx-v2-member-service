// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package nats provides NATS JetStream KV-backed implementations of the domain
// storage ports.
package nats

import "time"

// CacheStatus describes the freshness of a cached value.
type CacheStatus int

const (
	// CacheStatusFresh indicates the cached value is within its stale threshold.
	CacheStatusFresh CacheStatus = iota
	// CacheStatusStale indicates the cached value is past its stale threshold but
	// not yet expired; it may be served while a background refresh is triggered.
	CacheStatusStale
	// CacheStatusExpired indicates the cached value is past its expiry threshold
	// and must not be served; a synchronous fetch is required.
	CacheStatusExpired
	// CacheStatusMiss indicates no cached value was found for the key.
	CacheStatusMiss
)

// CachedValue is the JSON envelope written to every KV entry. It carries the
// actual record alongside two soft-TTL timestamps so callers can distinguish
// fresh, stale, and expired items without relying solely on the bucket-level TTL.
type CachedValue[T any] struct {
	// Data is the cached domain object.
	Data T `json:"data"`
	// StaleAt is the time after which the cached value may still be returned,
	// but a background refresh should be triggered.
	StaleAt time.Time `json:"stale_at"`
	// ExpiresAt is the time after which the cached value must not be returned;
	// a synchronous Salesforce fetch is required.
	ExpiresAt time.Time `json:"expires_at"`
}

// Status returns the CacheStatus of the value based on the current wall clock.
func (cv CachedValue[T]) Status() CacheStatus {
	now := time.Now()
	switch {
	case now.Before(cv.StaleAt):
		return CacheStatusFresh
	case now.Before(cv.ExpiresAt):
		return CacheStatusStale
	default:
		return CacheStatusExpired
	}
}

// CacheResult bundles a decoded value with its freshness status.
type CacheResult[T any] struct {
	// Value is the decoded domain object.
	Value T
	// Status is the freshness classification of the cached value.
	Status CacheStatus
	// Revision is the KV entry revision the value was read at; 0 when no
	// entry existed. Revision-conditional write-backs pass it so a fetch
	// started before an eviction cannot repopulate stale data afterward.
	Revision uint64
}

// TTLConfig controls the soft-TTL durations written into every CachedValue
// envelope. Both durations must be less than the NATS bucket MaxAge so that
// NATS-level eviction always occurs after soft expiry.
type TTLConfig struct {
	// StaleDuration is the duration after which a cached value is considered
	// stale and a background refresh should be triggered.
	StaleDuration time.Duration
	// ExpiresDuration is the duration after which a cached value must not be
	// served and a synchronous fetch is required.
	ExpiresDuration time.Duration
}

// DefaultTTLConfig is the recommended TTL configuration for cached membership
// records. StaleAt is set 6 hours out and ExpiresAt is set 23 hours out, both
// safely within the 24-hour NATS bucket MaxAge.
var DefaultTTLConfig = TTLConfig{
	StaleDuration:   6 * time.Hour,
	ExpiresDuration: 23 * time.Hour,
}

// newCachedValue wraps data in a CachedValue envelope using the given TTLConfig.
func newCachedValue[T any](data T, cfg TTLConfig) CachedValue[T] {
	now := time.Now()
	return CachedValue[T]{
		Data:      data,
		StaleAt:   now.Add(cfg.StaleDuration),
		ExpiresAt: now.Add(cfg.ExpiresDuration),
	}
}
