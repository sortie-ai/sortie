// Package procgroup provides the process-group liveness query and shutdown
// bound the capture harness waits under, reusing internal/agent/procutil's
// launch and signal primitives rather than reimplementing them.
package procgroup

import "time"

// ShutdownDeadline bounds how long a captured process group is polled for
// absence before the wait gives up.
const ShutdownDeadline = 30 * time.Second
