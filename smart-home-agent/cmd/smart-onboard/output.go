package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/smart-home/edge/agent/fleet"
	"github.com/smart-home/edge/agent/internal/onboard"
)

// reporter renders the engine's progress as a plain, dependency-free transcript
// to a writer. It is safe for the engine's single-goroutine step sequence; the
// mutex only guards against interleaving if that ever changes.
type reporter struct {
	w   io.Writer
	mu  sync.Mutex
	n   int
	t0  time.Time
	cur string
}

func newReporter(w io.Writer) *reporter {
	return &reporter{w: w, t0: time.Now()}
}

func (r *reporter) StepStarted(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.n++
	r.cur = name
	fmt.Fprintf(r.w, "\n[%d] %s\n", r.n, name)
}

func (r *reporter) StepSkipped(_, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.w, "    - skipped (%s)\n", reason)
}

func (r *reporter) StepCompleted(_ string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.w, "    - done\n")
}

func (r *reporter) StepFailed(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.w, "    ! failed: %v\n", err)
	_ = name
}

func (r *reporter) Info(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.w, "    · %s\n", msg)
}

// printHeader shows what this run is targeting before any work begins.
func printHeader(w io.Writer, opts options, m *fleet.Manifest) {
	fmt.Fprintln(w, "smart-onboard — Home Assistant gateway onboarding")
	fmt.Fprintf(w, "  device:  %s\n", opts.host)
	fmt.Fprintf(w, "  cloud:   %s\n", opts.cloudBaseURL)
	fmt.Fprintf(w, "  broker:  %s:%d (tls=%v)\n", opts.mqttHost, opts.mqttPort, opts.mqttTLS)
	fmt.Fprintf(w, "  addons:  %d (latest; auto-updated by the agent + HA)\n", len(m.Addons))
}

// printDone renders the final screen: the provisioned identity and, while
// unclaimed, the short claim code the installer reads off to bind the gateway.
func printDone(w io.Writer, st *onboard.State) {
	const line = "────────────────────────────────────────────────────"

	fmt.Fprintf(w, "\n%s\n", line)
	fmt.Fprintln(w, "  Gateway is ready.")
	fmt.Fprintf(w, "  uid:    %s\n", st.Claim.UID)

	if serial := resolvedSerial(st); serial != "" {
		fmt.Fprintf(w, "  serial: %s\n", serial)
	}

	if st.Claim.Claimed {
		fmt.Fprintln(w, "  status: already claimed — no code needed.")
	} else if st.Claim.ClaimCode != "" {
		fmt.Fprintln(w, "  status: unclaimed")
		fmt.Fprintf(w, "\n  CLAIM CODE:  %s\n", st.Claim.ClaimCode)
		fmt.Fprintln(w, "\n  Open the Smart Home app and enter this code to add the gateway to a home.")
	} else {
		fmt.Fprintln(w, "  status: unclaimed (no claim code surfaced yet — check the add-on log/notification)")
	}

	fmt.Fprintf(w, "%s\n", line)
}

// resolvedSerial is the hardware serial to show on the done screen. The ground
// truth is what the agent logged it enrolled under (st.Claim.Serial); if that
// scrolled out of the log window we fall back to an operator-supplied override
// (--serial, carried in the add-on options), which is the same value the agent
// would have used.
func resolvedSerial(st *onboard.State) string {
	if st.Claim.Serial != "" {
		return st.Claim.Serial
	}

	if s, ok := st.AgentOptions["serial"].(string); ok {
		return s
	}

	return ""
}
