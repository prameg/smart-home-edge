// Package backup pulls the Zigbee coordinator backup from Zigbee2MQTT over the
// gateway-local MQTT bus and shapes it for shipping to the cloud, so a dead or
// replaced hub can be restored WITHOUT physically re-pairing every device — the
// worst ops failure for an installed home.
//
// Z2M exposes a backup over its bridge API: publishing to
// `<base>/bridge/request/backup` makes it reply on `<base>/bridge/response/backup`
// with `{"status":"ok","data":{"backup":"<base64 zip of the data folder>"}}`.
// We keep only `coordinator_backup.json` (the network key + device table) — the
// smallest artifact a restore needs — extracted from that zip in memory, matching
// the cloud's `z2m-coordinator-backup` format.
package backup

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path"
)

// CoordinatorFormat tags the shipped artifact so the cloud (and a future restore)
// knows what it is. Must match the cloud's GatewayBackupController::FORMAT.
const CoordinatorFormat = "z2m-coordinator-backup"

// coordinatorBackupFile is the entry we extract from Z2M's backup zip.
const coordinatorBackupFile = "coordinator_backup.json"

// RequestTopic / ResponseTopic are Z2M's bridge backup topics for a given base
// topic (default "zigbee2mqtt").
func RequestTopic(base string) string  { return base + "/bridge/request/backup" }
func ResponseTopic(base string) string { return base + "/bridge/response/backup" }

// CoordinatorBackupFromResponse parses a Z2M `bridge/response/backup` payload and
// returns the bytes of coordinator_backup.json. It fails clearly on a non-ok
// status, an empty payload, or a zip missing the coordinator file, so the caller
// can log and retry next cycle rather than shipping garbage.
func CoordinatorBackupFromResponse(raw []byte) ([]byte, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Backup string `json:"backup"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("backup: decode response: %w", err)
	}

	// Z2M sets status "ok"/"error"; treat a present, non-ok status as a refusal.
	if resp.Status != "" && resp.Status != "ok" {
		msg := resp.Error
		if msg == "" {
			msg = resp.Status
		}

		return nil, fmt.Errorf("backup: zigbee2mqtt refused the backup: %s", msg)
	}

	if resp.Data.Backup == "" {
		return nil, fmt.Errorf("backup: response carried no backup data")
	}

	zipBytes, err := base64.StdEncoding.DecodeString(resp.Data.Backup)
	if err != nil {
		return nil, fmt.Errorf("backup: decode base64 backup: %w", err)
	}

	return extractCoordinatorBackup(zipBytes)
}

// extractCoordinatorBackup pulls coordinator_backup.json out of Z2M's backup zip.
// It matches by base name so a zip that nests the data folder still resolves.
func extractCoordinatorBackup(zipBytes []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("backup: open backup zip: %w", err)
	}

	for _, f := range zr.File {
		if path.Base(f.Name) != coordinatorBackupFile {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("backup: open %s: %w", coordinatorBackupFile, err)
		}
		defer rc.Close()

		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("backup: read %s: %w", coordinatorBackupFile, err)
		}

		return data, nil
	}

	return nil, fmt.Errorf("backup: %s not found in the backup zip", coordinatorBackupFile)
}
