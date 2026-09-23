// Command protocolpin compares the pinned Agent Client Protocol schema
// against the publisher's GitHub releases and decides a daily report
// issue's next action. It is a CI tool and never enters the shipped
// binary.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

const decideTimeLayout = "2006-01-02 15:04 UTC"

type decideInput struct {
	Releases        []release   `json:"releases"`
	PinnedTagRefs   []gitRef    `json:"pinned_tag_refs"`
	PinnedTagObject *gitTag     `json:"pinned_tag_object"`
	Report          reportIssue `json:"report"`
	RunURL          string      `json:"run_url"`
	Now             string      `json:"now"` // "2006-01-02 15:04 UTC"
}

type release struct {
	TagName     string         `json:"tag_name"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt string         `json:"published_at"` // RFC 3339
	Assets      []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name   string  `json:"name"`
	Digest *string `json:"digest"` // "sha256:<64 hex>"; nil when the publisher reports none
}

type gitRef struct {
	Ref    string    `json:"ref"` // "refs/tags/<tag>"
	Object gitObject `json:"object"`
}

type gitTag struct {
	Object gitObject `json:"object"`
}

type gitObject struct {
	Type string `json:"type"` // "commit" or "tag"
	SHA  string `json:"sha"`
}

type reportIssue struct {
	State  string `json:"state"`  // "absent", "open", or "closed"
	Number int    `json:"number"` // 0 when State is "absent"
	Body   string `json:"body"`
}

type locateOutput struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Title      string `json:"title"`
}

type decision struct {
	Action  string `json:"action"` // "none", "open", "update", "reopen", or "close"
	Number  int    `json:"number"` // Report.Number echoed; 0 for "open"
	Title   string `json:"title"`
	Body    string `json:"body"`    // non-empty for every action but none; marker-free for close
	Comment string `json:"comment"` // non-empty for update, reopen, close
	Notify  string `json:"notify"`  // non-empty for open, update, reopen
	Summary string `json:"summary"` // non-empty on every decision
}

// usageError marks a fault that must exit 2 rather than the ordinary
// fault's exit 1.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// newFault callers should format a publisher-supplied value in format with
// %q, so the result stays one line regardless of what that value contains.
func newFault(format string, args ...any) error {
	return errors.New("protocolpin: " + fmt.Sprintf(format, args...))
}

func validateDecideInput(in decideInput) error {
	switch in.Report.State {
	case "absent", "open", "closed":
	default:
		return newFault("report.state is %q, want \"absent\", \"open\", or \"closed\"", in.Report.State)
	}
	if (in.Report.State == "open" || in.Report.State == "closed") && in.Report.Number == 0 {
		return newFault("report.number is 0, want a nonzero issue number for report.state %q", in.Report.State)
	}
	if in.RunURL == "" {
		return newFault("run_url is empty")
	}
	if _, err := time.Parse(decideTimeLayout, in.Now); err != nil {
		return newFault("now %q does not match the layout %q", in.Now, decideTimeLayout)
	}
	return nil
}

func runLocate(repoRoot string, stdout io.Writer) error {
	pin, err := locatePin(repoRoot)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(locateOutput{
		Repository: pin.Repository,
		Tag:        pin.Tag,
		Title:      renderTitle(),
	})
}

// runDecide returns a *usageError for undecodable or invalid input, and an
// ordinary error for any other fault, so the exit status can tell them
// apart.
func runDecide(repoRoot string, stdin io.Reader, stdout io.Writer) error {
	var in decideInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return &usageError{newFault("undecodable input: %s", err)}
	}
	if err := validateDecideInput(in); err != nil {
		return &usageError{err}
	}

	pin, err := locatePin(repoRoot)
	if err != nil {
		return err
	}

	now, err := time.Parse(decideTimeLayout, in.Now)
	if err != nil {
		return &usageError{newFault("now %q does not match the layout %q", in.Now, decideTimeLayout)}
	}

	d, err := computeDrift(pin, in.Releases, in.PinnedTagRefs, in.PinnedTagObject, now)
	if err != nil {
		return err
	}

	currentFP, err := fingerprint(d)
	if err != nil {
		return err
	}
	bodyFP, hadMarker := extractFingerprint(in.Report.Body)
	bodyMatches := hadMarker && bodyFP == currentFP
	driftPresent := len(d.Newer) > 0 || len(d.Integrity) > 0 || len(d.Unrecognized) > 0
	action := decideAction(driftPresent, in.Report.State, bodyMatches)

	dec := decision{Action: action, Number: in.Report.Number, Title: renderTitle()}
	switch action {
	case "open", "update", "reopen":
		dec.Body = renderBody(action, d, currentFP)
		dec.Comment = renderComment(action, d, hadMarker, in.RunURL, in.Now)
		dec.Notify = renderNotify(action, d)
	case "close":
		dec.Body = renderBody(action, d, "")
		dec.Comment = renderComment(action, d, hadMarker, in.RunURL, in.Now)
	}
	dec.Summary = renderSummary(d, action, in.Report.Number)

	return json.NewEncoder(stdout).Encode(dec)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run returns the process's exit status rather than exiting the process
// itself, keeping that call in exactly one place.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, newFault("missing subcommand, want %q or %q", "locate", "decide"))
		return 2
	}
	subcommand := args[0]
	if subcommand != "locate" && subcommand != "decide" {
		_, _ = fmt.Fprintln(stderr, newFault("unrecognized subcommand %q, want %q or %q", subcommand, "locate", "decide"))
		return 2
	}

	flags := flag.NewFlagSet("protocolpin "+subcommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	repoRoot := flags.String("repo-root", "", "the repository checkout root")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *repoRoot == "" {
		_, _ = fmt.Fprintln(stderr, newFault("-repo-root is required"))
		return 2
	}

	var err error
	switch subcommand {
	case "locate":
		err = runLocate(*repoRoot, stdout)
	case "decide":
		err = runDecide(*repoRoot, stdin, stdout)
	}
	if err == nil {
		return 0
	}

	if _, ok := errors.AsType[*usageError](err); ok {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	_, _ = fmt.Fprintln(stderr, newFault("%s", err))
	return 1
}
