// Command nite applies a consecutive-sample incident policy to one nightly
// integration shard. It is a CI tool and never enters the shipped binary.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// monitorInput is the whole input the monitor reads from standard input.
type monitorInput struct {
	Outcome        string          `json:"outcome"`  // "success" or "failure"
	JobName        string          `json:"job_name"` // rendered job name, e.g. "Integration: Gemini - ACP"
	Adapter        string          `json:"adapter"`
	AdapterName    string          `json:"adapter_name"`
	Kind           string          `json:"kind"`
	Source         string          `json:"source"`
	Version        string          `json:"version"`
	RunURL         string          `json:"run_url"`
	Commit         string          `json:"commit"`
	Now            string          `json:"now"`              // "2006-01-02 15:04 UTC"
	TestReportPath string          `json:"test_report_path"` // path to the go test -json stream; "" when absent
	IncidentState  string          `json:"incident_state"`   // "absent", "open", or "closed"
	IncidentNumber int             `json:"incident_number"`  // 0 when IncidentState is "absent"
	IncidentRead   bool            `json:"incident_read"`    // false when the incident listing errored or ended early
	HistoryRead    bool            `json:"history_read"`     // false when the prior-sample fetch failed or matched no job
	History        []historySample `json:"history"`          // prior samples for JobName, newest first
}

// historySample is one prior nightly run's conclusion for this shard's job.
type historySample struct {
	RunID      int64  `json:"run_id"`
	RunURL     string `json:"run_url"`
	Conclusion string `json:"conclusion"` // the GitHub job conclusion, verbatim
}

// monitorDecision is the whole output the monitor writes to standard output.
type monitorDecision struct {
	Action         string `json:"action"`         // "none", "open", "reopen", "comment", or "close"
	Classification string `json:"classification"` // "pass", "contract", "environment", or "not_a_sample"
	Title          string `json:"title"`
	Body           string `json:"body"`       // the issue body for "open", the comment body otherwise; "" for "none"
	Summary        string `json:"summary"`    // Markdown appended to the job summary on every run
	Reason         string `json:"reason"`     // one sentence naming why Action was chosen
	Annotation     string `json:"annotation"` // a GitHub workflow command the shell echoes verbatim; "" when none
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]))
}

func run(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	flags := flag.NewFlagSet("nite", flag.ContinueOnError)
	flags.SetOutput(stderr)
	failureThreshold := flags.Int("failure-threshold", 2, "consecutive failing samples required to open or reopen the incident")
	passThreshold := flags.Int("pass-threshold", 2, "consecutive passing samples required to close the incident")
	lookback := flags.Int("lookback", 10, "how many prior runs the caller may examine")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if err := validateThresholds(*failureThreshold, *passThreshold, *lookback); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	var input monitorInput
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		_, _ = fmt.Fprintf(stderr, "decode monitor input: %v\n", err)
		return 2
	}
	if err := validateInput(input); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	classification := classifySample(input.Outcome, input.TestReportPath)
	verdict := verdictForClassification(classification.Classification)
	streaks := deriveStreaks(verdict, input.History, input.HistoryRead)
	chosen := decideAction(input.IncidentState, input.IncidentNumber, verdict, streaks, *failureThreshold, *passThreshold, input.HistoryRead, input.IncidentRead)

	ctx := renderContext{
		input:            input,
		classification:   classification,
		streaks:          streaks,
		failureThreshold: *failureThreshold,
		passThreshold:    *passThreshold,
	}

	result := monitorDecision{
		Action:         chosen.Action,
		Classification: classification.Classification,
		Title:          fmt.Sprintf("Nightly integration failure: %s", input.AdapterName),
		Body:           renderBody(chosen.Action, verdict, ctx),
		Summary:        renderSummary(ctx, chosen.Action, chosen.IncidentNumber),
		Reason:         chosen.Reason,
		Annotation:     chosen.Annotation,
	}

	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		_, _ = fmt.Fprintf(stderr, "encode monitor decision: %v\n", err)
		return 1
	}
	return 0
}

// validateThresholds reports an error naming the first flag below its
// required minimum of 1.
func validateThresholds(failureThreshold, passThreshold, lookback int) error {
	if failureThreshold < 1 {
		return fmt.Errorf("-failure-threshold must be at least 1, got %d", failureThreshold)
	}
	if passThreshold < 1 {
		return fmt.Errorf("-pass-threshold must be at least 1, got %d", passThreshold)
	}
	if lookback < 1 {
		return fmt.Errorf("-lookback must be at least 1, got %d", lookback)
	}
	return nil
}

// validateInput reports an error naming the first required field that
// is missing, empty, or out of range in input.
func validateInput(input monitorInput) error {
	switch input.Outcome {
	case "success", "failure":
	case "":
		return fmt.Errorf("missing required field: outcome")
	default:
		return fmt.Errorf("outcome must be success or failure, got %q", input.Outcome)
	}
	if input.JobName == "" {
		return fmt.Errorf("missing required field: job_name")
	}
	if input.AdapterName == "" {
		return fmt.Errorf("missing required field: adapter_name")
	}
	switch input.IncidentState {
	case "absent", "open", "closed":
	case "":
		return fmt.Errorf("missing required field: incident_state")
	default:
		return fmt.Errorf("incident_state must be absent, open, or closed, got %q", input.IncidentState)
	}
	if input.IncidentState != "absent" && input.IncidentNumber == 0 {
		return fmt.Errorf("incident_number must be non-zero when incident_state is %q", input.IncidentState)
	}
	return nil
}
