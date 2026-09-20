package eval

import (
	"os"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// TestQualifyReport defaults to the tracked gemini-cli capture and profile
// when its env vars are unset, so the command always renders a report.
func TestQualifyReport(t *testing.T) {
	dir := os.Getenv(captureDirEnvVar)
	if dir == "" {
		dir = chainFixtureTestdata
	}
	profilePath := os.Getenv(qualifyReemitProfileEnvVar)
	if profilePath == "" {
		profilePath = chainFixtureProfile
	}

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, profilePath)
	if err != nil {
		t.Fatalf("Load(%q, %q) error = %v, want nil", root, profilePath, err)
	}
	result, err := Run(Input{CaptureDir: dir, Profile: p})
	if err != nil {
		t.Fatalf("Run(CaptureDir: %q) error = %v, want nil", dir, err)
	}
	summary, measurement, err := Render(result, p, "2026-01-01", "report-model", "report-digest")
	if err != nil {
		t.Fatalf("Render(...) error = %v, want nil", err)
	}
	if summary == "" {
		t.Fatal("Render(...) summary = \"\", want a rendered report")
	}
	if measurement.Expectation.Verdict == "" {
		t.Error("Render(...) measurement.Expectation.Verdict = \"\", want a derived verdict")
	}
	t.Log(summary)
}
