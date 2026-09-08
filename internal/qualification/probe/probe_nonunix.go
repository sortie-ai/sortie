//go:build !unix

package probe

import "testing"

// qualificationNonUnixFailure is the exact diagnostic Run fails with on
// a platform with no Unix process-group oracle.
const qualificationNonUnixFailure = "Agent Client Protocol qualification requires a Unix process-group oracle"

// Run fails with the platform's unsupported-oracle diagnostic: live
// qualification has no Windows Job Object oracle, and a non-Unix host
// records live qualification as unobserved rather than passed. No
// production procutil, session, or adapter state changes here.
func Run(t *testing.T, coords Coordinates) Result {
	t.Helper()
	t.Fatalf("%s", qualificationNonUnixFailure)
	return Result{}
}
