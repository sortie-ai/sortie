package domain

import (
	"encoding/json"
	"testing"
)

func TestVerificationResult_ZeroValue(t *testing.T) {
	t.Parallel()

	var vr VerificationResult
	if vr.Command != "" {
		t.Errorf("Command = %q, want empty", vr.Command)
	}
	if vr.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", vr.ExitCode)
	}
	if vr.Stdout != "" {
		t.Errorf("Stdout = %q, want empty", vr.Stdout)
	}
	if vr.Stderr != "" {
		t.Errorf("Stderr = %q, want empty", vr.Stderr)
	}
	if vr.DurationMS != 0 {
		t.Errorf("DurationMS = %d, want 0", vr.DurationMS)
	}
	if vr.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	if vr.ExecutionError != "" {
		t.Errorf("ExecutionError = %q, want empty", vr.ExecutionError)
	}
}

func TestReviewMetadata_ZeroValue(t *testing.T) {
	t.Parallel()

	var rm ReviewMetadata
	if rm.Enabled {
		t.Error("Enabled = true, want false")
	}
	if rm.Iterations != nil {
		t.Errorf("Iterations = %v, want nil", rm.Iterations)
	}
	if rm.TotalIterations != 0 {
		t.Errorf("TotalIterations = %d, want 0", rm.TotalIterations)
	}
	if rm.FinalVerdict != "" {
		t.Errorf("FinalVerdict = %q, want empty", rm.FinalVerdict)
	}
	if rm.CapReached {
		t.Error("CapReached = true, want false")
	}
}

func TestReviewVerdict_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verdict ReviewVerdict
	}{
		{
			name: "pass verdict no issues",
			verdict: ReviewVerdict{
				Verdict: "pass",
				Summary: "All checks passed",
			},
		},
		{
			name: "iterate verdict with issues",
			verdict: ReviewVerdict{
				Verdict: "iterate",
				Summary: "Found 2 issues",
				Issues: []ReviewIssue{
					{File: "main.go", Line: 42, Severity: "error", Message: "nil dereference"},
					{File: "util.go", Severity: "warning", Message: "unused variable"},
				},
			},
		},
		{
			name:    "zero value",
			verdict: ReviewVerdict{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.verdict)
			if err != nil {
				t.Fatalf("json.Marshal(ReviewVerdict) error: %v", err)
			}

			var got ReviewVerdict
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(ReviewVerdict) error: %v", err)
			}

			if got.Verdict != tt.verdict.Verdict {
				t.Errorf("Verdict = %q, want %q", got.Verdict, tt.verdict.Verdict)
			}
			if got.Summary != tt.verdict.Summary {
				t.Errorf("Summary = %q, want %q", got.Summary, tt.verdict.Summary)
			}
			if len(got.Issues) != len(tt.verdict.Issues) {
				t.Fatalf("Issues len = %d, want %d", len(got.Issues), len(tt.verdict.Issues))
			}
			for i, wantIssue := range tt.verdict.Issues {
				gotIssue := got.Issues[i]
				if gotIssue.File != wantIssue.File {
					t.Errorf("Issues[%d].File = %q, want %q", i, gotIssue.File, wantIssue.File)
				}
				if gotIssue.Line != wantIssue.Line {
					t.Errorf("Issues[%d].Line = %d, want %d", i, gotIssue.Line, wantIssue.Line)
				}
				if gotIssue.Severity != wantIssue.Severity {
					t.Errorf("Issues[%d].Severity = %q, want %q", i, gotIssue.Severity, wantIssue.Severity)
				}
				if gotIssue.Message != wantIssue.Message {
					t.Errorf("Issues[%d].Message = %q, want %q", i, gotIssue.Message, wantIssue.Message)
				}
			}
		})
	}
}

func TestReviewIssue_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue ReviewIssue
	}{
		{
			name: "full issue with line range",
			issue: ReviewIssue{
				File:     "internal/foo/bar.go",
				Line:     10,
				EndLine:  15,
				Severity: "error",
				Message:  "index out of bounds",
			},
		},
		{
			name: "issue without line numbers",
			issue: ReviewIssue{
				File:     "README.md",
				Severity: "info",
				Message:  "missing example",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.issue)
			if err != nil {
				t.Fatalf("json.Marshal(ReviewIssue) error: %v", err)
			}

			var got ReviewIssue
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(ReviewIssue) error: %v", err)
			}

			if got.File != tt.issue.File {
				t.Errorf("File = %q, want %q", got.File, tt.issue.File)
			}
			if got.Line != tt.issue.Line {
				t.Errorf("Line = %d, want %d", got.Line, tt.issue.Line)
			}
			if got.EndLine != tt.issue.EndLine {
				t.Errorf("EndLine = %d, want %d", got.EndLine, tt.issue.EndLine)
			}
			if got.Severity != tt.issue.Severity {
				t.Errorf("Severity = %q, want %q", got.Severity, tt.issue.Severity)
			}
			if got.Message != tt.issue.Message {
				t.Errorf("Message = %q, want %q", got.Message, tt.issue.Message)
			}
		})
	}
}

func TestMaxVerificationOutputBytes(t *testing.T) {
	t.Parallel()

	if MaxVerificationOutputBytes <= 0 {
		t.Errorf("MaxVerificationOutputBytes = %d, want positive", MaxVerificationOutputBytes)
	}
	// Sanity-check the documented value (64 KB).
	if MaxVerificationOutputBytes != 65536 {
		t.Errorf("MaxVerificationOutputBytes = %d, want 65536", MaxVerificationOutputBytes)
	}
}
