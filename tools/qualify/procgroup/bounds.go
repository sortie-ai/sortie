// Package procgroup provides the process-group liveness query, signal, and
// shutdown bound the capture harness works under. The signal addresses a
// recorded group by number after its launch ended, which the shipped
// teardown in internal/agent/procutil never does.
package procgroup

import "time"

// ShutdownDeadline bounds how long a captured process group is polled for
// absence before the wait gives up.
const ShutdownDeadline = 30 * time.Second
