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
	fmt.Fprintf(w, "  release: %s", m.ReleaseID)
	if !m.Populated {
		fmt.Fprint(w, " (template — installing latest, versions not pinned)")
	}
	fmt.Fprintln(w)
}

// printDone renders the final screen: the provisioned identity and, while
// unclaimed, the short claim code the installer reads off to bind the gateway.
func printDone(w io.Writer, st *onboard.State) {
	const line = "────────────────────────────────────────────────────"

	fmt.Fprintf(w, "\n%s\n", line)
	fmt.Fprintln(w, "  Gateway is ready.")
	fmt.Fprintf(w, "  uid:    %s\n", st.Claim.UID)

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
