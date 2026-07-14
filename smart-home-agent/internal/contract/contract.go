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

// VersionsTopic is the uplink RETAINED software-inventory topic. The agent
// publishes it on every connect (and after an update reboot) so the cloud's
// gateways.{agent,ha,os}_version reflect what is ACTUALLY running — independent
// of provisioning, which only ran at first boot/recovery.
func VersionsTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/versions", Root, gatewayUID)
}

// UpdateTopic is the downlink one-shot UPDATE command (cloud -> agent). The
// cloud publishes it NON-RETAINED to ask the gateway to bring add-ons, HAOS,
// Core, and the agent itself to the latest available. A missed command (offline
// gateway) is fine by design: the agent's own daily self-check converges the
// unit anyway, so there is no retained doc or sequence cursor to track.
func UpdateTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/update", Root, gatewayUID)
}

// UpdateStatusTopic is the uplink fleet-update progress topic (agent -> cloud):
// started/ok/failed, for both cloud-triggered updates and the daily self-check.
func UpdateStatusTopic(gatewayUID string) string {
	return fmt.Sprintf("%s/%s/update/status", Root, gatewayUID)
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

// VersionsPayload is the uplink retained homes/{uid}/versions body: the
// gateway's currently-running software inventory. Everything runs latest, so
// there is no pinned-release cursor — these versions are pure observability.
type VersionsPayload struct {
	AgentVersion string `json:"agent_version"`
	HAVersion    string `json:"ha_version,omitempty"`
	OSVersion    string `json:"os_version,omitempty"`
	TS           string `json:"ts"`
}

// UpdatePayload is the downlink homes/{uid}/update body: a correlation id the
// agent echoes back on update/status so the cloud can tie a progress report to
// the command that triggered it. TS is advisory.
type UpdatePayload struct {
	UpdateID string `json:"update_id"`
	TS       string `json:"ts"`
}

// UpdateStatus is the constrained progress vocabulary the cloud maps onto its
// UpdateStatus enum (started -> updating, ok, failed).
type UpdateStatus string

const (
	UpdateStarted UpdateStatus = "started"
	UpdateOK      UpdateStatus = "ok"
	UpdateFailed  UpdateStatus = "failed"
)

// UpdateStatusPayload is the uplink homes/{uid}/update/status body. UpdateID
// echoes the command's id ("" for the agent's own daily self-check); Error
// carries the failure detail only when Status is failed.
type UpdateStatusPayload struct {
	UpdateID string       `json:"update_id,omitempty"`
	Status   UpdateStatus `json:"status"`
	Error    string       `json:"error,omitempty"`
	TS       string       `json:"ts"`
}

// PairingEventType is the {type} segment of the uplink event topic carrying
// pairing-session progress: homes/{uid}/event/pairing. The cloud routes it to
// the session state machine instead of the generic home-events feed.
const PairingEventType = "pairing"

// Pairing phases — the exact strings the cloud's PairingEventHandler matches.
const (
	PairingPhaseStarted      = "started"
	PairingPhaseDeviceFound  = "device_found"
	PairingPhaseInterviewing = "interviewing"
	PairingPhaseCompleted    = "completed"
	PairingPhaseFailed       = "failed"
	PairingPhaseStopped      = "stopped"
)

// PairingDevice is the protocol-specific detail of the device being paired.
// All fields best-effort; Zigbee fills them from Z2M's bridge events.
type PairingDevice struct {
	IEEE         string `json:"ieee,omitempty"`
	FriendlyName string `json:"friendly_name,omitempty"`
	Model        string `json:"model,omitempty"`
	Vendor       string `json:"vendor,omitempty"`
}

// PairingEventPayload is the uplink homes/{uid}/event/pairing body. One
// message per phase; the cloud applies them idempotently (terminal sessions
// never regress), so QoS-1 redelivery is safe without an event_id.
type PairingEventPayload struct {
	SessionID string         `json:"session_id"`
	Phase     string         `json:"phase"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Device    *PairingDevice `json:"device,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	TS        string         `json:"ts"`
}
