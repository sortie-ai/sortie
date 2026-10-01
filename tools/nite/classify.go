package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// excerptByteLimit bounds the log excerpt classifySample reports, so a
// long-running suite's captured output cannot grow a rendered issue
// body without bound.
const excerptByteLimit = 6000

// sampleClassification is what classifySample decided about one nightly
// sample, plus the evidence a renderer needs to describe it.
type sampleClassification struct {
	Classification string
	testReport
}

type goTestEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type testReport struct {
	executed      int
	passed        int
	failedTests   []string
	skippedTests  []goTestEvent
	packageFailed bool
	excerpt       string
}

func classifySample(outcome, testReportPath string) sampleClassification {
	report, usable := readTestReport(testReportPath)
	result := sampleClassification{testReport: report}
	switch {
	case outcome != "failure" && report.executed == 0:
		result.Classification = "not_a_sample"
	case outcome != "failure":
		result.Classification = "pass"
	case !usable:
		result.Classification = "environment"
		result.excerpt = "no test output was captured"
	case report.executed == 0 && !report.packageFailed:
		result.Classification = "environment"
	default:
		result.Classification = "test_failure"
	}
	return result
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
	results := make(map[string]goTestEvent)
	parents := make(map[string]bool)
	failedParents := make(map[string]bool)
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

		if event.Test == "" {
			if event.Action == "fail" {
				report.packageFailed = true
			}
			continue
		}
		key := event.Package + ": " + event.Test
		result := results[key]
		result.Package, result.Test = event.Package, event.Test
		result.Output = excerptTail(result.Output + event.Output)
		if event.Action == "pass" || event.Action == "fail" || event.Action == "skip" {
			result.Action = event.Action
		}
		results[key] = result
		for name := event.Test; strings.Contains(name, "/"); {
			name = name[:strings.LastIndexByte(name, '/')]
			parents[event.Package+": "+name] = true
			if event.Action == "fail" {
				failedParents[event.Package+": "+name] = true
			}
		}
	}
	if scanner.Err() != nil {
		return testReport{}, false
	}

	if decoded == 0 {
		if raw, readErr := os.ReadFile(testReportPath); readErr == nil { //nolint:gosec // G304: testReportPath is the workflow's own test-report location
			report.excerpt = excerptTail(string(raw))
		}
		return report, true
	}

	report.excerpt = excerptTail(output.String())
	var failures strings.Builder
	for _, key := range slices.Sorted(maps.Keys(results)) {
		result := results[key]
		if parents[key] && (result.Action != "fail" || failedParents[key]) {
			continue
		}
		switch result.Action {
		case "pass":
			report.passed++
			report.executed++
		case "fail":
			report.executed++
			report.failedTests = append(report.failedTests, key)
			fmt.Fprintf(&failures, "%s\n%s\n", key, result.Output)
		case "skip":
			report.skippedTests = append(report.skippedTests, result)
		}
	}
	if failures.Len() > 0 {
		report.excerpt = excerptTail(failures.String())
	}
	return report, true
}

func excerptTail(s string) string {
	if len(s) <= excerptByteLimit {
		return s
	}
	return s[len(s)-excerptByteLimit:]
}
