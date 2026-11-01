-- Defaults to 0: older rows counted requests whether or not measured.
ALTER TABLE session_metadata ADD COLUMN api_requests_measured INTEGER NOT NULL DEFAULT 0;
