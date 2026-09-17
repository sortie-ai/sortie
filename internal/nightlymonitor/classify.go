package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// excerptByteLimit bounds the log excerpt classifySample reports, so a
// long-running suite's captured output cannot grow a rendered issue
// body without bound.
const excerptByteLimit = 6000

// sampleClassification is what classifySample decided about one nightly
// sample, plus the evidence a renderer needs to describe it.
type sampleClassification struct {
	Classification string // "pass", "contract", "environment", or "not_a_sample"
	failedTests    []string
	packageFailed  bool
	excerpt        string
}

// goTestEvent is one decoded line of a `go test -json` event stream.
type goTestEvent struct {
	Action string
	Test   string
	Output string
}

// testReport is what readTestReport counts out of a decoded `go test
// -json` event stream.
type testReport struct {
	executed      int
	failedTests   []string
	packageFailed bool
	excerpt       string
}

// classifySample decides which of the five classification rows outcome
// and the `go test -json` stream at testReportPath reach. A success
// outcome that executed no test is "not_a_sample"; any other success is
// "pass". A failure outcome whose report is absent, unreadable, or ran
// no test without a package-level failure is "environment"; every
// other failure is "contract".
func classifySample(outcome, testReportPath string) sampleClassification {
	report, reportUsable := readTestReport(testReportPath)

	if outcome != "failure" {
		if report.executed == 0 {
			return sampleClassification{Classification: "not_a_sample"}
		}
		return sampleClassification{Classification: "pass"}
	}

	if !reportUsable {
		return sampleClassification{
			Classification: "environment",
			excerpt:        "no test output was captured",
		}
	}

	classification := "contract"
	if report.executed == 0 && !report.packageFailed {
		classification = "environment"
	}
	return sampleClassification{
		Classification: classification,
		failedTests:    report.failedTests,
		packageFailed:  report.packageFailed,
		excerpt:        report.excerpt,
	}
}

// readTestReport decodes testReportPath as a `go test -json` event
// stream, one JSON object per line, skipping any line that fails to
// decode. reportUsable is false when testReportPath is empty or the
// file cannot be opened; every other case is usable even when no line
// decoded, in which case the excerpt falls back to the raw file
// content.
func readTestReport(testReportPath string) (report testReport, reportUsable bool) {
	if testReportPath == "" {
		return testReport{}, false
	}

	file, err := os.Open(testReportPath) //nolint:gosec // G304: testReportPath is the workflow's own test-report location
	if err != nil {
		return testReport{}, false
	}
	defer func() { _ = file.Close() }()

	var output strings.Builder
	failed := make(map[string]bool)
	var decoded int

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		decoded++
		output.WriteString(event.Output)

		if event.Action != "pass" && event.Action != "fail" {
			continue
		}
		if event.Test == "" {
			if event.Action == "fail" {
				report.packageFailed = true
			}
			continue
		}
		report.executed++
		if event.Action == "fail" {
			failed[event.Test] = true
		}
	}

	if decoded == 0 {
		if raw, readErr := os.ReadFile(testReportPath); readErr == nil { //nolint:gosec // G304: testReportPath is the workflow's own test-report location
			report.excerpt = lastBytes(string(raw), excerptByteLimit)
		}
		return report, true
	}

	report.excerpt = lastBytes(output.String(), excerptByteLimit)
	report.failedTests = sortedKeys(failed)
	return report, true
}

// lastBytes returns the last n bytes of s, or s unchanged when it is no
// longer than n.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// sortedKeys returns the keys of m sorted ascending.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
