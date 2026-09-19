package qualification

import "time"

// ShutdownDeadline bounds how long a captured process group is polled for
// absence before the wait gives up.
const ShutdownDeadline = 30 * time.Second
