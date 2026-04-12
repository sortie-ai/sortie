package persistence

import _ "embed" // enables //go:embed directives for SQL migration files

// Migration represents a single numbered schema migration. Migrations are
// applied sequentially by [Store.Migrate]. The SQL field may contain multiple
// statements separated by semicolons.
type Migration struct {
	Version     int
	Description string
	SQL         string
}

//go:embed sql/001_initial.sql
var migration001SQL string

//go:embed sql/002_extended_token_metrics.sql
var migration002SQL string

//go:embed sql/003_workflow_file.sql
var migration003SQL string

//go:embed sql/004_run_history_issue_id_index.sql
var migration004SQL string

//go:embed sql/005_run_history_turns.sql
var migration005SQL string

//go:embed sql/006_display_identifier.sql
var migration006SQL string

//go:embed sql/007_self_review.sql
var migration007SQL string

//go:embed sql/008_reaction_fingerprints.sql
var migration008SQL string

//go:embed sql/009_retry_session_id.sql
var migration009SQL string

var migrations = []Migration{
	{Version: 1, Description: "core persistence tables", SQL: migration001SQL},
	{Version: 2, Description: "extended token metrics", SQL: migration002SQL},
	{Version: 3, Description: "workflow file in run history", SQL: migration003SQL},
	{Version: 4, Description: "run_history issue_id index", SQL: migration004SQL},
	{Version: 5, Description: "turns completed in run history", SQL: migration005SQL},
	{Version: 6, Description: "display identifier in run history", SQL: migration006SQL},
	{Version: 7, Description: "self-review metadata in run history", SQL: migration007SQL},
	{Version: 8, Description: "reaction fingerprints for cross-restart dedup", SQL: migration008SQL},
	{Version: 9, Description: "session ID in retry entries for cross-retry resume", SQL: migration009SQL},
}
