package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// fingerprint assumes d's slices are already in canonical order; it does
// not sort them itself, so skipping that ordering produces an unstable
// digest.
func fingerprint(d drift) (string, error) {
	encoded, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode drift for fingerprinting: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

var fingerprintMarkerRE = regexp.MustCompile(`(?m)^<!-- protocol-pin-fingerprint: (sha256:[0-9a-f]{64}) -->$`)

// extractFingerprint reads the last matching marker, not the first, so a
// stray earlier line can never shadow the one that actually governs.
func extractFingerprint(body string) (string, bool) {
	matches := fingerprintMarkerRE.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return "", false
	}
	last := matches[len(matches)-1]
	return last[1], true
}

// decideAction treats a body with no fingerprint marker as not matching:
// callers must derive bodyFingerprintMatches so that a missing marker
// yields false, or a resolved report would never reopen.
func decideAction(driftPresent bool, reportState string, bodyFingerprintMatches bool) string {
	switch {
	case !driftPresent && reportState == "open":
		return "close"
	case !driftPresent:
		return "none"
	case reportState == "absent":
		return "open"
	case bodyFingerprintMatches:
		return "none"
	case reportState == "open":
		return "update"
	default:
		return "reopen"
	}
}
