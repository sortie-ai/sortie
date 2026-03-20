package example

import (
	"os"
	"testing"
)

// skipUnlessIntegration skips the current test when the SORTIE_EXAMPLE_TEST
// environment variable is not set to "1", so disabled integration tests are
// reported as skipped rather than silently passing.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_EXAMPLE_TEST") != "1" {
		t.Skip("skipping Example integration test: set SORTIE_EXAMPLE_TEST=1 to enable")
	}
}

// requireEnv reads an environment variable and fails the test when empty.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// integrationConfig builds the adapter config map from environment variables.
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		// TODO: populate from requireEnv calls
	}
}

func TestIntegration_SmokeFetch(t *testing.T) {
	skipUnlessIntegration(t)

	cfg := integrationConfig(t)
	_ = cfg
	// TODO: create adapter, exercise a basic operation, assert on result
}
