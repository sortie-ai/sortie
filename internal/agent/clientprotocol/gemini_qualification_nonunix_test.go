//go:build !unix

package clientprotocol

import (
	"os"
	"testing"
)

// TestGeminiQualification is the non-Unix live-gate contract. With the
// Gemini qualification gate disabled it skips with the one explicit
// reason every disabled qualification scenario shares. With the gate
// enabled it fails with the exact unsupported-platform diagnostic,
// because live qualification has no Windows Job Object oracle and a
// non-Unix host records live qualification as unobserved, never as
// passed. No production procutil, Session, or adapter state changes
// here.
func TestGeminiQualification(t *testing.T) {
	if os.Getenv(geminiQualificationGateEnv) != "1" {
		t.Skip(geminiQualificationSkipReason)
	}
	t.Fatalf("%s", geminiQualificationNonUnixFailure)
}
