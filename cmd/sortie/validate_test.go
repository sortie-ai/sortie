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

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
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

// allContinuationKeysWorkflow returns a workflow whose prompt references all
// six reaction continuation keys under top-level {{ if }} guards, one
// documented sub-field of each of the four map-shaped keys, one element
// field of each of the two list-shaped keys inside {{ range }}, and exactly
// one unrecognized name, .not_a_reaction. The front matter configures no
// reaction block, so a clean result here also demonstrates that the
// recognized set does not depend on which reactions are configured.
func allContinuationKeysWorkflow() []byte {
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
{{ if .ci_failure }}x{{ end }}
{{ if .review_comments }}x{{ end }}
{{ if .bot_review_comments }}x{{ end }}
{{ if .merge_conflict }}x{{ end }}
{{ if .label_review }}x{{ end }}
{{ if .label_fix }}x{{ end }}
{{ .ci_failure.status }}
{{ .merge_conflict.base }}
{{ .label_review.actor }}
{{ .label_fix.branch }}
{{ range .review_comments }}{{ .body }}{{ end }}
{{ range .bot_review_comments }}{{ .body }}{{ end }}
{{ .not_a_reaction }}
`)
}

// TestValidateTemplateContinuationKeysCleanJSON verifies that sortie
// validate reports no unknown_var, unknown_field, or dot_context warning
// for any of the six reaction continuation keys or their documented
// fields, while an unrelated unrecognized name still produces exactly one
// unknown_var warning naming the full enumerated set of recognized names.
func TestValidateTemplateContinuationKeysCleanJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, allContinuationKeysWorkflow())

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

	const wantMessage = `unknown template variable ".not_a_reaction"; valid top-level variables are: .issue, .attempt, .run, .ci_failure, .review_comments, .bot_review_comments, .merge_conflict, .label_review, .label_fix`

	var unknownVarWarnings []validateDiag
	for _, w := range out.Warnings {
		switch w.Check {
		case "unknown_var":
			unknownVarWarnings = append(unknownVarWarnings, w)
		case "unknown_field", "dot_context":
			t.Errorf("validateOutput.Warnings contains check=%q: %v, want no %q or %q warnings for the continuation keys and their documented fields",
				w.Check, w, "unknown_field", "dot_context")
		}
	}
	if len(unknownVarWarnings) != 1 {
		t.Fatalf("validateOutput.Warnings has %d unknown_var entries, want exactly 1: %v", len(unknownVarWarnings), unknownVarWarnings)
	}
	got := unknownVarWarnings[0]
	if !strings.Contains(got.Message, ".not_a_reaction") {
		t.Errorf("unknown_var warning message = %q, want to contain %q", got.Message, ".not_a_reaction")
	}
	if got.Message != wantMessage {
		t.Errorf("unknown_var warning message = %q, want %q", got.Message, wantMessage)
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

// --- dispatch-specific validate tests ---

// makeDispatchWorkflow writes a WORKFLOW.md to dir with the given dispatch
// section content and returns the absolute path to the workflow file.
// The workflow includes the minimum required fields so that
// ValidateConfigForPromotion and the preflight checks are satisfied:
// tracker.kind, tracker.active_states, tracker.terminal_states, and agent.kind.
func makeDispatchWorkflow(t *testing.T, dir string, dispatchYAML string) string {
	t.Helper()
	content := []byte("---\n" +
		"polling:\n  interval_ms: 30000\n" +
		"tracker:\n  kind: file\n  active_states: [\"To Do\"]\n  terminal_states: [\"Done\"]\n" +
		"agent:\n  kind: mock\n" +
		"file:\n  path: issues.json\n" +
		dispatchYAML +
		"---\nDo {{ .issue.title }}.\n")
	return writeCustomWorkflowFile(t, dir, content)
}

func TestValidateDispatch_MalformedGlob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: bad-glob
      match:
        labels: ["[unclosed"]
      agent: mock
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
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
		if strings.Contains(d.Check, "dispatch.rules") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic mentioning dispatch.rules", out.Errors)
	}
}

func TestValidateDispatch_UnreachableRule(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Write a prompt file so the template path validation passes.
	if err := os.WriteFile(filepath.Join(dir, "catch.md"), []byte("catch all"), 0o644); err != nil {
		t.Fatalf("write catch.md: %v", err)
	}
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: catch-all
      template: catch.md

    - name: unreachable
      match:
        labels: ["bug"]
      agent: mock
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
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
		if strings.Contains(d.Message, "unreachable_rules") || strings.Contains(d.Check, "dispatch.rules") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic mentioning unreachable_rules", out.Errors)
	}
}

func TestValidateDispatch_AbsoluteTemplatePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: abs-rule
      match:
        labels: ["bug"]
      template: /etc/hosts
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
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
		if strings.Contains(d.Message, "relative") || strings.Contains(d.Check, "dispatch.rules") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic about relative template path", out.Errors)
	}
}

func TestValidateDispatch_TildeTemplatePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: tilde-rule
      match:
        labels: ["bug"]
      template: ~/templates/custom.md
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
}

func TestValidateDispatch_MissingTemplateFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: missing-tmpl
      match:
        labels: ["bug"]
      template: nonexistent/file.md
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
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
		if strings.Contains(d.Message, "cannot read template") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with 'cannot read template'", out.Errors)
	}
}

func TestValidateDispatch_FrontMatterInTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tmplDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "fm.md"), []byte("---\nkey: value\n---\nBody."), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: fm-rule
      match:
        labels: ["bug"]
      template: prompts/fm.md
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
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
		if d.Check == "template_parse" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "template_parse")
	}
}

func TestValidateDispatch_ValidRulesPassThrough(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tmplDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "bug.md"),
		[]byte("You are a bug-fixing agent for {{ .issue.identifier }}."), 0o644); err != nil {
		t.Fatalf("write bug.md: %v", err)
	}
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: bug
      match:
        labels: ["bug"]
      template: prompts/bug.md
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true; errors: %v", out.Errors)
	}
}

// TestValidateWorkspaceRetentionDaysOutOfRange covers R3: an out-of-range
// workspace.retention_days value is reported as an error diagnostic with
// check name config.workspace.retention_days, offline (the file tracker
// makes no network call).
func TestValidateWorkspaceRetentionDaysOutOfRange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := []byte("---\n" +
		"polling:\n  interval_ms: 30000\n" +
		"tracker:\n  kind: file\n  active_states: [\"To Do\"]\n  terminal_states: [\"Done\"]\n" +
		"agent:\n  kind: mock\n" +
		"file:\n  path: issues.json\n" +
		"workspace:\n  retention_days: 7\n" +
		"---\nDo {{ .issue.title }}.\n")
	wfPath := writeCustomWorkflowFile(t, dir, content)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
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
		if d.Check == "config.workspace.retention_days" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "config.workspace.retention_days")
	}
}

// TestValidateWorkspaceRetentionDaysValid covers R1 and R5: an in-range
// workspace.retention_days value in the front matter is recognized by
// the schema and produces no warnings, offline (the file tracker makes
// no network call).
func TestValidateWorkspaceRetentionDaysValid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := []byte("---\n" +
		"polling:\n  interval_ms: 30000\n" +
		"tracker:\n  kind: file\n  active_states: [\"To Do\"]\n  terminal_states: [\"Done\"]\n" +
		"agent:\n  kind: mock\n" +
		"file:\n  path: issues.json\n" +
		"workspace:\n  retention_days: 30\n" +
		"---\nDo {{ .issue.title }}.\n")
	wfPath := writeCustomWorkflowFile(t, dir, content)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true; errors: %v", out.Errors)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("validateOutput.Warnings = %v, want empty", out.Warnings)
	}
}

// TestValidateWorkspaceRetentionDaysFromEnvOverride covers R1, R5, and
// R9: a workspace.retention_days value supplied only through
// SORTIE_WORKSPACE_RETENTION_DAYS, with no retention_days line in the
// front matter, is recognized by the schema and produces no warnings.
//
// t.Setenv panics when called from a parallel test, so this test does
// not call t.Parallel.
func TestValidateWorkspaceRetentionDaysFromEnvOverride(t *testing.T) {
	t.Setenv("SORTIE_WORKSPACE_RETENTION_DAYS", "30")

	dir := t.TempDir()
	content := []byte("---\n" +
		"polling:\n  interval_ms: 30000\n" +
		"tracker:\n  kind: file\n  active_states: [\"To Do\"]\n  terminal_states: [\"Done\"]\n" +
		"agent:\n  kind: mock\n" +
		"file:\n  path: issues.json\n" +
		"---\nDo {{ .issue.title }}.\n")
	wfPath := writeCustomWorkflowFile(t, dir, content)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true; errors: %v", out.Errors)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("validateOutput.Warnings = %v, want empty", out.Warnings)
	}
}

func TestValidateDispatch_ConfigErrorRouting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := makeDispatchWorkflow(t, dir, `dispatch:
  rules:
    - name: Invalid Name!
      agent: mock
`)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}

	// mapManagerError routes *ConfigError to check "config." + Field.
	found := false
	for _, d := range out.Errors {
		if strings.HasPrefix(d.Check, "config.dispatch") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check prefixed 'config.dispatch'", out.Errors)
	}
}

// --- unresolved_extension_var end-to-end validate tests ---

// unresolvedExtVarWorkflow returns a workflow YAML containing an extension block
// whose api_key references varName, which must be unset when the test runs.
func unresolvedExtVarWorkflow(varName string) []byte {
	return []byte("---\n" +
		"tracker:\n  kind: file\n  active_states:\n    - To Do\n  terminal_states:\n    - Done\n" +
		"agent:\n  kind: mock\n" +
		"myext:\n  api_key: \"$" + varName + "\"\n" +
		"---\nDo {{ .issue.title }}.\n")
}

// TestValidateUnresolvedExtensionVar covers the unresolved_extension_var warning end-to-end.
func TestValidateUnresolvedExtensionVar(t *testing.T) {
	// The variable must resolve to empty. Use a name that is unique to this
	// test suite; explicitly clear it so the resolved value is "".
	const missingVar = "SORTIE_TEST_512_E2E_MISSING"

	t.Run("TextMode", func(t *testing.T) {
		// No t.Parallel: uses t.Setenv.
		t.Setenv(missingVar, "")

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, unresolvedExtVarWorkflow(missingVar))

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)

		// Exit code must be 0 (advisory warning).
		if code != 0 {
			t.Fatalf("run(validate) = %d, want 0 (warning is advisory); stderr: %s", code, stderr.String())
		}
		// Text mode: no output on stdout.
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty (text mode)", stdout.String())
		}
		got := stderr.String()
		// Text mode: warning must appear on stderr.
		if !strings.Contains(got, "warning:") {
			t.Errorf("stderr = %q, want to contain %q", got, "warning:")
		}
		if !strings.Contains(got, "unresolved_extension_var") {
			t.Errorf("stderr = %q, want to contain %q", got, "unresolved_extension_var")
		}
		// Field path must appear (prefixed onto the message by emitDiags).
		if !strings.Contains(got, "myext.api_key") {
			t.Errorf("stderr = %q, want to contain field path %q", got, "myext.api_key")
		}
		// Variable name must appear, resolved value (empty) must not add noise.
		if !strings.Contains(got, missingVar) {
			t.Errorf("stderr = %q, want to contain variable name %q", got, missingVar)
		}
	})

	t.Run("JSONMode", func(t *testing.T) {
		// No t.Parallel: uses t.Setenv.
		t.Setenv(missingVar, "")

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, unresolvedExtVarWorkflow(missingVar))

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)

		// Exit code 0.
		if code != 0 {
			t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
		}
		// JSON mode: stderr must be empty.
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty (JSON mode)", stderr.String())
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
		}
		// Valid must be true.
		if !out.Valid {
			t.Errorf("validateOutput.Valid = false, want true")
		}
		// No errors.
		if len(out.Errors) != 0 {
			t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
		}

		// JSON mode: the warnings array must contain the unresolved_extension_var entry.
		var found *validateDiag
		for i := range out.Warnings {
			if out.Warnings[i].Check == "unresolved_extension_var" {
				w := out.Warnings[i]
				found = &w
				break
			}
		}
		if found == nil {
			t.Fatalf("validateOutput.Warnings = %v, want at least one unresolved_extension_var entry", out.Warnings)
		}
		if found.Severity != "warning" {
			t.Errorf("warning.Severity = %q, want %q", found.Severity, "warning")
		}
		// Field path must be prefixed in the message.
		if !strings.Contains(found.Message, "myext.api_key") {
			t.Errorf("warning.Message = %q, want field path %q", found.Message, "myext.api_key")
		}
		// Variable name appears in the message.
		if !strings.Contains(found.Message, missingVar) {
			t.Errorf("warning.Message = %q, want variable name %q", found.Message, missingVar)
		}
		// Resolved value (empty string "") must not appear as a meaningful token.
		// The message must not contain any resolved credential value; since the value is
		// empty here the key assertion is that the variable name (not the value) is shown.
	})

	t.Run("ValidTrueExitZero", func(t *testing.T) {
		// No t.Parallel: uses t.Setenv.
		// Extra confirmation that valid:true and exit code 0 hold even when
		// the only warning present is an unresolved extension variable.
		t.Setenv(missingVar, "")

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, unresolvedExtVarWorkflow(missingVar))

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(validate --format json) = %d, want 0", code)
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if !out.Valid {
			t.Errorf("validateOutput.Valid = false, want true")
		}
	})
}

// --- reactions.label_commands end-to-end validate tests ---

// labelCommandsBothLabelsEmptyWorkflow returns a workflow with an active
// label_commands provider and both command labels explicitly disabled,
// which buildLabelCommandsConfig rejects as a loud misconfiguration.
func labelCommandsBothLabelsEmptyWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
reactions:
  label_commands:
    provider: github
    review_label: ""
    fix_label: ""
---
Do {{ .issue.title }}.
`)
}

// labelCommandsUnregisteredProviderWorkflow returns a workflow whose
// label_commands.provider names an SCM adapter that is not registered.
// activationChecks reports this offline as an scm_adapter error, the
// same registry lookup failure that already blocks construction.
func labelCommandsUnregisteredProviderWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
reactions:
  label_commands:
    provider: not-a-real-adapter
---
Do {{ .issue.title }}.
`)
}

// TestRunValidate_LabelCommandsBothLabelsEmpty covers the offline path of
// the both-labels-empty validation rule: an active provider with both
// command labels explicitly empty is a config-shape error surfaced by
// sortie validate, not a silently inert block.
func TestRunValidate_LabelCommandsBothLabelsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeIssuesFixture(t, dir)
	wfPath := writeCustomWorkflowFile(t, dir, labelCommandsBothLabelsEmptyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Error("validateOutput.Valid = true, want false")
	}

	found := false
	for _, d := range out.Errors {
		if d.Check == "config.reactions.label_commands" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "config.reactions.label_commands")
	}
}

// TestRunValidate_LabelCommandsUnregisteredProviderRejected is the
// companion case: an unregistered provider value named by an active
// label_commands reaction is a validate-time scm_adapter error, the
// same registry lookup failure that already blocks construction.
func TestRunValidate_LabelCommandsUnregisteredProviderRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeIssuesFixture(t, dir)
	wfPath := writeCustomWorkflowFile(t, dir, labelCommandsUnregisteredProviderWorkflow())

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
	if d := diagWithCheck(out.Errors, "scm_adapter"); d == nil {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "scm_adapter")
	}
}

// labelCommandsFixOnlyWorkflow returns a workflow with an active
// label_commands provider, an explicitly empty review_label, and a
// non-empty fix_label: a fix-only configuration that buildLabelCommandsConfig
// accepts because at least one command label is non-empty.
func labelCommandsFixOnlyWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
reactions:
  label_commands:
    provider: github
    review_label: ""
    fix_label: "sortie:fix"
---
Do {{ .issue.title }}.
`)
}

// TestRunValidate_LabelCommandsFixOnlyValid covers A1/A2's fix-only
// activation shape offline: a provider with review_label explicitly
// disabled and fix_label set is a valid block, not the
// both-labels-empty error (companion to
// TestRunValidate_LabelCommandsBothLabelsEmpty).
func TestRunValidate_LabelCommandsFixOnlyValid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeIssuesFixture(t, dir)
	wfPath := writeCustomWorkflowFile(t, dir, labelCommandsFixOnlyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0 (fix-only config is valid); stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true; errors: %v", out.Errors)
	}
}

// --- Gitea forge validate checks ---

// diagWithCheck returns a pointer to the first diag in diags whose Check
// matches want, or nil if none match.
func diagWithCheck(diags []validateDiag, want string) *validateDiag {
	for i := range diags {
		if diags[i].Check == want {
			return &diags[i]
		}
	}
	return nil
}

// forgeCheckKeys lists every check key this feature can emit: the reaction-
// config diagnostics plus the three activation diagnostics.
var forgeCheckKeys = []string{
	"reactions.review_comments",
	"reactions.bot_review",
	"reactions.auto_merge",
	"reactions.merge_conflicts",
	"scm_adapter",
	"ci_provider",
	"reactions.scm_provider_conflict",
}

// giteaForgeWorkflow returns a WORKFLOW.md whose front matter sets a
// fully valid gitea tracker and agent so preflight passes cleanly;
// extraYAML is appended before the closing front-matter delimiter to
// vary the reactions/ci_feedback block under test.
func giteaForgeWorkflow(extraYAML string) []byte {
	return []byte(`---
tracker:
  kind: gitea
  endpoint: "https://gitea.example.com"
  api_key: "gitea-forge-test-token"
  project: "acme/widgets"
  active_states:
    - backlog
  terminal_states:
    - done
agent:
  kind: mock
` + extraYAML + `---
Do {{ .issue.title }}.
`)
}

// forgeFaultWorkflow returns a WORKFLOW.md with a minimal valid tracker
// (file) and agent, so ValidateDispatchConfig passes cleanly, with
// extraYAML appended to introduce exactly one forge-only fault.
func forgeFaultWorkflow(extraYAML string) []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
` + extraYAML + `---
Do {{ .issue.title }}.
`)
}

// TestActivationChecks covers activationChecks directly with hand-built
// config.ServiceConfig literals, bypassing config.NewServiceConfig.
func TestActivationChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cfg        config.ServiceConfig
		wantChecks []string
	}{
		{
			name: "unregistered SCM provider",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea-scm"},
				},
			},
			wantChecks: []string{"scm_adapter"},
		},
		{
			name: "unregistered CI provider",
			cfg: config.ServiceConfig{
				CIFeedback: config.CIFeedbackConfig{Kind: "gitea-ci"},
			},
			wantChecks: []string{"ci_provider"},
		},
		{
			name: "two active reactions naming different providers",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea"},
					"auto_merge": {Provider: "github"},
				},
			},
			wantChecks: []string{"reactions.scm_provider_conflict"},
		},
		{
			name: "all gitea is clean",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea"},
					"auto_merge": {Provider: "gitea"},
				},
				CIFeedback: config.CIFeedbackConfig{Kind: "gitea"},
			},
			wantChecks: nil,
		},
		{
			// A configuration with no merge_completion block must leave the
			// pre-existing diagnostic set unchanged: a single active
			// reaction on one provider produces no diagnostic. This is the
			// regression an implementer who wires merge_completion into
			// activeSCMKinds unconditionally (ignoring its empty provider)
			// would introduce as a spurious provider-conflict diagnostic.
			name: "no merge_completion block leaves a single active reaction clean",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea"},
				},
			},
			wantChecks: nil,
		},
		{
			name: "merge_completion as the sole active SCM reaction constructs an adapter path",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"merge_completion": {Provider: "gitea"},
				},
			},
			wantChecks: nil,
		},
		{
			name: "merge_completion provider disagreement with another active SCM reaction",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"merge_completion": {Provider: "github"},
					"bot_review":       {Provider: "gitea"},
				},
			},
			wantChecks: []string{"reactions.scm_provider_conflict"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := activationChecks(tt.cfg)

			if len(got) != len(tt.wantChecks) {
				t.Fatalf("activationChecks(cfg) = %v, want %d diag(s) with checks %v", got, len(tt.wantChecks), tt.wantChecks)
			}
			for i, wantCheck := range tt.wantChecks {
				if got[i].Check != wantCheck {
					t.Errorf("activationChecks(cfg) diag[%d].Check = %q, want %q", i, got[i].Check, wantCheck)
				}
				if got[i].Severity != "error" {
					t.Errorf("activationChecks(cfg) diag[%d].Severity = %q, want %q", i, got[i].Severity, "error")
				}
			}
		})
	}
}

// mergeCompletionFaultWorkflow returns a WORKFLOW.md with a minimal valid
// tracker (file) and agent plus a handoff_state, so ValidateDispatchConfig
// passes cleanly, with a reactions.merge_completion block whose
// target_state names an active rather than a terminal state.
func mergeCompletionFaultWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
  handoff_state: In Review
agent:
  kind: mock
reactions:
  merge_completion:
    provider: gitea
    target_state: To Do
---
Do {{ .issue.title }}.
`)
}

// TestValidateMergeCompletionNonTerminalTargetState verifies that
// "sortie validate --format json" on a workflow whose merge_completion
// block sets a non-terminal target_state reports valid: false with an
// error diagnostic whose check is reactions.merge_completion, and exits
// 1, with no network access (the tracker is the offline file adapter).
func TestValidateMergeCompletionNonTerminalTargetState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, mergeCompletionFaultWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
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
	if d := diagWithCheck(out.Errors, "reactions.merge_completion"); d == nil {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "reactions.merge_completion")
	} else if d.Severity != "error" {
		t.Errorf("reactions.merge_completion diagnostic severity = %q, want %q", d.Severity, "error")
	}
}

// TestValidateReviewAndMergeConflictReactionConfigs verifies that validate
// runs every kind-specific numeric rule for review_comments and
// merge_conflicts without constructing an adapter or making a network call.
func TestValidateReviewAndMergeConflictReactionConfigs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		extraYAML    string
		wantCheck    string
		wantField    string
		wantExitCode int
	}{
		{
			name: "review_comments poll_interval_ms below minimum",
			extraYAML: `reactions:
  review_comments:
    provider: gitea
    poll_interval_ms: 1000
`,
			wantCheck:    "reactions.review_comments",
			wantField:    "poll_interval_ms",
			wantExitCode: 1,
		},
		{
			name: "review_comments debounce_ms negative",
			extraYAML: `reactions:
  review_comments:
    provider: gitea
    debounce_ms: -1
`,
			wantCheck:    "reactions.review_comments",
			wantField:    "debounce_ms",
			wantExitCode: 1,
		},
		{
			name: "review_comments max_continuation_turns zero",
			extraYAML: `reactions:
  review_comments:
    provider: gitea
    max_continuation_turns: 0
`,
			wantCheck:    "reactions.review_comments",
			wantField:    "max_continuation_turns",
			wantExitCode: 1,
		},
		{
			name: "merge_conflicts poll_interval_ms below minimum",
			extraYAML: `reactions:
  merge_conflicts:
    provider: gitea
    poll_interval_ms: 1000
`,
			wantCheck:    "reactions.merge_conflicts",
			wantField:    "poll_interval_ms",
			wantExitCode: 1,
		},
		{
			name: "valid numeric settings",
			extraYAML: `reactions:
  review_comments:
    provider: gitea
    poll_interval_ms: 30000
    debounce_ms: 0
    max_continuation_turns: 1
  merge_conflicts:
    provider: gitea
    poll_interval_ms: 30000
`,
			wantExitCode: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			wfPath := writeCustomWorkflowFile(t, dir, forgeFaultWorkflow(tt.extraYAML))

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
			if code != tt.wantExitCode {
				t.Fatalf("run(validate --format json) = %d, want %d; stderr: %s", code, tt.wantExitCode, stderr.String())
			}

			var out validateOutput
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
			}

			if tt.wantCheck == "" {
				if !out.Valid {
					t.Errorf("validateOutput.Valid = false, want true; errors: %v", out.Errors)
				}
				for _, check := range []string{"reactions.review_comments", "reactions.merge_conflicts"} {
					if d := diagWithCheck(out.Errors, check); d != nil {
						t.Errorf("validateOutput.Errors contains %q = %v, want none", check, d)
					}
				}
				return
			}

			if out.Valid {
				t.Errorf("validateOutput.Valid = true, want false")
			}
			d := diagWithCheck(out.Errors, tt.wantCheck)
			if d == nil {
				t.Fatalf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, tt.wantCheck)
			}
			if !strings.Contains(d.Message, tt.wantField) {
				t.Errorf("diagnostic message = %q, want field %q", d.Message, tt.wantField)
			}
		})
	}
}

// TestValidateGiteaForge exercises the fold point end-to-end through
// runValidate for a tracker.kind: gitea workflow, varying the reactions
// and ci_feedback blocks to isolate one forge fault per case.
func TestValidateGiteaForge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		extraYAML string
		wantCheck string
		wantCode  int
	}{
		{
			name: "bot_review bare string bot_usernames",
			extraYAML: `reactions:
  bot_review:
    provider: gitea
    bot_usernames: alice
`,
			wantCheck: "reactions.bot_review",
			wantCode:  1,
		},
		{
			name: "auto_merge strategy rebase-merge",
			extraYAML: `reactions:
  auto_merge:
    provider: gitea
    strategy: rebase-merge
`,
			wantCheck: "reactions.auto_merge",
			wantCode:  1,
		},
		{
			name: "unregistered SCM provider",
			extraYAML: `reactions:
  bot_review:
    provider: gitea-scm
`,
			wantCheck: "scm_adapter",
			wantCode:  1,
		},
		{
			name: "unregistered CI provider",
			extraYAML: `ci_feedback:
  kind: gitea-ci
`,
			wantCheck: "ci_provider",
			wantCode:  1,
		},
		{
			name: "active reactions naming different providers",
			extraYAML: `reactions:
  bot_review:
    provider: gitea
  auto_merge:
    provider: github
`,
			wantCheck: "reactions.scm_provider_conflict",
			wantCode:  1,
		},
		{
			name: "fully valid gitea forge configuration",
			extraYAML: `reactions:
  bot_review:
    provider: gitea
    bot_usernames:
      - sortie-bot
  auto_merge:
    provider: gitea
    strategy: squash
ci_feedback:
  kind: gitea
`,
			wantCheck: "",
			wantCode:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			wfPath := writeCustomWorkflowFile(t, dir, giteaForgeWorkflow(tt.extraYAML))

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("run(validate) = %d, want %d; stderr: %s", code, tt.wantCode, stderr.String())
			}

			var out validateOutput
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
			}

			if tt.wantCheck == "" {
				if !out.Valid {
					t.Errorf("validateOutput.Valid = false, want true; errors: %v", out.Errors)
				}
				for _, key := range forgeCheckKeys {
					if d := diagWithCheck(out.Errors, key); d != nil {
						t.Errorf("validateOutput.Errors contains forge diagnostic %q = %v, want none", key, d)
					}
					if d := diagWithCheck(out.Warnings, key); d != nil {
						t.Errorf("validateOutput.Warnings contains forge diagnostic %q = %v, want none", key, d)
					}
				}
				return
			}

			if out.Valid {
				t.Errorf("validateOutput.Valid = true, want false")
			}
			d := diagWithCheck(out.Errors, tt.wantCheck)
			if d == nil {
				t.Fatalf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, tt.wantCheck)
			}
			if d.Severity != "error" {
				t.Errorf("validateOutput.Errors[%q].Severity = %q, want %q", tt.wantCheck, d.Severity, "error")
			}
		})
	}

	t.Run("messages never leak the tracker api key", func(t *testing.T) {
		t.Parallel()

		const apiKey = "gitea-forge-test-token"

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, giteaForgeWorkflow(`reactions:
  bot_review:
    provider: gitea
    bot_usernames: not-a-list
  auto_merge:
    provider: gitea
    strategy: rebase-merge
`))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
		}
		for _, d := range out.Errors {
			if strings.Contains(d.Message, apiKey) {
				t.Errorf("validateOutput.Errors contains message %q with the tracker api key", d.Message)
			}
		}
	})
}

// TestValidateForgeFoldPointJSON guards the fold point: a forge-only
// error with no preflight error must not be masked by an otherwise
// passing preflight, in both JSON and text output modes.
func TestValidateForgeFoldPointJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		extraYAML string
		wantCheck string
	}{
		{
			name: "auto_merge invalid strategy, no preflight error",
			extraYAML: `reactions:
  auto_merge:
    provider: gitea
    strategy: rebase-merge
`,
			wantCheck: "reactions.auto_merge",
		},
		{
			name: "unregistered SCM provider, no preflight error",
			extraYAML: `reactions:
  bot_review:
    provider: gitea-scm
`,
			wantCheck: "scm_adapter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			wfPath := writeCustomWorkflowFile(t, dir, forgeFaultWorkflow(tt.extraYAML))

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("run(validate --format json) = %d, want 1; stderr: %s", code, stderr.String())
			}

			var out validateOutput
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
			}
			if out.Valid {
				t.Errorf("validateOutput.Valid = true, want false (forge-only error must not be masked by a passing preflight)")
			}
			if d := diagWithCheck(out.Errors, tt.wantCheck); d == nil {
				t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, tt.wantCheck)
			}
		})
	}

	t.Run("text format also exits 1 with no preflight error", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, forgeFaultWorkflow(`reactions:
  auto_merge:
    provider: gitea
    strategy: rebase-merge
`))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
		}
	})
}

// giteaShadowedEscalationWorkflow returns a WORKFLOW.md whose bot_review
// reaction sets an invalid escalation value. buildReactionsConfig rejects
// this at the config layer before orchestrator.ValidateReactionConfigs
// ever runs.
func giteaShadowedEscalationWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
reactions:
  bot_review:
    provider: gitea
    escalation: bogus
---
Do {{ .issue.title }}.
`)
}

// TestValidateShadowedEscalation proves that a malformed reaction
// escalation is reported and exits before ValidateReactionConfigs runs,
// so no reactions.bot_review or reactions.auto_merge diagnostic ever
// appears end-to-end for this fault.
func TestValidateShadowedEscalation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, giteaShadowedEscalationWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
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

	const wantCheck = "config.reactions.bot_review.escalation"
	if d := diagWithCheck(out.Errors, wantCheck); d == nil {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, wantCheck)
	}

	for _, shadowed := range []string{"reactions.bot_review", "reactions.auto_merge"} {
		if d := diagWithCheck(out.Errors, shadowed); d != nil {
			t.Errorf("validateOutput.Errors contains shadowed check %q = %v, want absent (config layer must exit first)", shadowed, d)
		}
	}
}

// handoffStateWorkflow renders a minimal workflow for the given tracker
// kind. Credentials and project are omitted deliberately: the state
// collision is rejected by the config loader, which runs before any
// adapter or preflight check, so validate never reaches those fields.
func handoffStateWorkflow(kind, handoff string) []byte {
	return fmt.Appendf(nil, `---
polling:
  interval_ms: 30000
tracker:
  kind: %s
  active_states:
    - "In Review"
  terminal_states:
    - "Done"
  handoff_state: %q
agent:
  kind: mock
---
Do {{ .issue.title }}.
`, kind, handoff)
}

// TestValidateHandoffStateCollisionEveryTrackerKind verifies that a
// handoff_state colliding with active_states or terminal_states is a hard
// error for every registered tracker kind. The rule is enforced by the
// config loader rather than by adapter validation hooks, so no adapter can
// opt out of it or downgrade it to a warning. The kind list comes from the
// registry so a newly registered adapter is covered without editing this
// test.
func TestValidateHandoffStateCollisionEveryTrackerKind(t *testing.T) {
	t.Parallel()

	kinds := registry.Trackers.Kinds()
	if len(kinds) == 0 {
		t.Fatal("registry.Trackers.Kinds() is empty; the subtests below would assert nothing")
	}

	collisions := []struct {
		name    string
		handoff string
		wantMsg string
	}{
		{
			name:    "active state",
			handoff: "In Review",
			wantMsg: `config.tracker.handoff_state: "In Review" collides with active state "In Review"`,
		},
		{
			name:    "terminal state",
			handoff: "Done",
			wantMsg: `config.tracker.handoff_state: "Done" collides with terminal state "Done"`,
		},
		{
			name:    "terminal state, different casing",
			handoff: "done",
			wantMsg: `config.tracker.handoff_state: "done" collides with terminal state "Done"`,
		},
	}

	for _, kind := range kinds {
		for _, tc := range collisions {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				wfPath := writeCustomWorkflowFile(t, t.TempDir(), handoffStateWorkflow(kind, tc.handoff))

				var stdout, stderr bytes.Buffer
				code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
				if code != 1 {
					t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
				}
				if !strings.Contains(stderr.String(), tc.wantMsg) {
					t.Errorf("stderr = %q, want to contain %q", stderr.String(), tc.wantMsg)
				}
			})
		}

		t.Run(kind+"/no collision", func(t *testing.T) {
			t.Parallel()

			wfPath := writeCustomWorkflowFile(t, t.TempDir(), handoffStateWorkflow(kind, "Awaiting Human"))

			var stdout, stderr bytes.Buffer
			run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)

			// The exit code is unconstrained: kinds that require an api_key or
			// project still fail preflight on this minimal workflow. What must
			// not appear is the collision diagnostic.
			if strings.Contains(stderr.String(), "config.tracker.handoff_state") {
				t.Errorf("stderr = %q, want no handoff_state diagnostic for a non-colliding state", stderr.String())
			}
		})
	}
}

// sentinelState is written into the one state list a defaulted-state
// fixture keeps, so that list carries no element of the tracker kind's own
// fallback lists and every collision the fixture asserts comes from the
// omitted list.
const sentinelState = "Sentinel Written State"

// defaultedStateWorkflow renders a minimal workflow for the given tracker
// kind. An empty active or terminal slice omits that front-matter key,
// which is what leaves the tracker adapter's own fallback list as the
// effective one; an empty handoff or inProgress omits that field.
// Credentials and project are omitted deliberately: the dispatch preflight
// collects every diagnostic rather than short-circuiting, so a kind that
// also reports tracker.api_key still reports the state collision.
func defaultedStateWorkflow(kind string, active, terminal []string, handoff, inProgress string) []byte {
	wf := fmt.Appendf(nil, `---
polling:
  interval_ms: 30000
tracker:
  kind: %s
`, kind)
	wf = appendStateList(wf, "active_states", active)
	wf = appendStateList(wf, "terminal_states", terminal)
	if handoff != "" {
		wf = fmt.Appendf(wf, "  handoff_state: %q\n", handoff)
	}
	if inProgress != "" {
		wf = fmt.Appendf(wf, "  in_progress_state: %q\n", inProgress)
	}
	return append(wf, `agent:
  kind: mock
---
Do {{ .issue.title }}.
`...)
}

// appendStateList appends a YAML sequence for key, or nothing when states
// is empty so the key is absent from the rendered front matter.
func appendStateList(wf []byte, key string, states []string) []byte {
	if len(states) == 0 {
		return wf
	}
	wf = fmt.Appendf(wf, "  %s:\n", key)
	for _, s := range states {
		wf = fmt.Appendf(wf, "    - %q\n", s)
	}
	return wf
}

// runValidateWorkflow writes content to a fresh temp directory and runs
// the validate command against it, returning the exit code and stderr.
func runValidateWorkflow(t *testing.T, content []byte) (int, string) {
	t.Helper()
	wfPath := writeCustomWorkflowFile(t, t.TempDir(), content)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	return code, stderr.String()
}

// requireStateAbsent fails the test when state appears in either of the
// kind's declared fallback lists. A fixture whose written list or handoff
// value overlaps a fallback list asserts a collision it did not construct,
// so this guard fails loudly instead of passing vacuously.
func requireStateAbsent(t *testing.T, kind string, meta registry.TrackerMeta, state string) {
	t.Helper()
	for _, list := range [][]string{meta.DefaultActiveStates, meta.DefaultTerminalStates} {
		for _, declared := range list {
			if strings.EqualFold(declared, state) {
				t.Fatalf("state %q is declared by tracker kind %q (active %v, terminal %v); pick a value outside both fallback lists",
					state, kind, meta.DefaultActiveStates, meta.DefaultTerminalStates)
			}
		}
	}
}

// TestValidateDefaultedTrackerStatesEveryTrackerKind verifies end to end
// that a handoff_state or in_progress_state colliding with a tracker
// adapter's own fallback state list is a hard error whenever the matching
// workflow list is empty. Both the kind list and every state value come
// from the registry, so a newly registered adapter that declares fallback
// lists is covered without editing this test.
func TestValidateDefaultedTrackerStatesEveryTrackerKind(t *testing.T) {
	t.Parallel()

	kinds := registry.Trackers.Kinds()
	if len(kinds) == 0 {
		t.Fatal("registry.Trackers.Kinds() is empty; the subtests below would assert nothing")
	}

	for _, kind := range kinds {
		meta, ok := registry.Trackers.Meta(kind)
		if !ok {
			t.Fatalf("Trackers.Meta(%q) reported not registered", kind)
		}

		if len(meta.DefaultActiveStates) == 0 && len(meta.DefaultTerminalStates) == 0 {
			t.Run(kind+"/no declared fallback lists", func(t *testing.T) {
				t.Parallel()

				_, stderr := runValidateWorkflow(t, defaultedStateWorkflow(kind, nil, []string{sentinelState}, "Awaiting Human", ""))

				// The exit code is unconstrained: a kind that requires an
				// api_key or project still fails preflight on this minimal
				// workflow. What must not appear is a state collision.
				for _, unwanted := range []string{"error: tracker.handoff_state: ", "error: tracker.in_progress_state: "} {
					if strings.Contains(stderr, unwanted) {
						t.Errorf("stderr = %q, want no %q diagnostic for a kind that declares no fallback state lists", stderr, unwanted)
					}
				}
			})
			continue
		}

		if len(meta.DefaultActiveStates) > 0 {
			t.Run(kind+"/handoff state inside the defaulted active list", func(t *testing.T) {
				t.Parallel()

				requireStateAbsent(t, kind, meta, sentinelState)
				handoff := meta.DefaultActiveStates[0]

				code, stderr := runValidateWorkflow(t, defaultedStateWorkflow(kind, nil, []string{sentinelState}, handoff, ""))

				want := fmt.Sprintf(`error: tracker.handoff_state: %q collides with active state %q; tracker.active_states is empty, so the %q adapter falls back to its own active states`,
					handoff, handoff, kind)
				if code != 1 {
					t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr)
				}
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want to contain %q", stderr, want)
				}
			})
		}

		if len(meta.DefaultTerminalStates) == 0 {
			continue
		}

		t.Run(kind+"/handoff state inside the defaulted terminal list", func(t *testing.T) {
			t.Parallel()

			requireStateAbsent(t, kind, meta, sentinelState)
			handoff := meta.DefaultTerminalStates[0]

			code, stderr := runValidateWorkflow(t, defaultedStateWorkflow(kind, []string{sentinelState}, nil, handoff, ""))

			want := fmt.Sprintf(`error: tracker.handoff_state: %q collides with terminal state %q; tracker.terminal_states is empty, so the %q adapter falls back to its own terminal states`,
				handoff, handoff, kind)
			if code != 1 {
				t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want to contain %q", stderr, want)
			}
		})

		// handoff_state stays absent here: a handoff inside the written
		// active_states is rejected by the config loader before the
		// preflight runs, so the in_progress diagnostic would never appear.
		t.Run(kind+"/in progress state inside the defaulted terminal list", func(t *testing.T) {
			t.Parallel()

			inProgress := meta.DefaultTerminalStates[0]

			code, stderr := runValidateWorkflow(t, defaultedStateWorkflow(kind, []string{inProgress}, nil, "", inProgress))

			want := fmt.Sprintf(`error: tracker.in_progress_state: %q collides with terminal state %q; tracker.terminal_states is empty, so the %q adapter falls back to its own terminal states`,
				inProgress, inProgress, kind)
			if code != 1 {
				t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want to contain %q", stderr, want)
			}
		})
	}
}

// TestValidateHandoffStateDefaultParityEveryTrackerKind proves that
// omitting active_states no longer changes whether the collision rule
// fires. The written form is rejected by the config loader under
// config.tracker.handoff_state and the omitted form by the dispatch
// preflight under tracker.handoff_state; the negative assertion is what
// makes the two paths distinguishable, because the loader's check key
// contains the preflight's as a substring.
func TestValidateHandoffStateDefaultParityEveryTrackerKind(t *testing.T) {
	t.Parallel()

	kinds := registry.Trackers.Kinds()
	if len(kinds) == 0 {
		t.Fatal("registry.Trackers.Kinds() is empty; the subtests below would assert nothing")
	}

	for _, kind := range kinds {
		meta, ok := registry.Trackers.Meta(kind)
		if !ok {
			t.Fatalf("Trackers.Meta(%q) reported not registered", kind)
		}
		if len(meta.DefaultActiveStates) == 0 {
			continue
		}

		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			requireStateAbsent(t, kind, meta, sentinelState)
			handoff := meta.DefaultActiveStates[0]

			// The written terminal list must not carry the handoff state:
			// otherwise the config loader rejects the omitted form too and the
			// negative assertion below would fail for the wrong reason.
			terminal := meta.DefaultTerminalStates
			if len(terminal) == 0 {
				terminal = []string{sentinelState}
			}
			for _, s := range terminal {
				if strings.EqualFold(s, handoff) {
					t.Fatalf("tracker kind %q declares %q in both fallback lists (active %v, terminal %v); the written and omitted forms are not comparable",
						kind, handoff, meta.DefaultActiveStates, meta.DefaultTerminalStates)
				}
			}

			collision := fmt.Sprintf("%q collides with active state %q", handoff, handoff)

			writtenCode, writtenStderr := runValidateWorkflow(t,
				defaultedStateWorkflow(kind, meta.DefaultActiveStates, terminal, handoff, ""))
			if writtenCode != 1 {
				t.Fatalf("run(validate) with active_states written = %d, want 1; stderr: %s", writtenCode, writtenStderr)
			}
			if want := "error: config.tracker.handoff_state: " + collision; !strings.Contains(writtenStderr, want) {
				t.Errorf("stderr with active_states written = %q, want to contain %q", writtenStderr, want)
			}

			omittedCode, omittedStderr := runValidateWorkflow(t,
				defaultedStateWorkflow(kind, nil, terminal, handoff, ""))
			if omittedCode != 1 {
				t.Fatalf("run(validate) with active_states omitted = %d, want 1; stderr: %s", omittedCode, omittedStderr)
			}
			want := fmt.Sprintf("error: tracker.handoff_state: %s; tracker.active_states is empty, so the %q adapter falls back to its own active states",
				collision, kind)
			if !strings.Contains(omittedStderr, want) {
				t.Errorf("stderr with active_states omitted = %q, want to contain %q", omittedStderr, want)
			}
			if strings.Contains(omittedStderr, "config.tracker.handoff_state") {
				t.Errorf("stderr with active_states omitted = %q, want no config.tracker.handoff_state diagnostic: the config loader cannot see the fallback list", omittedStderr)
			}
		})
	}
}
