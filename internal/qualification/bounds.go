package qualification

import "time"

// ShutdownDeadline is the shared shutdown bound: once it starts, every
// captured process group is polled until the kernel reports its
// absence or this deadline expires, whichever comes first.
const ShutdownDeadline = 30 * time.Second
