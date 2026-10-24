// Package e2e provides a deterministic, isolated end-to-end harness for
// driving a real orchestrator run against a file tracker and an injected
// agent adapter; assemble a [Harness] with [NewHarness] or [NewHarnessWithAgent].
package e2e

import "time"

// ShutdownDeadline bounds how long a launch's record may take to be
// released before the wait gives up.
const ShutdownDeadline = 30 * time.Second
