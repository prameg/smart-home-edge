package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// haServiceType is the mDNS/DNS-SD service Home Assistant advertises itself as
// (see HA's zeroconf integration). Every HAOS gateway on the LAN announces it,
// carrying the instance name, port, and a base/internal URL in its TXT record —
// enough to point the CLI at a freshly flashed device with zero configuration.
const (
	haServiceType = "_home-assistant._tcp"
	haDomain      = "local."
	haDefaultPort = 8123
)

// gateway is one discovered Home Assistant instance: a human label and the base
// URL to drive it at.
type gateway struct {
	name string
	url  string
}

// discoverGateways browses the LAN for Home Assistant instances over mDNS for up
// to timeout, returning the unique gateways found (empty, not an error, when
// none answer within the window). Discovery is inherently best-effort — a quiet
// network or an mDNS-unfriendly link simply yields nothing and the caller falls
// back to a prompt/default.
func discoverGateways(ctx context.Context, timeout time.Duration) ([]gateway, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mDNS resolver: %w", err)
	}

	browseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry)

	var (
		mu    sync.Mutex
		found = map[string]gateway{}
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for entry := range entries {
			if gw, ok := gatewayFromEntry(entry); ok {
				mu.Lock()
				found[gw.url] = gw
				mu.Unlock()
			}
		}
	}()

	if err := resolver.Browse(browseCtx, haServiceType, haDomain, entries); err != nil {
		return nil, fmt.Errorf("mDNS browse: %w", err)
	}

	<-browseCtx.Done()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	out := make([]gateway, 0, len(found))
	for _, gw := range found {
		out = append(out, gw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out, nil
}

// gatewayFromEntry turns an mDNS service entry into a reachable gateway URL. It
// prefers a resolved LAN IPv4 over the announced internal_url TXT value, because
// on HAOS that TXT can carry a Docker-internal address (172.30.x) that is not
// reachable from the operator's machine (home-assistant/core#59553), whereas the
// A record is the interface HAOS chose to announce on.
func gatewayFromEntry(e *zeroconf.ServiceEntry) (gateway, bool) {
	if e == nil {
		return gateway{}, false
	}

	txt := parseTXT(e.Text)

	name := txt["location_name"]
	if name == "" {
		name = e.Instance
	}
	if name == "" {
		name = e.HostName
	}

	port := e.Port
	if port == 0 {
		port = haDefaultPort
	}

	switch {
	case len(e.AddrIPv4) > 0:
		return gateway{name: name, url: fmt.Sprintf("http://%s", net.JoinHostPort(e.AddrIPv4[0].String(), strconv.Itoa(port)))}, true
	case txt["internal_url"] != "":
		return gateway{name: name, url: strings.TrimRight(txt["internal_url"], "/")}, true
	case len(e.AddrIPv6) > 0:
		return gateway{name: name, url: fmt.Sprintf("http://%s", net.JoinHostPort(e.AddrIPv6[0].String(), strconv.Itoa(port)))}, true
	default:
		return gateway{}, false
	}
}

// devDefaultHost is where a developer's Home Assistant almost always sits: a
// VirtualBox/UTM VM NATs guest:8123 onto the host's loopback.
const devDefaultHost = "http://127.0.0.1:8123"

// devCandidates are the well-known local URLs a developer's HA is reachable at
// but mDNS cannot surface: a NAT'd VM answers on the host's loopback while its
// mDNS announcement carries the guest's internal IP, not 127.0.0.1.
var devCandidates = []string{
	devDefaultHost,
	"http://homeassistant.local:8123",
}

// probeGateways probes each host and returns the ones that answer, for the
// --dev path where mDNS is blind to a NAT'd VM. Probes run concurrently so a
// slow/absent candidate never holds up a reachable one.
func probeGateways(ctx context.Context, hosts []string) []gateway {
	found := make([]gateway, len(hosts))

	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			if probeGateway(ctx, h) {
				found[i] = gateway{name: "Home Assistant (local)", url: strings.TrimRight(h, "/")}
			}
		}(i, h)
	}
	wg.Wait()

	out := make([]gateway, 0, len(hosts))
	for _, g := range found {
		if g.url != "" {
			out = append(out, g)
		}
	}

	return out
}

// probeGateway reports whether something answers HA's onboarding endpoint at
// host. Any HTTP response counts as "a gateway is here" — a warming-up Core may
// reply 401/503, and the connect step handles that wait; we only need to know
// the address is live.
func probeGateway(ctx context.Context, host string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(host, "/")+"/api/onboarding", nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()

	return true
}

// dedupeGateways drops repeated URLs while preserving order (mDNS + local probes
// can surface the same instance).
func dedupeGateways(in []gateway) []gateway {
	seen := make(map[string]struct{}, len(in))
	out := make([]gateway, 0, len(in))
	for _, g := range in {
		if _, ok := seen[g.url]; ok {
			continue
		}
		seen[g.url] = struct{}{}
		out = append(out, g)
	}

	return out
}

// parseTXT turns DNS-SD "key=value" TXT strings into a map (keys lowercased).
func parseTXT(records []string) map[string]string {
	out := make(map[string]string, len(records))
	for _, r := range records {
		k, v, ok := strings.Cut(r, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}

	return out
}
