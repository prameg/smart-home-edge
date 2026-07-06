package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestParseTXT(t *testing.T) {
	txt := parseTXT([]string{"Location_Name=Home", "internal_url=http://192.168.1.5:8123", "malformed", "  version = 2026.7 "})

	if txt["location_name"] != "Home" {
		t.Errorf("key should be lowercased: %+v", txt)
	}
	if txt["internal_url"] != "http://192.168.1.5:8123" {
		t.Errorf("internal_url not parsed: %+v", txt)
	}
	if txt["version"] != "2026.7" {
		t.Errorf("value should be trimmed: %q", txt["version"])
	}
	if _, ok := txt["malformed"]; ok {
		t.Errorf("a record without = must be skipped: %+v", txt)
	}
}

func TestGatewayFromEntry(t *testing.T) {
	t.Run("prefers resolved IPv4 over internal_url", func(t *testing.T) {
		e := zeroconf.NewServiceEntry("Home", haServiceType, haDomain)
		e.Port = 8123
		e.AddrIPv4 = []net.IP{net.ParseIP("192.168.1.42")}
		// A HAOS docker-internal internal_url must not win over the LAN address.
		e.Text = []string{"location_name=My Home", "internal_url=http://172.30.32.1:8123"}

		gw, ok := gatewayFromEntry(e)
		if !ok {
			t.Fatal("expected a gateway")
		}
		if gw.url != "http://192.168.1.42:8123" {
			t.Errorf("should build URL from the LAN IPv4, got %q", gw.url)
		}
		if gw.name != "My Home" {
			t.Errorf("name should come from location_name, got %q", gw.name)
		}
	})

	t.Run("falls back to internal_url when no address resolved", func(t *testing.T) {
		e := zeroconf.NewServiceEntry("gw", haServiceType, haDomain)
		e.Text = []string{"internal_url=http://ha.lan:8123/"}

		gw, ok := gatewayFromEntry(e)
		if !ok {
			t.Fatal("expected a gateway")
		}
		if gw.url != "http://ha.lan:8123" {
			t.Errorf("should use internal_url (trailing slash trimmed), got %q", gw.url)
		}
		if gw.name != "gw" {
			t.Errorf("name should fall back to the instance, got %q", gw.name)
		}
	})

	t.Run("defaults the port when unset", func(t *testing.T) {
		e := zeroconf.NewServiceEntry("Home", haServiceType, haDomain)
		e.AddrIPv4 = []net.IP{net.ParseIP("10.0.0.9")}

		gw, _ := gatewayFromEntry(e)
		if gw.url != "http://10.0.0.9:8123" {
			t.Errorf("expected default port 8123, got %q", gw.url)
		}
	})

	t.Run("no address and no internal_url yields nothing", func(t *testing.T) {
		e := zeroconf.NewServiceEntry("Home", haServiceType, haDomain)

		if _, ok := gatewayFromEntry(e); ok {
			t.Error("an entry with nothing reachable must be dropped")
		}
	})
}

// probeGateways is the --dev path that finds a local HA (e.g. a NAT'd VM on
// loopback) mDNS cannot see: a live URL is returned, an unreachable one dropped.
func TestProbeGateways(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := probeGateways(context.Background(), []string{srv.URL, "http://127.0.0.1:0"})

	if len(got) != 1 || got[0].url != srv.URL {
		t.Errorf("only the reachable candidate should be returned, got %+v", got)
	}
}

func TestDedupeGateways(t *testing.T) {
	in := []gateway{
		{name: "a", url: "http://x:8123"},
		{name: "b", url: "http://x:8123"},
		{name: "c", url: "http://y:8123"},
	}
	out := dedupeGateways(in)

	if len(out) != 2 || out[0].url != "http://x:8123" || out[1].url != "http://y:8123" {
		t.Errorf("duplicates by URL should collapse in order, got %+v", out)
	}
}
