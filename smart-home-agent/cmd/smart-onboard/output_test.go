package main

import (
	"strings"
	"testing"

	"github.com/smart-home/edge/agent/internal/onboard"
)

// The done screen must surface the resolved serial so the operator can see the
// identity the gateway enrolled under (which they normally never set).
func TestPrintDoneShowsResolvedSerial(t *testing.T) {
	out := &strings.Builder{}
	st := &onboard.State{Claim: onboard.ClaimInfo{UID: "gw-uid-1", Serial: "pi-abc123", ClaimCode: "WXYZ-2345"}}

	printDone(out, st)

	if !strings.Contains(out.String(), "serial: pi-abc123") {
		t.Errorf("done screen should show the resolved serial:\n%s", out.String())
	}
}

// When the agent's log line scrolled out (no parsed serial), the done screen
// falls back to an operator-supplied override carried in the add-on options.
func TestPrintDoneFallsBackToOverriddenSerial(t *testing.T) {
	out := &strings.Builder{}
	st := &onboard.State{
		Claim:        onboard.ClaimInfo{UID: "gw-uid-1", Claimed: true},
		AgentOptions: map[string]any{"serial": "SN-override"},
	}

	printDone(out, st)

	if !strings.Contains(out.String(), "serial: SN-override") {
		t.Errorf("done screen should fall back to the overridden serial:\n%s", out.String())
	}
}

// With neither a parsed nor an overridden serial, the line is simply omitted
// rather than printing an empty/blank serial.
func TestPrintDoneOmitsSerialWhenUnknown(t *testing.T) {
	out := &strings.Builder{}
	st := &onboard.State{Claim: onboard.ClaimInfo{UID: "gw-uid-1", ClaimCode: "WXYZ-2345"}}

	printDone(out, st)

	if strings.Contains(out.String(), "serial:") {
		t.Errorf("an unknown serial should be omitted, not shown blank:\n%s", out.String())
	}
}
