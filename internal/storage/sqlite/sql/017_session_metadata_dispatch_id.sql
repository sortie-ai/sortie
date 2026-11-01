-- Defaults to '' so an older row never matches a running dispatch.
ALTER TABLE session_metadata ADD COLUMN dispatch_id TEXT NOT NULL DEFAULT '';
