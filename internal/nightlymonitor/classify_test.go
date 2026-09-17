package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestClassifySample drives classifySample over one input per row of the
// classification table, plus the undecodable-line and excerpt-bound
// properties the table does not name.
func TestClassifySample(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		outcome        string
		testReportPath string
		want           string
	}{
		{
			name:           "row1_success_no_test_executed_is_not_a_sample",
			outcome:        "success",
			testReportPath: "testdata/all_skipped.json",
			want:           "not_a_sample",
		},
		{
			name:           "row2_success_any_is_pass",
			outcome:        "success",
			testReportPath: "testdata/clean_pass.json",
			want:           "pass",
		},
		{
			name:           "row3_failure_report_path_empty_is_environment",
			outcome:        "failure",
			testReportPath: "",
			want:           "environment",
		},
		{
			name:           "row3_failure_report_file_absent_is_environment",
			outcome:        "failure",
			testReportPath: "testdata/does-not-exist.json",
			want:           "environment",
		},
		{
			name:           "row4_failure_no_test_executed_no_package_failure_is_environment",
			outcome:        "failure",
			testReportPath: "testdata/all_skipped.json",
			want:           "environment",
		},
		{
			// install_failure_absent.json holds no valid go test -json
			// line at all (an install/build failure never produced one),
			// reaching row 4 through the decode-nothing fallback path
			// rather than the row above's decode-some-skip-all path.
			name:           "row4_failure_report_undecodable_is_environment",
			outcome:        "failure",
			testReportPath: "testdata/install_failure_absent.json",
			want:           "environment",
		},
		{
			name:           "row5_failure_any_is_contract",
			outcome:        "failure",
			testReportPath: "testdata/contract_failure.json",
			want:           "contract",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifySample(tt.outcome, tt.testReportPath)

			if got.Classification != tt.want {
				t.Errorf("classifySample(%q, %q).Classification = %q, want %q", tt.outcome, tt.testReportPath, got.Classification, tt.want)
			}
		})
	}

	t.Run("undecodable_line_is_skipped_not_aborting", func(t *testing.T) {
		t.Parallel()

		// contract_failure.json interleaves one bare, non-JSON panic
		// line among valid go test -json lines. Decoding must skip it
		// and keep classifying the lines that do decode.
		got := classifySample("failure", "testdata/contract_failure.json")

		wantFailed := []string{"TestRecordedShapeViolations/S-9/tool_result_event_missing"}
		if !slices.Equal(got.failedTests, wantFailed) {
			t.Errorf("classifySample(...).failedTests = %v, want %v", got.failedTests, wantFailed)
		}
		if !got.packageFailed {
			t.Errorf("classifySample(...).packageFailed = %v, want true", got.packageFailed)
		}
		if !strings.Contains(got.excerpt, "unexpected violation") {
			t.Errorf("classifySample(...).excerpt = %q, want it to contain the captured test output", got.excerpt)
		}
	})

	t.Run("excerpt_bound_at_6000_bytes", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "big.json")

		longOutput := strings.Repeat("x", excerptByteLimit+500)
		content := fmt.Sprintf(`{"Action":"run","Test":"TestBig"}`+"\n"+
			`{"Action":"output","Test":"TestBig","Output":%q}`+"\n"+
			`{"Action":"fail","Test":"TestBig"}`+"\n"+
			`{"Action":"fail","Test":""}`+"\n", longOutput)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}

		got := classifySample("failure", path)

		if got.Classification != "contract" {
			t.Fatalf("classifySample(...).Classification = %q, want %q", got.Classification, "contract")
		}
		if len(got.excerpt) != excerptByteLimit {
			t.Errorf("len(classifySample(...).excerpt) = %d, want %d", len(got.excerpt), excerptByteLimit)
		}
		wantExcerpt := longOutput[len(longOutput)-excerptByteLimit:]
		if got.excerpt != wantExcerpt {
			t.Errorf("classifySample(...).excerpt = last %d bytes mismatch, want the tail of the written output", excerptByteLimit)
		}
	})
}
