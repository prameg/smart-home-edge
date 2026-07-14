package localmqtt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolvePrefersEnv(t *testing.T) {
	t.Setenv("LOCAL_MQTT_HOST", "localhost")
	t.Setenv("LOCAL_MQTT_PORT", "1884")
	t.Setenv("SUPERVISOR_TOKEN", "should-not-be-used")

	opts, err := Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "localhost" || opts.Port != 1884 {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestResolveViaSupervisorServices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services/mqtt" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("missing supervisor token")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": "ok",
			"data":   map[string]any{"host": "core-mosquitto", "port": 1883, "username": "addons", "password": "pw", "ssl": false},
		})
	}))
	defer server.Close()

	t.Setenv("LOCAL_MQTT_HOST", "")
	t.Setenv("SUPERVISOR_TOKEN", "tok")
	t.Setenv("SUPERVISOR_API", server.URL)

	opts, err := Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "core-mosquitto" || opts.Username != "addons" || opts.Password != "pw" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestResolveUnavailable(t *testing.T) {
	t.Setenv("LOCAL_MQTT_HOST", "")
	t.Setenv("SUPERVISOR_TOKEN", "")

	if _, err := Resolve(context.Background()); err == nil {
		t.Fatal("expected ErrUnavailable")
	}
}
