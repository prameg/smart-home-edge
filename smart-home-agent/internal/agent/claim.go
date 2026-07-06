package agent

import (
	"context"
	"fmt"
	"time"
)

// SurfaceClaimStatus makes the current claim state visible to a human without
// reading add-on logs. While unclaimed it ensures a live claim code (reissuing
// one if the stored code is missing or expired) and posts it to the log and an
// HA persistent notification; while claimed it clears that notification. It is
// best-effort and safe to call repeatedly — call it once at boot, and it is
// re-run on every claim/unclaim transition and on a recovery that reissues a
// code.
func (a *Agent) SurfaceClaimStatus(ctx context.Context) {
	if a.ha == nil {
		return
	}

	if a.isClaimed() {
		a.dismissClaimNotification(ctx)

		return
	}

	a.ensureClaimCode(ctx)
	a.surfaceClaimCode(ctx)
}

// ensureClaimCode reissues the short claim code when the on-device one is
// missing or expired (e.g. the cloud cleared it on an unclaim, or it simply
// timed out while the gateway sat unclaimed). Requires a stored provision token;
// best-effort otherwise.
func (a *Agent) ensureClaimCode(ctx context.Context) {
	if a.prov == nil {
		return
	}

	a.credsMu.RLock()
	code := a.creds.ClaimCode
	expires := a.creds.ClaimCodeExpiresAt
	a.credsMu.RUnlock()

	if code != "" && !claimCodeExpired(expires) {
		return
	}

	creds, err := a.prov.ReissueClaimCode(ctx)
	if err != nil {
		a.log.Warn("could not reissue claim code", "error", err)

		return
	}

	a.applyRecoveredCredentials(creds)
}

// surfaceClaimCode logs a prominent banner and posts/updates the HA persistent
// notification carrying the current claim code. No-op when there is no code.
func (a *Agent) surfaceClaimCode(ctx context.Context) {
	if a.ha == nil {
		return
	}

	a.credsMu.RLock()
	code := a.creds.ClaimCode
	a.credsMu.RUnlock()

	if code == "" {
		return
	}

	a.log.Warn("gateway is UNCLAIMED — enter this claim code to add it to a home", "claim_code", code)

	a.ha.CreatePersistentNotification(
		ctx,
		claimNotificationID,
		"Smart Home: add this gateway",
		fmt.Sprintf("Open the Smart Home app and enter claim code **%s** to add this gateway to a home.", code),
	)
}

// dismissClaimNotification clears the on-HA claim-code notification (on claim).
func (a *Agent) dismissClaimNotification(ctx context.Context) {
	if a.ha == nil {
		return
	}

	a.ha.DismissPersistentNotification(ctx, claimNotificationID)
}

// claimCodeExpired reports whether an ISO-8601 expiry is in the past. An empty
// or unparseable value is treated as "not expired" so a malformed timestamp
// never triggers a needless reissue loop.
func claimCodeExpired(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}

	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false
	}

	return time.Now().After(t)
}
