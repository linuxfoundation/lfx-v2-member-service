// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Forward-lookup index key formats in the project-membership-mapping KV bucket.
// Each key maps a source SFID to a JSON-encoded list of related SFIDs, enabling
// cross-entity fan-out re-indexing without a full KV scan.
const (
	// mappingKeyAccountAssets maps accountSfid → [assetSfid, ...].
	mappingKeyAccountAssets = "account.assets.%s"

	// mappingKeyProduct2Assets maps product2Sfid → [assetSfid, ...].
	mappingKeyProduct2Assets = "product2.assets.%s"

	// mappingKeyContactProjectRoles maps contactSfid → [projectRoleSfid, ...].
	mappingKeyContactProjectRoles = "contact.project-roles.%s"

	// mappingKeyAssetProjectRoles maps assetSfid → [projectRoleSfid, ...].
	mappingKeyAssetProjectRoles = "asset.project-roles.%s"
)

// OCC retry configuration for read-modify-write operations on the mapping KV bucket.
const (
	mappingOCCMaxRetries    = 5
	mappingOCCRetryInterval = 200 * time.Millisecond
)

// mappingStore wraps a NATS JetStream KeyValue handle for the project-membership-mapping bucket
// and provides helpers for maintaining forward-lookup index lists using optimistic concurrency
// control (OCC) — the same pattern used by v1-sync-helper for its merged-user email indexes.
type mappingStore struct {
	kv jetstream.KeyValue
}

// newMappingStore creates a mappingStore backed by the given KeyValue handle.
func newMappingStore(kv jetstream.KeyValue) *mappingStore {
	return &mappingStore{kv: kv}
}

// addToList appends value to the JSON-encoded string list stored at key, using OCC retries
// to handle concurrent updates. If the key does not yet exist it is created. The function
// is idempotent: adding a value that is already present is a no-op.
func (m *mappingStore) addToList(ctx context.Context, key, value string) error {
	for attempt := 1; attempt <= mappingOCCMaxRetries; attempt++ {
		list, revision, err := m.getList(ctx, key)
		if err != nil {
			return fmt.Errorf("mapping addToList get %q: %w", key, err)
		}

		// Check if value is already present (idempotent).
		for _, v := range list {
			if v == value {
				return nil
			}
		}

		list = append(list, value)

		encoded, marshalErr := json.Marshal(list)
		if marshalErr != nil {
			return fmt.Errorf("mapping addToList marshal %q: %w", key, marshalErr)
		}

		var casErr error
		if revision == 0 {
			// Key did not previously exist; use Create for atomic first-write.
			_, casErr = m.kv.Create(ctx, key, encoded)
		} else {
			// Key exists; use Update with the last-known revision.
			_, casErr = m.kv.Update(ctx, key, encoded, revision)
		}

		if casErr == nil {
			return nil
		}

		// CAS conflict — another writer raced us; retry after a short sleep.
		slog.DebugContext(ctx, "mapping OCC conflict on addToList, retrying",
			"key", key,
			"attempt", attempt,
			"error", casErr,
		)
		time.Sleep(mappingOCCRetryInterval)
	}

	return fmt.Errorf("mapping addToList %q: exceeded %d OCC retries", key, mappingOCCMaxRetries)
}

// removeFromList removes value from the JSON-encoded string list stored at key, using OCC
// retries to handle concurrent updates. Removing a value that is not present is a no-op.
func (m *mappingStore) removeFromList(ctx context.Context, key, value string) error { //nolint:unused // Reserved for future delete cleanup paths.
	for attempt := 1; attempt <= mappingOCCMaxRetries; attempt++ {
		list, revision, err := m.getList(ctx, key)
		if err != nil {
			return fmt.Errorf("mapping removeFromList get %q: %w", key, err)
		}

		// Build updated list without the value.
		updated := list[:0]
		found := false
		for _, v := range list {
			if v == value {
				found = true
				continue
			}
			updated = append(updated, v)
		}

		if !found {
			// Value not present; nothing to do.
			return nil
		}

		encoded, marshalErr := json.Marshal(updated)
		if marshalErr != nil {
			return fmt.Errorf("mapping removeFromList marshal %q: %w", key, marshalErr)
		}

		if revision == 0 {
			// Key no longer exists; nothing to update.
			return nil
		}

		_, casErr := m.kv.Update(ctx, key, encoded, revision)
		if casErr == nil {
			return nil
		}

		// CAS conflict — retry.
		slog.DebugContext(ctx, "mapping OCC conflict on removeFromList, retrying",
			"key", key,
			"attempt", attempt,
			"error", casErr,
		)
		time.Sleep(mappingOCCRetryInterval)
	}

	return fmt.Errorf("mapping removeFromList %q: exceeded %d OCC retries", key, mappingOCCMaxRetries)
}

// getList reads the JSON-encoded string list stored at key and returns it along with the
// current KV revision (for OCC updates). Returns an empty list and revision 0 when the key
// does not exist.
func (m *mappingStore) getList(ctx context.Context, key string) ([]string, uint64, error) {
	entry, err := m.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return []string{}, 0, nil
		}
		return nil, 0, fmt.Errorf("mapping getList %q: %w", key, err)
	}

	var list []string
	if unmarshalErr := json.Unmarshal(entry.Value(), &list); unmarshalErr != nil {
		return nil, 0, fmt.Errorf("mapping getList unmarshal %q: %w", key, unmarshalErr)
	}

	return list, entry.Revision(), nil
}

// listValues returns the string list stored at key. It is a read-only convenience
// wrapper over getList. Returns an empty slice when the key does not exist.
func (m *mappingStore) listValues(ctx context.Context, key string) ([]string, error) {
	list, _, err := m.getList(ctx, key)
	return list, err
}

// ---- Typed helpers for each forward-lookup index ----

// addAssetToAccount records that assetSfid is associated with accountSfid so that
// account updates can fan out to all affected asset records.
func (m *mappingStore) addAssetToAccount(ctx context.Context, accountSfid, assetSfid string) error {
	return m.addToList(ctx, fmt.Sprintf(mappingKeyAccountAssets, accountSfid), assetSfid)
}

// removeAssetFromAccount removes assetSfid from the account → assets index.
func (m *mappingStore) removeAssetFromAccount(ctx context.Context, accountSfid, assetSfid string) error { //nolint:unused // Reserved for future delete cleanup paths.
	return m.removeFromList(ctx, fmt.Sprintf(mappingKeyAccountAssets, accountSfid), assetSfid)
}

// getAssetsForAccount returns all asset SFIDs associated with the given account SFID.
func (m *mappingStore) getAssetsForAccount(ctx context.Context, accountSfid string) ([]string, error) {
	return m.listValues(ctx, fmt.Sprintf(mappingKeyAccountAssets, accountSfid))
}

// addAssetToProduct2 records that assetSfid is associated with product2Sfid so that
// product updates can fan out to all affected asset records.
func (m *mappingStore) addAssetToProduct2(ctx context.Context, product2Sfid, assetSfid string) error {
	return m.addToList(ctx, fmt.Sprintf(mappingKeyProduct2Assets, product2Sfid), assetSfid)
}

// removeAssetFromProduct2 removes assetSfid from the product2 → assets index.
func (m *mappingStore) removeAssetFromProduct2(ctx context.Context, product2Sfid, assetSfid string) error { //nolint:unused // Reserved for future delete cleanup paths.
	return m.removeFromList(ctx, fmt.Sprintf(mappingKeyProduct2Assets, product2Sfid), assetSfid)
}

// getAssetsForProduct2 returns all asset SFIDs associated with the given Product2 SFID.
func (m *mappingStore) getAssetsForProduct2(ctx context.Context, product2Sfid string) ([]string, error) { //nolint:unused // Reserved for future product2 fan-out delete paths.
	return m.listValues(ctx, fmt.Sprintf(mappingKeyProduct2Assets, product2Sfid))
}

// addProjectRoleToContact records that roleSfid is associated with contactSfid so that
// contact updates can fan out to all affected project_role records.
func (m *mappingStore) addProjectRoleToContact(ctx context.Context, contactSfid, roleSfid string) error {
	return m.addToList(ctx, fmt.Sprintf(mappingKeyContactProjectRoles, contactSfid), roleSfid)
}

// removeProjectRoleFromContact removes roleSfid from the contact → project-roles index.
func (m *mappingStore) removeProjectRoleFromContact(ctx context.Context, contactSfid, roleSfid string) error { //nolint:unused // Reserved for future delete cleanup paths.
	return m.removeFromList(ctx, fmt.Sprintf(mappingKeyContactProjectRoles, contactSfid), roleSfid)
}

// getProjectRolesForContact returns all project_role SFIDs associated with the given contact SFID.
func (m *mappingStore) getProjectRolesForContact(ctx context.Context, contactSfid string) ([]string, error) {
	return m.listValues(ctx, fmt.Sprintf(mappingKeyContactProjectRoles, contactSfid))
}

// addProjectRoleToAsset records that roleSfid is associated with assetSfid so that
// asset updates can fan out to all affected project_role records.
func (m *mappingStore) addProjectRoleToAsset(ctx context.Context, assetSfid, roleSfid string) error {
	return m.addToList(ctx, fmt.Sprintf(mappingKeyAssetProjectRoles, assetSfid), roleSfid)
}

// removeProjectRoleFromAsset removes roleSfid from the asset → project-roles index.
func (m *mappingStore) removeProjectRoleFromAsset(ctx context.Context, assetSfid, roleSfid string) error { //nolint:unused // Reserved for future delete cleanup paths.
	return m.removeFromList(ctx, fmt.Sprintf(mappingKeyAssetProjectRoles, assetSfid), roleSfid)
}

// getProjectRolesForAsset returns all project_role SFIDs associated with the given asset SFID.
func (m *mappingStore) getProjectRolesForAsset(ctx context.Context, assetSfid string) ([]string, error) {
	return m.listValues(ctx, fmt.Sprintf(mappingKeyAssetProjectRoles, assetSfid))
}
