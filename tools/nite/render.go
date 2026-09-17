package main

import (
	"fmt"
	"strings"
)

// renderContext carries every value a render function needs: the raw
// monitor input, the current sample's classification, the derived
// streaks, and the thresholds they are measured against.
type renderContext struct {
	input            monitorInput
	classification   sampleClassification
	streaks          streakResult
	failureThreshold int
	passThreshold    int
}

// renderBody produces the issue or comment body for action, given the
// current sample's verdict. It returns an empty string for action
// "none", the same failing-sample content for "open", "reopen", and a
// failing "comment", and the close-specific and passing-comment bodies
// for their own actions.
func renderBody(action string, verdict sampleVerdict, ctx renderContext) string {
	switch action {
	case "open", "reopen":
		return renderFailingBody(ctx)
	case "comment":
		if verdict == verdictFailing {
			return renderFailingBody(ctx)
		}
		return renderPassingCommentBody(ctx)
	case "close":
		return renderCloseBody(ctx)
	default:
		return ""
	}
}

// renderFailingBody renders the body shared by "open", "reopen", and a
// "comment" posted on a failing sample: the metadata table (including
// the current sample's Classification and its supporting Evidence),
// the failing-test list, and the log excerpt.
func renderFailingBody(ctx renderContext) string {
	in := ctx.input
	var b strings.Builder

	fmt.Fprintf(&b, "Nightly integration tests failed for `%s`.\n\n", in.Adapter)
	b.WriteString("| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Adapter | `%s` (%s) |\n", in.Adapter, in.Kind)
	fmt.Fprintf(&b, "| Tested version | %s |\n", in.Version)
	fmt.Fprintf(&b, "| Install source | %s |\n", in.Source)
	fmt.Fprintf(&b, "| Run | %s |\n", in.RunURL)
	fmt.Fprintf(&b, "| Commit | `%s` |\n", in.Commit)
	fmt.Fprintf(&b, "| Date | %s |\n", in.Now)
	fmt.Fprintf(&b, "| Classification | %s |\n", ctx.classification.Classification)
	fmt.Fprintf(&b, "| Evidence | %s |\n\n", renderEvidence("failing", ctx.streaks.failStreak, ctx.failureThreshold, in.RunURL, ctx.streaks.counted))

	b.WriteString("### Failing tests\n\n")
	if len(ctx.classification.failedTests) > 0 {
		for _, name := range ctx.classification.failedTests {
			fmt.Fprintf(&b, "- `%s`\n", name)
		}
	} else {
		b.WriteString("_No test-level failures parsed (build error, panic, timeout, or install failure). See the log excerpt._\n")
	}

	b.WriteString("\n### Log excerpt\n\n```\n")
	b.WriteString(ctx.classification.excerpt)
	b.WriteString("\n```\n\n")
	b.WriteString("_Filed by NITE. It comments on recurrence and closes automatically after the required consecutive passing samples._\n")
	return b.String()
}

// renderPassingCommentBody renders the body for a "comment" posted on a
// passing sample whose passing streak has not yet reached the
// threshold to close the incident. It never claims a recovery or a
// closure, because none has happened yet.
func renderPassingCommentBody(ctx renderContext) string {
	in := ctx.input
	return fmt.Sprintf(
		"Nightly integration tests passed for `%s`.\n\n%d consecutive passing %s of %d required to close this incident.\n\nRun: %s\n",
		in.Adapter, ctx.streaks.passStreak, sampleWord(ctx.streaks.passStreak), ctx.passThreshold, in.RunURL,
	)
}

// renderCloseBody renders the body for the "close" action: the
// recovery, the passing streak and its threshold, and the run URL.
func renderCloseBody(ctx renderContext) string {
	in := ctx.input
	return fmt.Sprintf(
		"Recovered: `%s` integration tests passed for %d consecutive %s of %d required. Closing.\n\nRun: %s\nDate: %s\n",
		in.Adapter, ctx.streaks.passStreak, sampleWord(ctx.streaks.passStreak), ctx.passThreshold, in.RunURL, in.Now,
	)
}

// renderEvidence names the streak and the threshold it met, lists the
// run URL of every counted sample (the current run plus every prior
// sample the streak counted), and states that a prior sample's verdict
// comes from its job conclusion alone, which carries no classification,
// so the counted samples are not asserted to share a cause.
func renderEvidence(kind string, count, threshold int, currentRunURL string, counted []historySample) string {
	urls := make([]string, 0, count)
	urls = append(urls, currentRunURL)
	for _, sample := range counted {
		urls = append(urls, sample.RunURL)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d consecutive %s %s of %d required.", count, kind, sampleWord(count), threshold)
	b.WriteString(" Counted runs: ")
	b.WriteString(strings.Join(urls, ", "))
	b.WriteString(". A prior sample's verdict comes from its job conclusion alone, which carries no classification, so the counted samples are not asserted to share a cause.")
	return b.String()
}

// renderSummary produces the Markdown appended to the job summary on
// every run, naming the classification, the action, both streaks with
// their thresholds, and the incident number or "none".
func renderSummary(ctx renderContext, action string, incidentNumber int) string {
	incident := "none"
	if incidentNumber != 0 {
		incident = fmt.Sprintf("#%d", incidentNumber)
	}
	return fmt.Sprintf(
		"### NITE decision for `%s`\n\n"+
			"| Field | Value |\n|---|---|\n"+
			"| Classification | %s |\n"+
			"| Action | %s |\n"+
			"| Failing streak | %d of %d required |\n"+
			"| Passing streak | %d of %d required |\n"+
			"| Incident | %s |\n",
		ctx.input.AdapterName, ctx.classification.Classification, action,
		ctx.streaks.failStreak, ctx.failureThreshold,
		ctx.streaks.passStreak, ctx.passThreshold,
		incident,
	)
}
