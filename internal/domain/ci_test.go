package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Compile-time interface satisfaction check.
var _ CIStatusProvider = (*mockCIStatusProvider)(nil)

type mockCIStatusProvider struct{}

func (m *mockCIStatusProvider) FetchCIStatus(_ context.Context, _ string) (CIResult, error) {
	return CIResult{}, nil
}

// --- TestCIResult_ToTemplateMap ---

func TestCIResult_ToTemplateMap_FullyPopulated(t *testing.T) {
	t.Parallel()

	r := CIResult{
		Status: CIStatusFailing,
		CheckRuns: []CheckRun{
			{
				Name:       "build",
				Status:     CheckRunStatusCompleted,
				Conclusion: CheckConclusionSuccess,
				DetailsURL: "https://ci.example.com/runs/1",
			},
			{
				Name:       "lint",
				Status:     CheckRunStatusCompleted,
				Conclusion: CheckConclusionFailure,
				DetailsURL: "https://ci.example.com/runs/2",
			},
		},
		LogExcerpt:   "error: undefined: foo",
		FailingCount: 1,
		Ref:          "main",
	}

	m := r.ToTemplateMap()

	if len(m) != 5 {
		t.Errorf("ToTemplateMap() len = %d, want 5", len(m))
	}
	if got := m["status"]; got != "failing" {
		t.Errorf("ToTemplateMap()[status] = %q, want %q", got, "failing")
	}
	if got := m["log_excerpt"]; got != "error: undefined: foo" {
		t.Errorf("ToTemplateMap()[log_excerpt] = %q, want %q", got, "error: undefined: foo")
	}
	if got := m["failing_count"]; got != 1 {
		t.Errorf("ToTemplateMap()[failing_count] = %v, want 1", got)
	}
	if got := m["ref"]; got != "main" {
		t.Errorf("ToTemplateMap()[ref] = %q, want %q", got, "main")
	}

	runs, ok := m["check_runs"].([]map[string]any)
	if !ok {
		t.Fatalf("ToTemplateMap()[check_runs] type = %T, want []map[string]any", m["check_runs"])
	}
	if len(runs) != 2 {
		t.Fatalf("ToTemplateMap()[check_runs] len = %d, want 2", len(runs))
	}

	wantKeys := []string{"name", "status", "conclusion", "details_url"}
	for i, run := range runs {
		for _, key := range wantKeys {
			if _, present := run[key]; !present {
				t.Errorf("check_runs[%d] missing key %q", i, key)
			}
		}
	}

	if got := runs[0]["name"]; got != "build" {
		t.Errorf("check_runs[0][name] = %q, want %q", got, "build")
	}
	if got := runs[1]["conclusion"]; got != "failure" {
		t.Errorf("check_runs[1][conclusion] = %q, want %q", got, "failure")
	}
}

func TestCIResult_ToTemplateMap_PassingNoChecks(t *testing.T) {
	t.Parallel()

	r := CIResult{
		Status:    CIStatusPassing,
		CheckRuns: []CheckRun{},
	}

	m := r.ToTemplateMap()

	if got := m["status"]; got != "passing" {
		t.Errorf("ToTemplateMap()[status] = %q, want %q", got, "passing")
	}
	if got := m["failing_count"]; got != 0 {
		t.Errorf("ToTemplateMap()[failing_count] = %v, want 0", got)
	}
	if got := m["log_excerpt"]; got != "" {
		t.Errorf("ToTemplateMap()[log_excerpt] = %q, want %q", got, "")
	}

	runs, ok := m["check_runs"].([]map[string]any)
	if !ok {
		t.Fatalf("ToTemplateMap()[check_runs] type = %T, want []map[string]any", m["check_runs"])
	}
	if runs == nil {
		t.Error("ToTemplateMap()[check_runs] = nil, want non-nil empty slice")
	}
	if len(runs) != 0 {
		t.Errorf("ToTemplateMap()[check_runs] len = %d, want 0", len(runs))
	}
}

func TestCIResult_ToTemplateMap_NilCheckRuns(t *testing.T) {
	t.Parallel()

	// Nil CheckRuns must produce a non-nil empty slice in the template map.
	r := CIResult{
		Status:    CIStatusPending,
		CheckRuns: nil,
	}

	m := r.ToTemplateMap()

	runs, ok := m["check_runs"].([]map[string]any)
	if !ok {
		t.Fatalf("ToTemplateMap()[check_runs] type = %T, want []map[string]any", m["check_runs"])
	}
	if runs == nil {
		t.Error("ToTemplateMap()[check_runs] = nil, want non-nil empty slice")
	}
}

func TestCIResult_ToTemplateMap_EmptyLogExcerpt(t *testing.T) {
	t.Parallel()

	r := CIResult{
		Status:     CIStatusPassing,
		CheckRuns:  []CheckRun{},
		LogExcerpt: "",
	}

	m := r.ToTemplateMap()

	val, present := m["log_excerpt"]
	if !present {
		t.Fatal("ToTemplateMap()[log_excerpt] key absent, want present")
	}
	if val != "" {
		t.Errorf("ToTemplateMap()[log_excerpt] = %q, want %q", val, "")
	}
}

// --- TestCIError ---

func TestCIError_Error_WithWrapped(t *testing.T) {
	t.Parallel()

	inner := errors.New("connection refused")
	e := &CIError{
		Kind:    ErrCITransport,
		Message: "dial failed",
		Err:     inner,
	}

	got := e.Error()
	if !strings.Contains(got, string(ErrCITransport)) {
		t.Errorf("CIError.Error() = %q, want to contain %q", got, ErrCITransport)
	}
	if !strings.Contains(got, "dial failed") {
		t.Errorf("CIError.Error() = %q, want to contain %q", got, "dial failed")
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("CIError.Error() = %q, want to contain %q", got, "connection refused")
	}
}

func TestCIError_Error_NoWrapped(t *testing.T) {
	t.Parallel()

	e := &CIError{
		Kind:    ErrCIAuth,
		Message: "token expired",
		Err:     nil,
	}

	got := e.Error()
	if strings.Contains(got, "<nil>") {
		t.Errorf("CIError.Error() = %q, must not contain \"<nil>\"", got)
	}
	if !strings.Contains(got, "token expired") {
		t.Errorf("CIError.Error() = %q, want to contain %q", got, "token expired")
	}
}

func TestCIError_Unwrap(t *testing.T) {
	t.Parallel()

	inner := &sentinelErr{msg: "underlying"}

	e := &CIError{
		Kind:    ErrCIAPI,
		Message: "rate limited",
		Err:     inner,
	}

	var target *sentinelErr
	if !errors.As(e, &target) {
		t.Fatalf("errors.As(&CIError{Err: *sentinelErr{}}) = false, want true")
	}
	if target.msg != "underlying" {
		t.Errorf("unwrapped error msg = %q, want %q", target.msg, "underlying")
	}
}

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }
