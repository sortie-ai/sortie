-- Migration 16: record whether a session's API request count is a measurement
--
-- The default is 0, unlike run_history.tokens_measured's default of 1.
-- session_metadata holds one current-state row per issue that the next
-- session for that issue overwrites, and a pre-migration row's count was
-- written unconditionally, so defaulting to 1 would preserve exactly the
-- unmeasured figure this column exists to mark. A pre-migration row
-- reads 0 and self-heals on that issue's next write.

ALTER TABLE session_metadata ADD COLUMN api_requests_measured INTEGER NOT NULL DEFAULT 0;
