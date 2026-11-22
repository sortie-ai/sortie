-- The attempt counter of the episode a row belongs to, and how the last run
-- dispatched for the row's fingerprint ended, so both survive a restart.
ALTER TABLE reaction_fingerprints ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE reaction_fingerprints ADD COLUMN last_exit TEXT NOT NULL DEFAULT '';
