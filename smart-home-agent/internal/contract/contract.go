// Package contract mirrors the MQTT topic + payload boundary shared with the
// cloud repo. The single source of truth is the cloud repo's App\Mqtt\Topics
// and docs/mqtt-contract.md (mirrored at ../../docs/mqtt-contract.md). Change
// all of them together — this file must never drift from the PHP Topics class.
package contract

import "fmt"

// Root is the top-level topic segment every topic is rooted at. The broker ACL
// confines a gateway to homes/{uid}/#.
const Root = "homes"

// StatusOnline / StatusOffline are the exact retained-availability / last-will
// bodies the cloud's ingest matches against (case-insensitive "online" without
// "offline"). Keep them bare strings, not JSON — the ingest tolerates a bare body.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// StateTopic is the uplink reported-state topic for a single device (QoS 1, not
// retained).
func StateTopic(gatewayUID, deviceUID string) string {
	return fmt.Sprintf("%s/%s/state/%s", Root, gatewayUID, deviceUID)
}

// EventTopic is the uplink event/alarm topic (QoS 1, not retained). type is the
// trailing segment the cloud stores as Event.type.
func EventTopic(gatewayUID, eventType string) string {
	return fmt.Sprintf("%s/%s/event/%s", Root, gatewayUID, eventType)
}

// InventoryTopic is the uplink RETAINED HA-entity inventory the cloud upserts
// into devices (agent -> cloud device sync).
func InventoryTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/inventory", Root, gatewayUID)
}

// CommandAckTopic is the uplink command-acknowledgement topic (QoS 1, not
// retained).
func CommandAckTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/cmd/ack", Root, gatewayUID)
}

// AvailabilityTopic is the retained liveness topic (also the last-will topic).
// HA's MQTT convention is an availability topic carrying online/offline.
func AvailabilityTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/availability", Root, gatewayUID)
}

// CommandTopic is the downlink one-shot-command topic the agent subscribes to
// (QoS 1, not retained).
func CommandTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/cmd", Root, gatewayUID)
}

// DesiredShadowTopic is the downlink retained PER-DEVICE desired-state topic
// (one retained document per device, mirroring the per-device state topic).
func DesiredShadowTopic(gatewayUID, deviceUID string) string {
	return fmt.Sprintf("%s/%s/shadow/desired/%s", Root, gatewayUID, deviceUID)
}

// DesiredShadowFilter is the wildcard the agent subscribes to for every
// per-device retained desired-state document; it routes each message by the
// payload's device_uid.
func DesiredShadowFilter(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/shadow/desired/#", Root, gatewayUID)
}

// ConfigTopic is the downlink RETAINED config topic (claim status + entity_map)
// the agent subscribes to. It is the cloud's source of truth for the agent's
// runtime and is always readable — even before the gateway is claimed.
func ConfigTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/config", Root, gatewayUID)
}

// StatePayload is the uplink homes/{uid}/state/{device_uid} body.
//
// State is HA-native — { "state": "<ha state string>", "attributes": {...} } —
// and is HA's ACTUAL state read back from HA, never an echo of the desired
// document. This is the same vocabulary DesiredShadowPayload.State uses, so
// desired and reported are directly comparable.
//
// Version echoes the desired_version the agent has applied; the cloud ingest is
// monotonic (a lower version than reported_version is dropped). Omitting Version
// (nil) means "still converged" — encoded as absent so the cloud reads it as
// pure telemetry (also what the (re)connect reconcile publishes).
type StatePayload struct {
	State   map[string]any `json:"state"`
	Version *int           `json:"version,omitempty"`
	TS      string         `json:"ts"`
}

// Severity is the constrained set the cloud maps onto EventSeverity (anything
// else falls back to info server-side).
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// EventPayload is the uplink homes/{uid}/event/{type} body. Deduped on EventID
// by the cloud, so it must be stable across QoS-1 redeliveries. Extra keys are
// allowed by the contract (the cloud stores the whole payload) but this struct
// carries the well-known ones; callers add more via a map if needed.
type EventPayload struct {
	EventID   string   `json:"event_id"`
	Severity  Severity `json:"severity"`
	DeviceUID string   `json:"device_uid,omitempty"`
	TS        string   `json:"ts"`
}

// CommandAckStatus is the terminal outcome reported for a command.
type CommandAckStatus string

const (
	AckAcked  CommandAckStatus = "acked"
	AckFailed CommandAckStatus = "failed"
)

// CommandAckPayload is the uplink homes/{uid}/cmd/ack body. Idempotent on the
// cloud side (a terminal command is not re-transitioned).
type CommandAckPayload struct {
	CmdID  string           `json:"cmd_id"`
	Status CommandAckStatus `json:"status"`
}

// CommandPayload is the downlink homes/{uid}/cmd body — a one-shot HA service
// call. Idempotent on CmdID; TTLSec is the MQTT-3.1.1 message-expiry substitute
// (past the TTL the agent drops it and the cloud expires it).
type CommandPayload struct {
	CmdID     string         `json:"cmd_id"`
	DeviceUID string         `json:"device_uid"`
	Action    string         `json:"action"`
	Params    map[string]any `json:"params"`
	TS        string         `json:"ts"`
	TTLSec    int            `json:"ttl_sec"`
	Source    string         `json:"source"`
}

// DesiredShadowPayload is the downlink retained homes/{uid}/shadow/desired body.
// State is the HA-native TARGET — { "state": "on", "attributes": {...} } — the
// same vocabulary StatePayload uses. The agent applies it (state != "off" ->
// turn_on with recognized attributes passed through, state == "off" -> turn_off)
// and then reports HA's actual resulting state on the state topic carrying the
// applied DesiredVersion (no separate ack).
type DesiredShadowPayload struct {
	DeviceUID      string         `json:"device_uid"`
	DesiredVersion int            `json:"desired_version"`
	State          map[string]any `json:"state"`
}

// InventoryEntity is one HA entity in the uplink inventory. Domain is the HA
// domain (light, switch, ...); the cloud registers a device only for
// allow-listed domains and seeds Name/Area on first sight.
//
// The HA* fields carry HA's own grouping metadata (its exact registry attribute
// keys) so the cloud can group multi-entity gadgets in the UI at render time
// without a physical device-table split: HADeviceID/HADeviceName come from HA's
// device registry, DeviceClass/UnitOfMeasurement from the entity registry. All
// are best-effort and omitted when HA does not provide them.
type InventoryEntity struct {
	EntityID          string `json:"entity_id"`
	Domain            string `json:"domain"`
	Name              string `json:"name"`
	Area              string `json:"area,omitempty"`
	HADeviceID        string `json:"ha_device_id,omitempty"`
	HADeviceName      string `json:"ha_device_name,omitempty"`
	DeviceClass       string `json:"device_class,omitempty"`
	UnitOfMeasurement string `json:"unit_of_measurement,omitempty"`
}

// InventoryPayload is the uplink retained homes/{uid}/inventory body. Hash lets
// the cloud skip re-processing an unchanged retained redelivery.
type InventoryPayload struct {
	Hash     string            `json:"hash"`
	Entities []InventoryEntity `json:"entities"`
	TS       string            `json:"ts"`
}

// ConfigEntityMapEntry binds a cloud device_uid to an HA entity_id.
type ConfigEntityMapEntry struct {
	DeviceUID string `json:"device_uid"`
	EntityID  string `json:"entity_id"`
}

// ConfigPayload is the downlink retained homes/{uid}/config body. Claimed gates
// all device work; ConfigVersion is monotonic (ignore a lower redelivery);
// EntityMap is the authoritative device_uid <-> entity_id mapping.
type ConfigPayload struct {
	Claimed       bool                   `json:"claimed"`
	ConfigVersion int                    `json:"config_version"`
	EntityMap     []ConfigEntityMapEntry `json:"entity_map"`
	TS            string                 `json:"ts"`
}
