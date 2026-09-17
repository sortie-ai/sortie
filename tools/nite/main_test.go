package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("write failed")
}

func validMonitorInput() monitorInput {
	return monitorInput{
		Outcome:        "success",
		JobName:        "Integration: Example",
		AdapterName:    "Example",
		TestReportPath: "testdata/clean_pass.json",
		IncidentState:  "absent",
		IncidentRead:   true,
		HistoryRead:    true,
	}
}

func runNITE(t *testing.T, input monitorInput, args ...string) (int, string, string) {
	t.Helper()

	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("Marshal(%v): %v", input, err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run(bytes.NewReader(encoded), &stdout, &stderr, args)
	return code, stdout.String(), stderr.String()
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("valid_input_writes_decision", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := runNITE(t, validMonitorInput())

		if code != 0 {
			t.Fatalf("run(valid input) = %d, want 0; stderr = %q", code, stderr)
		}
		var decision monitorDecision
		if err := json.Unmarshal([]byte(stdout), &decision); err != nil {
			t.Fatalf("Unmarshal(run(valid input) stdout): %v", err)
		}
		if decision.Classification != "pass" {
			t.Errorf("run(valid input).Classification = %q, want %q", decision.Classification, "pass")
		}
	})

	t.Run("invalid_outcome_is_rejected", func(t *testing.T) {
		t.Parallel()

		input := validMonitorInput()
		input.Outcome = "passed"
		code, stdout, stderr := runNITE(t, input)

		if code != 2 {
			t.Errorf("run(outcome=%q) = %d, want 2", input.Outcome, code)
		}
		if stdout != "" {
			t.Errorf("run(outcome=%q) stdout = %q, want empty", input.Outcome, stdout)
		}
		if !strings.Contains(stderr, "outcome must be success or failure") {
			t.Errorf("run(outcome=%q) stderr = %q, want outcome validation error", input.Outcome, stderr)
		}
	})

	t.Run("invalid_threshold_is_rejected", func(t *testing.T) {
		t.Parallel()

		code, _, stderr := runNITE(t, validMonitorInput(), "-failure-threshold", "0")

		if code != 2 {
			t.Errorf("run(-failure-threshold 0) = %d, want 2", code)
		}
		if !strings.Contains(stderr, "-failure-threshold must be at least 1") {
			t.Errorf("run(-failure-threshold 0) stderr = %q, want threshold validation error", stderr)
		}
	})

	t.Run("malformed_json_is_rejected", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := run(strings.NewReader("{"), &stdout, &stderr, nil)

		if code != 2 {
			t.Errorf("run(malformed JSON) = %d, want 2", code)
		}
		if !strings.Contains(stderr.String(), "decode monitor input") {
			t.Errorf("run(malformed JSON) stderr = %q, want decode error", stderr.String())
		}
	})

	t.Run("output_failure_is_reported", func(t *testing.T) {
		t.Parallel()

		encoded, err := json.Marshal(validMonitorInput())
		if err != nil {
			t.Fatalf("Marshal(valid input): %v", err)
		}
		var stderr bytes.Buffer
		code := run(bytes.NewReader(encoded), failingWriter{}, &stderr, nil)

		if code != 1 {
			t.Errorf("run(output failure) = %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "encode monitor decision") {
			t.Errorf("run(output failure) stderr = %q, want encode error", stderr.String())
		}
	})
}
