package persistence

import _ "embed" // enables //go:embed directives for SQL migration files

// Migration is a single numbered schema migration applied sequentially
// by [Store.Migrate]. SQL may contain multiple semicolon-separated
// statements.
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

//go:embed sql/010_dispatch_rule_routing.sql
var migration010SQL string

//go:embed sql/011_run_history_token_columns.sql
var migration011SQL string

//go:embed sql/012_run_history_tokens_measured.sql
var migration012SQL string

//go:embed sql/013_handoff_absence_resets.sql
var migration013SQL string

//go:embed sql/014_parked_issues.sql
var migration014SQL string

//go:embed sql/015_budget_hold_notices.sql
var migration015SQL string

//go:embed sql/016_session_metadata_requests_measured.sql
var migration016SQL string

//go:embed sql/017_session_metadata_dispatch_id.sql
var migration017SQL string

//go:embed sql/018_aggregate_metrics_unmeasured_sessions.sql
var migration018SQL string

//go:embed sql/019_run_history_unaccounted_turns.sql
var migration019SQL string

//go:embed sql/020_cache_write_tokens.sql
var migration020SQL string

//go:embed sql/021_run_history_configured_settings.sql
var migration021SQL string

//go:embed sql/022_reaction_handoffs.sql
var migration022SQL string

//go:embed sql/023_stage_hops.sql
var migration023SQL string

//go:embed sql/024_stage_chains.sql
var migration024SQL string

//go:embed sql/025_issue_identity.sql
var migration025SQL string

//go:embed sql/026_retry_trigger_parked_identity.sql
var migration026SQL string

//go:embed sql/027_dispatch_context.sql
var migration027SQL string

//go:embed sql/028_run_origin.sql
var migration028SQL string

//go:embed sql/029_session_metadata_unaccounted_turns.sql
var migration029SQL string

//go:embed sql/030_issue_outcomes.sql
var migration030SQL string

//go:embed sql/031_park_and_hop_context.sql
var migration031SQL string

//go:embed sql/032_first_after_hop.sql
var migration032SQL string

//go:embed sql/033_reaction_escalations.sql
var migration033SQL string

//go:embed sql/034_attention_acknowledgements.sql
var migration034SQL string

//go:embed sql/035_run_history_display_identifier_index.sql
var migration035SQL string

//go:embed sql/036_run_events.sql
var migration036SQL string

//go:embed sql/037_stage_funnels.sql
var migration037SQL string

//go:embed sql/038_reaction_activity.sql
var migration038SQL string

//go:embed sql/039_run_history_resume_usage.sql
var migration039SQL string

//go:embed sql/040_run_model_usage.sql
var migration040SQL string

//go:embed sql/041_run_cost.sql
var migration041SQL string

//go:embed sql/042_run_history_completed_at_index.sql
var migration042SQL string

//go:embed sql/043_run_model_unrated_classes.sql
var migration043SQL string

//go:embed sql/044_reaction_triage_outcomes.sql
var migration044SQL string

//go:embed sql/045_run_declared_outcome.sql
var migration045SQL string

//go:embed sql/046_run_history_skills.sql
var migration046SQL string

//go:embed sql/047_parked_issue_release_hand_over.sql
var migration047SQL string

//go:embed sql/048_usage_limit_holds.sql
var migration048SQL string

//go:embed sql/049_drop_run_history_resume_columns.sql
var migration049SQL string

//go:embed sql/050_run_usage_provenance_and_reported_amounts.sql
var migration050SQL string

// reactionActivityMigration is the version of the migration that creates the
// reaction_activity table.
const reactionActivityMigration = 38

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
	{Version: 10, Description: "dispatch rule routing columns on retry_entries and run_history", SQL: migration010SQL},
	{Version: 11, Description: "run_history token columns", SQL: migration011SQL},
	{Version: 12, Description: "tokens_measured column on run_history", SQL: migration012SQL},
	{Version: 13, Description: "handoff absence sequence reset points", SQL: migration013SQL},
	{Version: 14, Description: "parked issues held out of primary dispatch", SQL: migration014SQL},
	{Version: 15, Description: "budget hold notices for cross-restart tracker-comment dedup", SQL: migration015SQL},
	{Version: 16, Description: "api_requests_measured column on session_metadata", SQL: migration016SQL},
	{Version: 17, Description: "dispatch_id column on session_metadata", SQL: migration017SQL},
	{Version: 18, Description: "unmeasured_sessions column on aggregate_metrics", SQL: migration018SQL},
	{Version: 19, Description: "unaccounted_turns column on run_history", SQL: migration019SQL},
	{Version: 20, Description: "cache_write_tokens column on run_history, session_metadata, and aggregate_metrics", SQL: migration020SQL},
	{Version: 21, Description: "configured model, configured effort, and reported model on run_history", SQL: migration021SQL},
	{Version: 22, Description: "review comments each issue's runs were given", SQL: migration022SQL},
	{Version: 23, Description: "stage hop counts and the previous stage on retry entries and run history", SQL: migration023SQL},
	{Version: 24, Description: "stage chain identifier and hop result on run history, retry entries, and stage hops", SQL: migration024SQL},
	{Version: 25, Description: "issue identity on retry entries and run history", SQL: migration025SQL},
	{Version: 26, Description: "retry trigger and parked issue identity", SQL: migration026SQL},
	{Version: 27, Description: "dispatch context on retry entries, run history, and stage hops", SQL: migration027SQL},
	{Version: 28, Description: "run origin on run history and retry entries", SQL: migration028SQL},
	{Version: 29, Description: "unaccounted_turns column on session_metadata", SQL: migration029SQL},
	{Version: 30, Description: "issue outcomes", SQL: migration030SQL},
	{Version: 31, Description: "run context and issue facts on parked issues and issue facts on stage hops", SQL: migration031SQL},
	{Version: 32, Description: "first run after a stage hop on run history", SQL: migration032SQL},
	{Version: 33, Description: "reaction escalation records", SQL: migration033SQL},
	{Version: 34, Description: "attention acknowledgements", SQL: migration034SQL},
	{Version: 35, Description: "display_identifier index on run_history", SQL: migration035SQL},
	{Version: 36, Description: "dispatch ID on run history and retained run events", SQL: migration036SQL},
	{Version: 37, Description: "blocked runs on run history and live passes", SQL: migration037SQL},
	{Version: reactionActivityMigration, Description: "reaction activity records per stage pass", SQL: migration038SQL},
	{Version: 39, Description: "resume usage on run history", SQL: migration039SQL},
	{Version: 40, Description: "per-model token usage of a run", SQL: migration040SQL},
	{Version: 41, Description: "recorded run cost and live session estimate", SQL: migration041SQL},
	{Version: 42, Description: "completed_at index on run_history", SQL: migration042SQL},
	{Version: 43, Description: "unrated token classes of a run's per-model cost", SQL: migration043SQL},
	{Version: 44, Description: "reaction triage answers per comment set", SQL: migration044SQL},
	{Version: 45, Description: "declared outcome and statement of a run", SQL: migration045SQL},
	{Version: 46, Description: "skill calls of a run on run history", SQL: migration046SQL},
	{Version: 47, Description: "release hand-over columns on parked issues", SQL: migration047SQL},
	{Version: 48, Description: "usage-limit holds and the usage-limit notice flag on retry entries", SQL: migration048SQL},
	{Version: 49, Description: "drop the resume columns from run history", SQL: migration049SQL},
	{Version: 50, Description: "usage provenance and completeness on run history and the amounts a run reported", SQL: migration050SQL},
}
