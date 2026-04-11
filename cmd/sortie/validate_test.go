package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errWriter is a test double that always returns a fixed error from Write.
type errWriter struct{ err error }

// unknownTrackerKindWorkflow is a minimal workflow with an unregistered
// tracker kind, used to trigger the tracker_adapter preflight check.
func unknownTrackerKindWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: nonexistent
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// jiraEmptyAPIKeyWorkflow returns a workflow using the jira tracker with
// an api_key referencing SORTIE_TEST_NONEXISTENT_VAR_198, which must be
// unset (or empty) when the test runs. The jira adapter requires an API
// key, so os.ExpandEnv resolving to "" triggers tracker.api_key preflight.

// jiraEmptyAPIKeyWorkflow returns a workflow using the jira tracker with
// an api_key referencing SORTIE_TEST_NONEXISTENT_VAR_198, which must be
// unset (or empty) when the test runs. The jira adapter requires an API
// key, so os.ExpandEnv resolving to "" triggers tracker.api_key preflight.
func jiraEmptyAPIKeyWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: jira
  api_key: "$SORTIE_TEST_NONEXISTENT_VAR_198"
  project: TEST
  active_states:
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

func TestValidateValidWorkflow(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (text format produces no output on success)", stdout.String())
	}
}

func TestValidateValidWorkflowJSON(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if out.Errors == nil {
		t.Errorf("validateOutput.Errors = nil, want [] (must not be null in JSON)")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty slice", out.Errors)
	}

	// Verify the raw JSON contains "errors":[] not "errors":null.
	raw := stdout.String()
	if !strings.Contains(raw, `"errors":[]`) {
		t.Errorf("JSON output = %q, want to contain %q", raw, `"errors":[]`)
	}

	// Warnings must be a non-null empty array in JSON output.
	if out.Warnings == nil {
		t.Errorf("validateOutput.Warnings = nil, want [] (must not be null in JSON)")
	}
	if len(out.Warnings) != 0 {
		t.Errorf("validateOutput.Warnings = %v, want empty slice", out.Warnings)
	}
	if !strings.Contains(raw, `"warnings":[]`) {
		t.Errorf("JSON output = %q, want to contain %q", raw, `"warnings":[]`)
	}
}

func TestValidateDefaultPath(t *testing.T) {
	// setupRunDir sets cwd to a temp dir that contains WORKFLOW.md.
	setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// No explicit path — resolveWorkflowPath defaults to ./WORKFLOW.md.
	code := run(ctx, []string{"validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestValidateMissingFile(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "/nonexistent/sortie-test-workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate /nonexistent) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "workflow") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "workflow")
	}
}

func TestValidateMissingFileJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", "/nonexistent/sortie-test-workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json /nonexistent) = %d, want 1", code)
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
	if len(out.Errors) == 0 {
		t.Errorf("validateOutput.Errors is empty, want at least one diagnostic")
	}
	if len(out.Errors) > 0 && !strings.Contains(out.Errors[0].Check, "workflow") {
		t.Errorf("validateOutput.Errors[0].Check = %q, want to contain %q", out.Errors[0].Check, "workflow")
	}
}

func TestValidateMissingTrackerKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, noTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tracker.kind") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.kind")
	}
}

func TestValidateMissingTrackerKindJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, noTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json) = %d, want 1", code)
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}

	found := false
	for _, d := range out.Errors {
		if d.Check == "tracker.kind" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "tracker.kind")
	}
}

func TestValidateUnregisteredAdapter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tracker_adapter") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker_adapter")
	}
}

func TestValidateInvalidFormat(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "xml"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format xml) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid --format") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "invalid --format")
	}
}

func TestValidateExplicitTextFormat(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "text", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format text) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestValidateHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// --help must exit 0 — it is not a failure.
	code := run(ctx, []string{"validate", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --help) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "format") {
		t.Errorf("stdout = %q, want help text containing %q", stdout.String(), "format")
	}
}

func TestValidateUnknownFlagText(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// An unknown flag in text mode must be routed through emitDiags, not
	// printed directly by the flag package. stderr must contain the
	// "args: " prefix that emitDiags emits, and stdout must be empty.
	code := run(ctx, []string{"validate", "--unknown-flag-xyz"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --unknown-flag-xyz) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "args: ") {
		t.Errorf("stderr = %q, want to contain %q (emitDiags prefix)", stderr.String(), "args: ")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty for text-mode error", stdout.String())
	}
}

func TestValidateUnknownFlagJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// --format is parsed before --unknown-flag-xyz, so *format is "json"
	// when the parse error is returned. emitDiags must write structured
	// JSON to stdout; stderr must remain empty.
	code := run(ctx, []string{"validate", "--format", "json", "--unknown-flag-xyz"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json --unknown-flag-xyz) = %d, want 1", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for JSON-mode error", stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
	if len(out.Errors) == 0 {
		t.Errorf("validateOutput.Errors is empty, want at least one diagnostic")
	}
	if len(out.Errors) > 0 && out.Errors[0].Check != "args" {
		t.Errorf("validateOutput.Errors[0].Check = %q, want %q", out.Errors[0].Check, "args")
	}
}

func TestValidateTooManyArgs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "a.md", "b.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate a.md b.md) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "too many arguments") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "too many arguments")
	}
}

func TestValidateUnresolvedEnvVar(t *testing.T) {
	// t.Parallel omitted: t.Setenv requires a sequential test.

	// Ensure the test env var expands to empty string. Using t.Setenv
	// with "" has the same expansion result as the var being unset — both
	// cause os.ExpandEnv to produce "". t.Setenv restores the original
	// value after the test.
	t.Setenv("SORTIE_TEST_NONEXISTENT_VAR_198", "")

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, jiraEmptyAPIKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}
	// os.ExpandEnv produces "" for the unset var, then preflight check 3
	// catches the empty api_key for the jira adapter.
	if !strings.Contains(stderr.String(), "tracker.api_key") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.api_key")
	}
}

func TestValidateDoesNotCreateDB(t *testing.T) {
	wfPath := setupRunDir(t)
	wfDir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}

	// The validate subcommand must not open the database.
	dbPath := filepath.Join(wfDir, ".sortie.db")
	if _, err := os.Stat(dbPath); err == nil {
		t.Errorf("database file %s must not be created by validate subcommand", dbPath)
	}
}

func TestValidateDoesNotStartWatcher(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// The validate subcommand must return promptly — no filesystem
	// watcher goroutine is started (mgr.Start is never called).
	start := time.Now()
	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	// 30 s is generous enough to remain stable on slow CI runners while
	// still catching the case where a watcher goroutine blocks the return.
	const maxDuration = 30 * time.Second
	if elapsed > maxDuration {
		t.Errorf("run(validate) took %v, want < %v (possible watcher goroutine started)", elapsed, maxDuration)
	}
}

// --- Front matter warning integration tests ---

// typoTopLevelKeyWorkflow returns a workflow with the "trackers" typo at the
// top level (unknown_key warning) and a valid tracker.kind so preflight passes.

// --- Front matter warning integration tests ---

// typoTopLevelKeyWorkflow returns a workflow with the "trackers" typo at the
// top level (unknown_key warning) and a valid tracker.kind so preflight passes.
func typoTopLevelKeyWorkflow() []byte {
	return []byte(`---
trackers:
  kind: file
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// typoSubKeyWorkflow returns a workflow with an unknown sub-key inside the
// tracker section (unknown_sub_key warning). Preflight passes.

// typoSubKeyWorkflow returns a workflow with an unknown sub-key inside the
// tracker section (unknown_sub_key warning). Preflight passes.
func typoSubKeyWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
  typo_endpoint: "should not be here"
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// typeMismatchWorkflow returns a workflow where hooks.timeout_ms is a
// non-numeric string (type_mismatch warning). Preflight passes.

// typeMismatchWorkflow returns a workflow where hooks.timeout_ms is a
// non-numeric string (type_mismatch warning). Preflight passes.
func typeMismatchWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
hooks:
  timeout_ms: not-a-number
---
Do {{ .issue.title }}.
`)
}

// nonPositiveHooksTimeoutWorkflow returns a workflow where hooks.timeout_ms
// is -1 (semantic type_mismatch warning: non-positive). Preflight passes.

// nonPositiveHooksTimeoutWorkflow returns a workflow where hooks.timeout_ms
// is -1 (semantic type_mismatch warning: non-positive). Preflight passes.
func nonPositiveHooksTimeoutWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
hooks:
  timeout_ms: -1
---
Do {{ .issue.title }}.
`)
}

// errorAndWarningWorkflow returns a workflow with the "trackers" typo
// (warning) and no tracker.kind (error). ValidateConfigForPromotion
// passes because active_states is set; preflight fails on tracker.kind.

// errorAndWarningWorkflow returns a workflow with the "trackers" typo
// (warning) and no tracker.kind (error). ValidateConfigForPromotion
// passes because active_states is set; preflight fails on tracker.kind.
func errorAndWarningWorkflow() []byte {
	return []byte(`---
trackers:
  kind: file
tracker:
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// TestValidateWarningTypoTopLevelKeyText asserts that a typo top-level key
// produces exit 0 (valid), an empty stdout (text mode), and the warning
// written to stderr with the "warning:" prefix.

// TestValidateWarningTypoTopLevelKeyText asserts that a typo top-level key
// produces exit 0 (valid), an empty stdout (text mode), and the warning
// written to stderr with the "warning:" prefix.
func TestValidateWarningTypoTopLevelKeyText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typoTopLevelKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (text mode, no errors)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "warning:")
	}
	if !strings.Contains(stderr.String(), "trackers") {
		t.Errorf("stderr = %q, want to contain %q (typo key name)", stderr.String(), "trackers")
	}
}

// TestValidateWarningTypoTopLevelKeyJSON asserts that a typo top-level key
// in JSON mode produces exit 0, valid=true, empty errors slice, and a single
// warning diagnostic with the expected fields.

// TestValidateWarningTypoTopLevelKeyJSON asserts that a typo top-level key
// in JSON mode produces exit 0, valid=true, empty errors slice, and a single
// warning diagnostic with the expected fields.
func TestValidateWarningTypoTopLevelKeyJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typoTopLevelKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty (JSON mode, no fallback)", stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("validateOutput.Warnings = %v (len %d), want 1", out.Warnings, len(out.Warnings))
	}
	if out.Warnings[0].Severity != "warning" {
		t.Errorf("warnings[0].Severity = %q, want %q", out.Warnings[0].Severity, "warning")
	}
	if out.Warnings[0].Check != "unknown_key" {
		t.Errorf("warnings[0].Check = %q, want %q", out.Warnings[0].Check, "unknown_key")
	}
	if !strings.Contains(out.Warnings[0].Message, "trackers") {
		t.Errorf("warnings[0].Message = %q, want to contain %q", out.Warnings[0].Message, "trackers")
	}
}

// TestValidateWarningTypoSubKeyText asserts that an unknown sub-key inside
// a known section produces exit 0 and a warning on stderr.

// TestValidateWarningTypoSubKeyText asserts that an unknown sub-key inside
// a known section produces exit 0 and a warning on stderr.
func TestValidateWarningTypoSubKeyText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typoSubKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "warning:")
	}
	if !strings.Contains(stderr.String(), "unknown_sub_key") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown_sub_key")
	}
}

// TestValidateWarningTypeMismatchText asserts that a type-mismatched field
// produces exit 0 and a warning on stderr.

// TestValidateWarningTypeMismatchText asserts that a type-mismatched field
// produces exit 0 and a warning on stderr.
func TestValidateWarningTypeMismatchText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typeMismatchWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "warning:")
	}
	if !strings.Contains(stderr.String(), "type_mismatch") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "type_mismatch")
	}
}

// TestValidateWarningNonPositiveHooksTimeout asserts that a non-positive
// hooks.timeout_ms produces exit 0 and a semantic warning on stderr.

// TestValidateWarningNonPositiveHooksTimeout asserts that a non-positive
// hooks.timeout_ms produces exit 0 and a semantic warning on stderr.
func TestValidateWarningNonPositiveHooksTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, nonPositiveHooksTimeoutWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-positive") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "non-positive")
	}
}

// TestValidateErrorAndWarningsTogether asserts that a workflow with both a
// warning (typo top-level key) and an error (missing tracker.kind) produces
// exit 1 with both diagnostic categories in the JSON output.

// TestValidateErrorAndWarningsTogether asserts that a workflow with both a
// warning (typo top-level key) and an error (missing tracker.kind) produces
// exit 1 with both diagnostic categories in the JSON output.
func TestValidateErrorAndWarningsTogether(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, errorAndWarningWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json) = %d, want 1; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
	if len(out.Errors) == 0 {
		t.Errorf("validateOutput.Errors is empty, want at least one error diagnostic")
	}
	if len(out.Warnings) == 0 {
		t.Errorf("validateOutput.Warnings is empty, want at least one warning diagnostic")
	}
	// The error must be a preflight "tracker.kind" check.
	foundTrackerKind := false
	for _, d := range out.Errors {
		if d.Check == "tracker.kind" {
			foundTrackerKind = true
			if d.Severity != "error" {
				t.Errorf("errors[tracker.kind].Severity = %q, want %q", d.Severity, "error")
			}
		}
	}
	if !foundTrackerKind {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "tracker.kind")
	}
	// The warning must be an "unknown_key" for "trackers".
	foundTrackers := false
	for _, w := range out.Warnings {
		if w.Check == "unknown_key" && strings.Contains(w.Message, "trackers") {
			foundTrackers = true
			if w.Severity != "warning" {
				t.Errorf("warnings[unknown_key].Severity = %q, want %q", w.Severity, "warning")
			}
		}
	}
	if !foundTrackers {
		t.Errorf("validateOutput.Warnings = %v, want a warning with check %q containing %q", out.Warnings, "unknown_key", "trackers")
	}
}

// --- Template static analysis warning tests ---

// dotContextWorkflow returns a workflow whose prompt triggers WarnDotContext:
// .issue.title referenced inside {{ range }} where dot is the element.

// --- Template static analysis warning tests ---

// dotContextWorkflow returns a workflow whose prompt triggers WarnDotContext:
// .issue.title referenced inside {{ range }} where dot is the element.
func dotContextWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ range .issue.labels }}{{ .issue.title }}{{ end }}
`)
}

// unknownVarWorkflow returns a workflow whose prompt triggers WarnUnknownVar:
// .config is not in the template data contract.

// unknownVarWorkflow returns a workflow whose prompt triggers WarnUnknownVar:
// .config is not in the template data contract.
func unknownVarWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ .config }}
`)
}

// unknownFieldWorkflow returns a workflow whose prompt triggers WarnUnknownField:
// .run.nonexistent is not a valid sub-field of run.

// unknownFieldWorkflow returns a workflow whose prompt triggers WarnUnknownField:
// .run.nonexistent is not a valid sub-field of run.
func unknownFieldWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ .run.nonexistent }}
`)
}

// multipleTemplateWarningWorkflow returns a workflow whose prompt triggers
// both WarnDotContext (.issue.title inside range) and WarnUnknownVar ($.config).

// multipleTemplateWarningWorkflow returns a workflow whose prompt triggers
// both WarnDotContext (.issue.title inside range) and WarnUnknownVar ($.config).
func multipleTemplateWarningWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ range .issue.labels }}{{ .issue.title }}{{ $.config }}{{ end }}
`)
}

// TestValidateTemplateDotContextText verifies that a dot-context misuse
// produces exit 0, empty stdout, and a "dot_context" warning on stderr.

// TestValidateTemplateDotContextText verifies that a dot-context misuse
// produces exit 0, empty stdout, and a "dot_context" warning on stderr.
func TestValidateTemplateDotContextText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, dotContextWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (text mode, warnings only)", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Errorf("stderr = %q, want to contain %q", got, "warning:")
	}
	if !strings.Contains(got, "dot_context") {
		t.Errorf("stderr = %q, want to contain %q", got, "dot_context")
	}
}

// TestValidateTemplateDotContextJSON verifies that a dot-context misuse
// produces valid=true, empty errors, and a warning with check="dot_context"
// in JSON output.

// TestValidateTemplateDotContextJSON verifies that a dot-context misuse
// produces valid=true, empty errors, and a warning with check="dot_context"
// in JSON output.
func TestValidateTemplateDotContextJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, dotContextWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
	}
	found := false
	for _, w := range out.Warnings {
		if w.Check == "dot_context" {
			found = true
			if w.Severity != "warning" {
				t.Errorf("warnings[dot_context].Severity = %q, want %q", w.Severity, "warning")
			}
		}
	}
	if !found {
		t.Errorf("validateOutput.Warnings = %v, want at least one entry with check=%q", out.Warnings, "dot_context")
	}
}

// TestValidateTemplateUnknownVarText verifies that an unknown top-level
// variable produces exit 0 and an "unknown_var" warning on stderr.

// TestValidateTemplateUnknownVarText verifies that an unknown top-level
// variable produces exit 0 and an "unknown_var" warning on stderr.
func TestValidateTemplateUnknownVarText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownVarWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Errorf("stderr = %q, want to contain %q", got, "warning:")
	}
	if !strings.Contains(got, "unknown_var") {
		t.Errorf("stderr = %q, want to contain %q", got, "unknown_var")
	}
}

// TestValidateTemplateUnknownVarJSON verifies that an unknown top-level
// variable produces valid=true and a warning with check="unknown_var" in JSON.

// TestValidateTemplateUnknownVarJSON verifies that an unknown top-level
// variable produces valid=true and a warning with check="unknown_var" in JSON.
func TestValidateTemplateUnknownVarJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownVarWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	found := false
	for _, w := range out.Warnings {
		if w.Check == "unknown_var" {
			found = true
		}
	}
	if !found {
		t.Errorf("validateOutput.Warnings = %v, want at least one entry with check=%q", out.Warnings, "unknown_var")
	}
}

// TestValidateTemplateUnknownFieldText verifies that an unknown sub-field
// produces exit 0 and an "unknown_field" warning on stderr.

// TestValidateTemplateUnknownFieldText verifies that an unknown sub-field
// produces exit 0 and an "unknown_field" warning on stderr.
func TestValidateTemplateUnknownFieldText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownFieldWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Errorf("stderr = %q, want to contain %q", got, "warning:")
	}
	if !strings.Contains(got, "unknown_field") {
		t.Errorf("stderr = %q, want to contain %q", got, "unknown_field")
	}
}

// TestValidateTemplateUnknownFieldJSON verifies that an unknown sub-field
// produces valid=true and a warning with check="unknown_field" in JSON.

// TestValidateTemplateUnknownFieldJSON verifies that an unknown sub-field
// produces valid=true and a warning with check="unknown_field" in JSON.
func TestValidateTemplateUnknownFieldJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownFieldWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	found := false
	for _, w := range out.Warnings {
		if w.Check == "unknown_field" {
			found = true
		}
	}
	if !found {
		t.Errorf("validateOutput.Warnings = %v, want at least one entry with check=%q", out.Warnings, "unknown_field")
	}
}

// TestValidateTemplateMultipleWarnings verifies that a prompt triggering
// multiple warning classes reports all of them without changing the exit code.

// TestValidateTemplateMultipleWarnings verifies that a prompt triggering
// multiple warning classes reports all of them without changing the exit code.
func TestValidateTemplateMultipleWarnings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, multipleTemplateWarningWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
	}
	hasDotContext := false
	hasUnknownVar := false
	for _, w := range out.Warnings {
		if w.Check == "dot_context" {
			hasDotContext = true
		}
		if w.Check == "unknown_var" {
			hasUnknownVar = true
		}
	}
	if !hasDotContext {
		t.Errorf("validateOutput.Warnings = %v, want at least one %q warning", out.Warnings, "dot_context")
	}
	if !hasUnknownVar {
		t.Errorf("validateOutput.Warnings = %v, want at least one %q warning", out.Warnings, "unknown_var")
	}
}

// TestValidateTemplateCleanNoWarnings verifies that a well-formed workflow
// produces no template warnings.

// TestValidateTemplateCleanNoWarnings verifies that a well-formed workflow
// produces no template warnings.
func TestValidateTemplateCleanNoWarnings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, minimalWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	got := stderr.String()
	if strings.Contains(got, "dot_context") || strings.Contains(got, "unknown_var") || strings.Contains(got, "unknown_field") {
		t.Errorf("stderr = %q, want no template warnings for a clean workflow", got)
	}
}

// --- writeJSON / emitDiags error-path tests ---

// --- writeJSON / emitDiags error-path tests ---

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	t.Run("success returns nil", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		if err := writeJSON(&buf, validateOutput{Valid: true, Errors: []validateDiag{}}); err != nil {
			t.Errorf("writeJSON() unexpected error: %v", err)
		}
		if buf.Len() == 0 {
			t.Error("writeJSON() wrote nothing to the buffer")
		}
	})

	t.Run("writer failure is returned as error", func(t *testing.T) {
		t.Parallel()

		w := errWriter{err: fmt.Errorf("disk full")}
		if err := writeJSON(w, validateOutput{Valid: false, Errors: []validateDiag{}}); err == nil {
			t.Error("writeJSON() expected error from failing writer, got nil")
		}
	})
}

func TestEmitDiagsJSONFallback(t *testing.T) {
	t.Parallel()

	// When stdout fails to accept JSON, emitDiags must fall back to
	// plain-text diagnostics on stderr so the caller still sees the error.
	diags := []validateDiag{
		{Severity: "error", Check: "tracker.kind", Message: "tracker kind is required"},
	}
	var stderr bytes.Buffer
	emitDiags(errWriter{err: fmt.Errorf("disk full")}, &stderr, "json", diags, nil)

	got := stderr.String()
	if !strings.Contains(got, "tracker.kind") {
		t.Errorf("stderr = %q, want to contain %q (fallback text)", got, "tracker.kind")
	}
	if !strings.Contains(got, "tracker kind is required") {
		t.Errorf("stderr = %q, want to contain %q (fallback text)", got, "tracker kind is required")
	}
}

func TestRunValidateJSONSuccessStdoutFails(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	//
	// When the success-path JSON write fails and there are no errors or
	// warnings to fall back on, emitDiags has nothing to print to stderr.
	// runValidate still returns 0 (the workflow is valid; the I/O failure
	// is best-effort output delivery).
	wfPath := setupRunDir(t)

	var stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath},
		errWriter{err: fmt.Errorf("disk full")}, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) with failing stdout = %d, want 0; stderr: %s",
			code, stderr.String())
	}
	// No per-diag fallback lines when there are no errors or warnings.
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty (no diags to fall back on)", stderr.String())
	}
}

// --- OS signal and server shutdown edge-case tests ---

// TestRunSIGINTCleanShutdown verifies that run() returns 0 when the process
// receives SIGINT via signal.NotifyContext. Uses the helper-subprocess
// pattern to avoid delivering OS signals to the test runner's own process.
//
// Subprocess mode is activated by SORTIE_TEST_SIGINT_HELPER=1.

// --- GitHub validate tests ---

// githubInvalidProjectWorkflow is a minimal GitHub workflow where
// tracker.project is not in owner/repo format (no slash), used to
// trigger the tracker.project.format preflight diagnostic.
func githubInvalidProjectWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: github
  api_key: "tok"
  project: "notvalid"
  active_states:
    - backlog
  terminal_states:
    - done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// githubStateOverlapWorkflow is a minimal GitHub workflow where
// active_states and terminal_states overlap on "done", used to
// trigger the tracker.states.overlap warning.

// githubStateOverlapWorkflow is a minimal GitHub workflow where
// active_states and terminal_states overlap on "done", used to
// trigger the tracker.states.overlap warning.
func githubStateOverlapWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: github
  api_key: "tok"
  project: "sortie-ai/sortie"
  active_states:
    - backlog
    - done
  terminal_states:
    - done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// githubMissingAPIKeyWorkflow is a minimal GitHub workflow where
// tracker.api_key references an unset environment variable so it
// resolves to empty, used to trigger the api_key preflight error and
// the tracker.api_key.github_token_hint warning.

// githubMissingAPIKeyWorkflow is a minimal GitHub workflow where
// tracker.api_key references an unset environment variable so it
// resolves to empty, used to trigger the api_key preflight error and
// the tracker.api_key.github_token_hint warning.
func githubMissingAPIKeyWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: github
  api_key: "$SORTIE_TEST_NONEXISTENT_VAR_303"
  project: "sortie-ai/sortie"
  active_states:
    - backlog
  terminal_states:
    - done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// TestValidateGitHubInvalidProject verifies that sortie validate exits 1
// and emits a tracker.project.format error when tracker.project is not
// in owner/repo format.

// TestValidateGitHubInvalidProject verifies that sortie validate exits 1
// and emits a tracker.project.format error when tracker.project is not
// in owner/repo format.
func TestValidateGitHubInvalidProject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, githubInvalidProjectWorkflow())

	t.Run("text output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "tracker.project.format") {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.project.format")
		}
	})

	t.Run("json output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(validate --format json) = %d, want 1; stderr: %s", code, stderr.String())
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
		}
		if out.Valid {
			t.Errorf("validateOutput.Valid = true, want false")
		}

		found := false
		for _, e := range out.Errors {
			if e.Check == "tracker.project.format" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("validateOutput.Errors = %v, want entry with check %q", out.Errors, "tracker.project.format")
		}
	})
}

// TestValidateGitHubStateOverlapWarning verifies that sortie validate exits 0
// with a tracker.states.overlap warning when active_states and terminal_states
// share a label.

// TestValidateGitHubStateOverlapWarning verifies that sortie validate exits 0
// with a tracker.states.overlap warning when active_states and terminal_states
// share a label.
func TestValidateGitHubStateOverlapWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, githubStateOverlapWorkflow())

	t.Run("text output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "tracker.states.overlap") {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.states.overlap")
		}
	})

	t.Run("json output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
		}
		if !out.Valid {
			t.Errorf("validateOutput.Valid = false, want true")
		}
		if len(out.Errors) != 0 {
			t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
		}

		found := false
		for _, w := range out.Warnings {
			if w.Check == "tracker.states.overlap" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("validateOutput.Warnings = %v, want entry with check %q", out.Warnings, "tracker.states.overlap")
		}
	})
}

// TestValidateGitHubTokenHintWarning verifies that sortie validate exits 1
// (generic tracker.api_key error) and also emits the
// tracker.api_key.github_token_hint advisory warning when GITHUB_TOKEN is set.

// TestValidateGitHubTokenHintWarning verifies that sortie validate exits 1
// (generic tracker.api_key error) and also emits the
// tracker.api_key.github_token_hint advisory warning when GITHUB_TOKEN is set.
func TestValidateGitHubTokenHintWarning(t *testing.T) {
	// No t.Parallel(): uses t.Setenv to control GITHUB_TOKEN.
	t.Setenv("GITHUB_TOKEN", "ghp_test_token_validate_hint")

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, githubMissingAPIKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json) = %d, want 1 (generic api_key error); stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}

	// Generic tracker.api_key error must be present.
	foundErr := false
	for _, e := range out.Errors {
		if e.Check == "tracker.api_key" {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("validateOutput.Errors = %v, want entry with check %q", out.Errors, "tracker.api_key")
	}

	// GITHUB_TOKEN hint warning must also be present.
	foundWarn := false
	for _, w := range out.Warnings {
		if w.Check == "tracker.api_key.github_token_hint" {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("validateOutput.Warnings = %v, want entry with check %q", out.Warnings, "tracker.api_key.github_token_hint")
	}
}

// --- HTTP Server Always-On integration tests ---

func TestValidateShortHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run([validate -h]) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--format") {
		t.Errorf("run([validate -h]) stdout = %q, want to contain %q", stdout.String(), "--format")
	}
	if stderr.Len() != 0 {
		t.Errorf("run([validate -h]) stderr = %q, want empty", stderr.String())
	}
}
