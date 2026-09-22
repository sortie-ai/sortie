package agenttest

// contractReporter is the minimal reporting surface a contract check
// needs; [*testing.T] satisfies it, as does a lightweight test double.
type contractReporter interface {
	Helper()
	Errorf(format string, args ...any)
}
