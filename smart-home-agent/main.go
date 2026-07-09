// Command smart-home-agent is the Home Assistant add-on that bridges a home to
// the smart-home cloud. On boot it provisions (or reuses local credentials),
// connects to the cloud MQTT broker over TLS with a persistent session, and
// runs the contract in both directions until stopped.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/smart-home/edge/agent/internal/agent"
	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/ha"
	"github.com/smart-home/edge/agent/internal/provision"

	"github.com/google/uuid"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	log.Info("smart-home-agent starting", "version", cfg.AgentVersion, "serial", cfg.Serial)

	// Root context cancelled on SIGINT/SIGTERM so the will/graceful-offline path
	// runs on a normal add-on stop.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	haClient := ha.New(cfg, log)

	// Provision (first boot) or reuse local credentials. HA version is
	// best-effort metadata for the cloud inventory. EnsureProvisioned retries
	// with backoff (never a fatal exit) so a flaky first boot self-heals.
	prov := provision.New(cfg, log)
	creds, err := prov.EnsureProvisioned(ctx, provision.Metadata{
		HAVersion:    haClient.Version(ctx),
		AgentVersion: cfg.AgentVersion,
	})
	if err != nil {
		return fmt.Errorf("provisioning: %w", err)
	}

	log.Info("provisioned",
		"uid", creds.UID,
		"serial", creds.Serial,
		"claim_status", creds.ClaimStatus,
		"mqtt_username", creds.MQTTUsername,
	)

	a := agent.New(cfg, log, creds, haClient, prov)

	// Surface the claim state to a human: while unclaimed this shows the short
	// claim code in the log and an HA persistent notification (reissuing it if
	// stale); while claimed it clears any lingering prompt.
	a.SurfaceClaimStatus(ctx)

	// Boot event (deduped cloud-side on event_id). Dropped while unclaimed —
	// only a claimed gateway has a home to attach events to — and buffered until
	// the broker connects otherwise.
	a.PublishEvent("agent.boot", uuid.NewString(), contract.SeverityInfo, "")

	return a.Run(ctx)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level

	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warning", "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
