package main

import "fmt"

// sampleVerdict is what one sample counts as for the consecutive-sample
// streak policy: a passing sample, a failing sample, or, when the
// sample executed no test, not a sample at all.
type sampleVerdict int

const (
	verdictNotASample sampleVerdict = iota
	verdictPassing
	verdictFailing
)

// verdictForClassification maps a sampleClassification.Classification
// value to the verdict the streak policy tracks. "not_a_sample" carries
// no verdict, "pass" is passing, and every other classification counts
// as failing, per the classification table's "counts as" column.
func verdictForClassification(classification string) sampleVerdict {
	switch classification {
	case "not_a_sample":
		return verdictNotASample
	case "pass":
		return verdictPassing
	default:
		return verdictFailing
	}
}

// verdictForConclusion maps a prior sample's GitHub job conclusion to a
// verdict: "success" is passing, "failure" and "timed_out" are failing,
// and every other conclusion is not a sample and is skipped without
// breaking the streak.
func verdictForConclusion(conclusion string) sampleVerdict {
	switch conclusion {
	case "success":
		return verdictPassing
	case "failure", "timed_out":
		return verdictFailing
	default:
		return verdictNotASample
	}
}

// streakResult is the consecutive-sample streak the current sample
// reaches, together with the prior samples that streak actually
// counted, newest first, for evidence rendering.
type streakResult struct {
	failStreak int
	passStreak int
	counted    []historySample
}

// deriveStreaks computes the failing and passing streaks the current
// sample reaches, counting forward through history (newest first) for
// as long as each prior sample carries the same verdict as the current
// one. A sample whose verdict is not_a_sample is skipped without
// breaking the streak. History is treated as empty when historyRead is
// false, so both streaks are at most 1.
func deriveStreaks(current sampleVerdict, history []historySample, historyRead bool) streakResult {
	if current == verdictNotASample {
		return streakResult{}
	}
	if !historyRead {
		history = nil
	}

	count := 1
	var counted []historySample
	for _, sample := range history {
		verdict := verdictForConclusion(sample.Conclusion)
		if verdict == verdictNotASample {
			continue
		}
		if verdict != current {
			break
		}
		count++
		counted = append(counted, sample)
	}

	if current == verdictFailing {
		return streakResult{failStreak: count, counted: counted}
	}
	return streakResult{passStreak: count, counted: counted}
}

// decision is what decideAction chose for one sample: the action the
// shell must carry out, the incident number that action applies to
// (echoed unchanged from the input), the reason the action was chosen,
// and any workflow annotation the shell echoes verbatim.
type decision struct {
	Action         string
	IncidentNumber int
	Reason         string
	Annotation     string
}

// decideAction chooses the action for one sample from the incident
// state, sample verdict, and derived streaks, following the
// incident-state/sample/threshold decision table. A HistoryRead of
// false downgrades open and reopen to none and close to comment; an
// IncidentRead of false downgrades open to none. Annotation carries a
// non-empty GitHub workflow command whenever the decision suppresses or
// downgrades an action on a failing sample: a warning when a failing
// streak sits below its threshold, an error when a degraded read forced
// the downgrade.
func decideAction(incidentState string, incidentNumber int, current sampleVerdict, streaks streakResult, failureThreshold, passThreshold int, historyRead, incidentRead bool) decision {
	if current == verdictNotASample {
		return decision{Action: "none", IncidentNumber: incidentNumber, Reason: "the sample executed no test and does not affect the streak"}
	}

	action, reason := tableAction(incidentState, current, streaks, failureThreshold, passThreshold)

	degradedBy := ""
	if !historyRead {
		switch action {
		case "open", "reopen":
			action = "none"
			reason = "the prior-run history could not be read, so a failing streak cannot be confirmed"
			degradedBy = "history"
		case "close":
			action = "comment"
			reason = "the prior-run history could not be read, so the recovery is reported without closing the incident"
		}
	}
	if !incidentRead && action == "open" {
		action = "none"
		reason = "the incident listing could not be read, so an existing incident cannot be ruled out"
		degradedBy = "incident"
	}

	annotation := ""
	if current == verdictFailing && action == "none" {
		if degradedBy != "" {
			annotation = "::error::" + reason
		} else {
			annotation = "::warning::" + reason
		}
	}

	return decision{Action: action, IncidentNumber: incidentNumber, Reason: reason, Annotation: annotation}
}

// tableAction applies the incident-state/sample/threshold decision
// table, before either degraded-read override is considered.
func tableAction(incidentState string, current sampleVerdict, streaks streakResult, failureThreshold, passThreshold int) (action, reason string) {
	switch incidentState {
	case "absent":
		if current == verdictPassing {
			return "none", "no incident is open and the sample passed"
		}
		if streaks.failStreak < failureThreshold {
			return "none", fmt.Sprintf("%d consecutive failing %s of %d required to open an incident", streaks.failStreak, sampleWord(streaks.failStreak), failureThreshold)
		}
		return "open", fmt.Sprintf("%d consecutive failing %s reached the threshold of %d", streaks.failStreak, sampleWord(streaks.failStreak), failureThreshold)
	case "open":
		if current == verdictFailing {
			return "comment", "the incident is open and the sample failed"
		}
		if streaks.passStreak < passThreshold {
			return "comment", fmt.Sprintf("%d consecutive passing %s of %d required to close the incident", streaks.passStreak, sampleWord(streaks.passStreak), passThreshold)
		}
		return "close", fmt.Sprintf("%d consecutive passing %s reached the threshold of %d", streaks.passStreak, sampleWord(streaks.passStreak), passThreshold)
	case "closed":
		if current == verdictPassing {
			return "none", "the incident is closed and the sample passed"
		}
		if streaks.failStreak < failureThreshold {
			return "none", fmt.Sprintf("%d consecutive failing %s of %d required to reopen the incident", streaks.failStreak, sampleWord(streaks.failStreak), failureThreshold)
		}
		return "reopen", fmt.Sprintf("%d consecutive failing %s reached the threshold of %d", streaks.failStreak, sampleWord(streaks.failStreak), failureThreshold)
	default:
		return "none", fmt.Sprintf("unrecognized incident state %q", incidentState)
	}
}

// sampleWord returns the singular or plural noun for n samples.
func sampleWord(n int) string {
	if n == 1 {
		return "sample"
	}
	return "samples"
}
