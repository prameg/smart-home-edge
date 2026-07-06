// Package entitymap resolves between the cloud's stable device_uid and Home
// Assistant's entity_id, and translates a desired-state document into an HA
// service call.
//
// The AUTHORITATIVE map is pushed down by the cloud in the retained config doc
// (homes/{uid}/config) and applied at runtime via New(); see internal/agent.
// The built-in defaults + the optional {ConfigDir}/entity-map.json override are
// only a dev/bootstrap convenience for running the agent before (or without) a
// cloud config — a tester can wire physical entities without recompiling.
package entitymap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Entry binds one cloud device to one HA entity.
type Entry struct {
	DeviceUID string `json:"device_uid"`
	EntityID  string `json:"entity_id"`
}

// Map is a bidirectional device_uid <-> entity_id index.
type Map struct {
	deviceToEntity map[string]string
	entityToDevice map[string]string
}

// defaults is the built-in spike map. Replace the entity_ids with entities that
// exist on the test Pi (or override via {ConfigDir}/entity-map.json). The
// device_uids must match rows the cloud created for the claimed home.
var defaults = []Entry{
	{DeviceUID: "spike-light-1", EntityID: "light.spike_test"},
	{DeviceUID: "spike-switch-1", EntityID: "switch.spike_test"},
}

// New builds a bidirectional map from the given entries (the shape the cloud
// config doc carries).
func New(entries []Entry) Map {
	m := Map{
		deviceToEntity: make(map[string]string, len(entries)),
		entityToDevice: make(map[string]string, len(entries)),
	}

	for _, e := range entries {
		if e.DeviceUID == "" || e.EntityID == "" {
			continue
		}

		m.deviceToEntity[e.DeviceUID] = e.EntityID
		m.entityToDevice[e.EntityID] = e.DeviceUID
	}

	return m
}

// Load builds the bootstrap map, preferring {dir}/entity-map.json over the
// built-in defaults so a tester can point at real entities before the cloud
// pushes an authoritative config.
func Load(dir string) Map {
	entries := defaults

	if override := readOverride(filepath.Join(dir, "entity-map.json")); len(override) > 0 {
		entries = override
	}

	return New(entries)
}

// Len reports how many device<->entity bindings the map holds.
func (m Map) Len() int {
	return len(m.deviceToEntity)
}

// Devices returns every mapped device_uid (order unspecified). Used to reconcile
// reported state for the whole mapped set on (re)connect.
func (m Map) Devices() []string {
	out := make([]string, 0, len(m.deviceToEntity))
	for deviceUID := range m.deviceToEntity {
		out = append(out, deviceUID)
	}

	return out
}

// Entries returns every device_uid <-> entity_id binding (order unspecified).
// Used by the status panel to list the mapped devices alongside their live HA
// state.
func (m Map) Entries() []Entry {
	out := make([]Entry, 0, len(m.deviceToEntity))
	for deviceUID, entityID := range m.deviceToEntity {
		out = append(out, Entry{DeviceUID: deviceUID, EntityID: entityID})
	}

	return out
}

// EntityForDevice returns the HA entity for a device_uid ("" if unmapped).
func (m Map) EntityForDevice(deviceUID string) string {
	return m.deviceToEntity[deviceUID]
}

// DeviceForEntity returns the device_uid for an HA entity ("" if unmapped).
func (m Map) DeviceForEntity(entityID string) string {
	return m.entityToDevice[entityID]
}

// DomainOf extracts the HA domain from an entity_id ("light.kitchen" -> "light").
func DomainOf(entityID string) string {
	domain, _, _ := strings.Cut(entityID, ".")

	return domain
}

func readOverride(path string) []Entry {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}

	return entries
}
