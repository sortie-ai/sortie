package main

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/storage/sqlite"
)

// openStore opens the database at path for writing and applies every pending
// migration. On failure it logs the failed step, closes what it opened, and
// returns nil and false. The caller closes a returned store.
func openStore(ctx context.Context, path string, logger *slog.Logger) (*sqlite.Store, bool) {
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		logger.Error("failed to open database", slog.Any("error", err))
		return nil, false
	}
	if err := store.Migrate(ctx); err != nil {
		logger.Error("failed to migrate database", slog.Any("error", err))
		store.Close() //nolint:errcheck,gosec // best-effort cleanup after a failed start
		return nil, false
	}
	return store, true
}

// openStoreReadOnly opens an existing database for reading only and never
// migrates it, so a process that only reads cannot change the schema.
func openStoreReadOnly(ctx context.Context, path string) (*sqlite.Store, error) {
	return sqlite.OpenReadOnly(ctx, path)
}
