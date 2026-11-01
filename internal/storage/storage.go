// Package storage defines the backend-neutral storage contracts: the records callers exchange with durable storage and, for each storage domain, the interface a backend implements. It holds no SQL and imports no database driver.
package storage

import "context"

// Pinger checks that the store can serve requests.
type Pinger interface {
	Ping(ctx context.Context) error
}
