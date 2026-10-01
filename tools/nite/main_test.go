package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func TestRunReportsLeafResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome string
		report  string
		want    []string
	}{
		{
			name:    "failure includes evidence before later successful output",
			outcome: "failure",
			report: `{"Package":"a","Test":"TestSuite/broken","Action":"output","Output":"model unavailable\n"}
{"Package":"a","Test":"TestSuite/broken","Action":"fail"}
{"Package":"a","Test":"TestSuite/missing","Action":"output","Output":"fixture is not configured\n"}
{"Package":"a","Test":"TestSuite/missing","Action":"skip"}
{"Package":"a","Test":"TestSuite","Action":"fail"}
{"Package":"b","Test":"TestSuite","Action":"pass"}
` + fmt.Sprintf("{\"Package\":\"b\",\"Test\":\"TestSuite\",\"Action\":\"output\",\"Output\":%q}\n", strings.Repeat("later output\n", 1000)),
			want: []string{"| Classification | test_failure |", "1 passed, 1 failed, 1 skipped", "| Coverage | partial |", "a: TestSuite/broken", "model unavailable", "a: TestSuite/missing", "fixture is not configured"},
		},
		{
			name:    "successful parent with only skipped children is not a sample",
			outcome: "success",
			report: `{"Package":"a","Test":"TestSuite/optional","Action":"skip"}
{"Package":"a","Test":"TestSuite","Action":"pass"}
{"Package":"a","Action":"pass"}`,
			want: []string{"| Classification | not_a_sample |", "0 passed, 0 failed, 1 skipped", "| Coverage | none |"},
		},
		{
			name:    "parent assertion failure survives successful children",
			outcome: "failure",
			report: `{"Package":"a","Test":"TestSuite/ok","Action":"pass"}
{"Package":"a","Test":"TestSuite","Action":"output","Output":"cleanup assertion failed\n"}
{"Package":"a","Test":"TestSuite","Action":"fail"}`,
			want: []string{"1 passed, 1 failed, 0 skipped", "cleanup assertion failed"},
		},
		{
			name:    "mixed success shows partial coverage",
			outcome: "success",
			report: `{"Package":"a","Test":"TestLive","Action":"pass"}
{"Package":"a","Test":"TestOptional","Action":"skip"}`,
			want: []string{"| Classification | pass |", "1 passed, 0 failed, 1 skipped", "| Coverage | partial |", "a: TestOptional"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte(tt.report), 0o600); err != nil {
				t.Fatal(err)
			}
			input := validMonitorInput()
			input.Outcome, input.TestReportPath = tt.outcome, path
			code, stdout, stderr := runNITE(t, input)
			if code != 0 {
				t.Fatalf("run() = %d: %s", code, stderr)
			}
			var decision monitorDecision
			if err := json.Unmarshal([]byte(stdout), &decision); err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(decision.Summary, want) {
					t.Errorf("summary missing %q: %s", want, decision.Summary)
				}
			}
		})
	}
}
