package backup

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// makeResponse builds a Z2M bridge/response/backup payload whose zip contains the
// given files (name -> contents).
func makeResponse(t *testing.T, status string, files map[string]string) []byte {
	t.Helper()

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	resp := map[string]any{
		"status": status,
		"data":   map[string]any{"backup": base64.StdEncoding.EncodeToString(zbuf.Bytes())},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	return raw
}

func TestCoordinatorBackupFromResponse(t *testing.T) {
	want := `{"metadata":{"format":"zigbee"},"network_key":"secret"}`

	t.Run("extracts coordinator_backup.json from the zip", func(t *testing.T) {
		raw := makeResponse(t, "ok", map[string]string{
			"coordinator_backup.json": want,
			"configuration.yaml":      "serial:\n  port: /dev/ttyACM0\n",
		})

		got, err := CoordinatorBackupFromResponse(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("matches a nested data-folder path by base name", func(t *testing.T) {
		raw := makeResponse(t, "ok", map[string]string{
			"data/coordinator_backup.json": want,
		})

		got, err := CoordinatorBackupFromResponse(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("accepts an empty status (older Z2M) as success", func(t *testing.T) {
		raw := makeResponse(t, "", map[string]string{"coordinator_backup.json": want})

		if _, err := CoordinatorBackupFromResponse(raw); err != nil {
			t.Errorf("empty status should be treated as ok: %v", err)
		}
	})

	t.Run("errors on a non-ok status", func(t *testing.T) {
		raw := []byte(`{"status":"error","error":"adapter not connected"}`)

		_, err := CoordinatorBackupFromResponse(raw)
		if err == nil || !strings.Contains(err.Error(), "adapter not connected") {
			t.Errorf("expected the refusal reason surfaced, got %v", err)
		}
	})

	t.Run("errors when the zip lacks the coordinator file", func(t *testing.T) {
		raw := makeResponse(t, "ok", map[string]string{"configuration.yaml": "x"})

		if _, err := CoordinatorBackupFromResponse(raw); err == nil {
			t.Error("expected an error when coordinator_backup.json is absent")
		}
	})

	t.Run("errors on empty backup data", func(t *testing.T) {
		if _, err := CoordinatorBackupFromResponse([]byte(`{"status":"ok","data":{"backup":""}}`)); err == nil {
			t.Error("expected an error on empty backup data")
		}
	})

	t.Run("errors on malformed base64", func(t *testing.T) {
		if _, err := CoordinatorBackupFromResponse([]byte(`{"status":"ok","data":{"backup":"@@@"}}`)); err == nil {
			t.Error("expected an error on malformed base64")
		}
	})

	t.Run("errors on malformed json", func(t *testing.T) {
		if _, err := CoordinatorBackupFromResponse([]byte(`not json`)); err == nil {
			t.Error("expected an error on malformed json")
		}
	})
}

func TestBackupTopics(t *testing.T) {
	if got := RequestTopic("zigbee2mqtt"); got != "zigbee2mqtt/bridge/request/backup" {
		t.Errorf("RequestTopic = %q", got)
	}
	if got := ResponseTopic("zigbee2mqtt"); got != "zigbee2mqtt/bridge/response/backup" {
		t.Errorf("ResponseTopic = %q", got)
	}
}
