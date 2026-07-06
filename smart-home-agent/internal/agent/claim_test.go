package agent

import (
	"testing"
	"time"
)

func TestClaimCodeExpired(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	if !claimCodeExpired(past) {
		t.Error("expected a past expiry to be expired")
	}

	if claimCodeExpired(future) {
		t.Error("expected a future expiry to be live")
	}

	// An empty or unparseable value must never be treated as expired, so a
	// malformed timestamp does not trigger a needless reissue loop.
	if claimCodeExpired("") {
		t.Error("expected an empty expiry to be treated as not expired")
	}

	if claimCodeExpired("not-a-time") {
		t.Error("expected an unparseable expiry to be treated as not expired")
	}
}
