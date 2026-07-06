package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/entitymap"
)

const configStateFile = "gateway-config.json"

func configStatePath(dataDir string) string {
	return filepath.Join(dataDir, configStateFile)
}

// isClaimed reports whether the gateway is currently bound to a home.
func (a *Agent) isClaimed() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.claimed
}

// deviceForEntity / entityForDevice read the current (config-driven) map under
// the lock, so a concurrent config update never races the HA/command handlers.
func (a *Agent) deviceForEntity(entityID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.emap.DeviceForEntity(entityID)
}

func (a *Agent) entityForDevice(deviceUID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.emap.EntityForDevice(deviceUID)
}

// handleConfig applies the retained downlink config doc: the cloud's source of
// truth for claim status + the device entity_map. Monotonic on config_version
// (a stale redelivery is ignored). On the unclaimed->claimed transition it
// activates the device sync (subscribe to downlink, publish inventory, flush the
// offline buffer) in a separate goroutine so it never deadlocks the broker's
// message-callback thread.
func (a *Agent) handleConfig(_ string, raw []byte) {
	var cfg contract.ConfigPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		a.log.Warn("config decode failed", "error", err)

		return
	}

	a.mu.Lock()
	if cfg.ConfigVersion < a.configVersion {
		a.mu.Unlock()

		return
	}

	wasClaimed := a.claimed
	a.claimed = cfg.Claimed
	a.configVersion = cfg.ConfigVersion
	a.emap = entitymap.New(toEntries(cfg.EntityMap))
	a.mu.Unlock()

	a.saveConfigState(cfg)

	switch {
	case cfg.Claimed && !wasClaimed:
		a.log.Info("gateway claimed; activating device sync", "config_version", cfg.ConfigVersion)

		go a.onClaimed()
	case !cfg.Claimed && wasClaimed:
		a.log.Info("gateway unclaimed; deactivating device sync", "config_version", cfg.ConfigVersion)

		go a.onUnclaimed()
	}
}

// onClaimed runs the just-became-claimed side effects off the broker callback
// goroutine.
func (a *Agent) onClaimed() {
	// The gateway is now bound to a home — clear the on-HA claim-code prompt.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.dismissClaimNotification(ctx)

	if a.broker == nil || !a.broker.IsConnected() {
		return
	}

	a.activateClaimed(a.broker)

	for _, p := range a.buffer.drain() {
		_ = a.broker.Publish(p.topic, p.payload, p.qos, p.retain)
	}
}

// onUnclaimed runs the just-became-unclaimed side effects off the broker
// callback goroutine.
func (a *Agent) onUnclaimed() {
	if a.broker != nil && a.broker.IsConnected() {
		a.deactivateClaimed(a.broker)
	}

	// The cloud cleared this unit's claim code on unclaim, so reissue a fresh
	// one and resurface it — the gateway is ready to be re-added to a home.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.SurfaceClaimStatus(ctx)
}

// publishInventory reports the current HA entity inventory as a retained uplink,
// hash-diffed so an unchanged inventory is not re-published. No-op while
// unclaimed or disconnected.
func (a *Agent) publishInventory(ctx context.Context) {
	if !a.isClaimed() || a.broker == nil || !a.broker.IsConnected() {
		return
	}

	entities, err := a.ha.EntityInventory(ctx)
	if err != nil {
		a.log.Warn("inventory fetch failed", "error", err)

		return
	}

	payload := contract.InventoryPayload{
		Hash:     inventoryHash(entities),
		Entities: entities,
		TS:       time.Now().UTC().Format(time.RFC3339),
	}

	a.mu.Lock()
	if payload.Hash == a.inventoryHash {
		a.mu.Unlock()

		return
	}
	a.inventoryHash = payload.Hash
	a.mu.Unlock()

	raw, err := json.Marshal(payload)
	if err != nil {
		a.log.Warn("inventory encode failed", "error", err)

		return
	}

	if err := a.broker.Publish(contract.InventoryTopic(a.creds.UID), raw, 1, true); err != nil {
		a.log.Warn("inventory publish failed", "error", err)
		// Reset so the next refresh retries rather than seeing an unchanged hash.
		a.mu.Lock()
		a.inventoryHash = ""
		a.mu.Unlock()
	}
}

// persistedConfig is the on-disk shape of the last applied cloud config, so an
// already-claimed gateway boots straight into claimed mode.
type persistedConfig struct {
	Claimed       bool                            `json:"claimed"`
	ConfigVersion int                             `json:"config_version"`
	EntityMap     []contract.ConfigEntityMapEntry `json:"entity_map"`
}

func (a *Agent) restoreConfigState() {
	raw, err := os.ReadFile(a.configPath)
	if err != nil {
		return
	}

	var stored persistedConfig
	if err := json.Unmarshal(raw, &stored); err != nil {
		return
	}

	a.mu.Lock()
	a.claimed = stored.Claimed
	a.configVersion = stored.ConfigVersion
	if len(stored.EntityMap) > 0 {
		a.emap = entitymap.New(toEntries(stored.EntityMap))
	}
	a.mu.Unlock()
}

func (a *Agent) saveConfigState(cfg contract.ConfigPayload) {
	stored := persistedConfig{
		Claimed:       cfg.Claimed,
		ConfigVersion: cfg.ConfigVersion,
		EntityMap:     cfg.EntityMap,
	}

	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return
	}

	if err := os.MkdirAll(a.cfg.DataDir, 0o755); err != nil {
		return
	}

	tmp := a.configPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}

	_ = os.Rename(tmp, a.configPath)
}

// toEntries adapts the wire entity_map into the entitymap package's shape.
func toEntries(entries []contract.ConfigEntityMapEntry) []entitymap.Entry {
	out := make([]entitymap.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entitymap.Entry{DeviceUID: e.DeviceUID, EntityID: e.EntityID})
	}

	return out
}

// inventoryHash is a stable content hash of the entity set so an unchanged
// inventory is not re-published.
func inventoryHash(entities []contract.InventoryEntity) string {
	raw, err := json.Marshal(entities)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])
}
