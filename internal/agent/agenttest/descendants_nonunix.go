//go:build !unix

package agenttest

func recordDescendants(string) {}

// LiveDescendants returns nil: this platform has no process-table scan.
func LiveDescendants(int) []int { return nil }
