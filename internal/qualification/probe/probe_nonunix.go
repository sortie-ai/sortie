//go:build !unix

package probe

import "testing"

// qualificationNonUnixFailure is the diagnostic Run fails with on a
// platform with no Unix process-group oracle.
const qualificationNonUnixFailure = "Agent Client Protocol qualification requires a Unix process-group oracle"

// Run fails with the unsupported-oracle diagnostic: a non-Unix host has
// no process-group oracle and records live qualification as unobserved.
func Run(t *testing.T, coords Coordinates) Result {
	t.Helper()
	t.Fatalf("%s", qualificationNonUnixFailure)
	return Result{}
}
