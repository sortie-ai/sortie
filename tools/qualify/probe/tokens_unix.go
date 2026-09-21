//go:build unix

package probe

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/tools/qualify/eval"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// protocolTokenInventory reads the token inventory from the raw wire,
// not the domain usage event: the adapter emits none for this
// transport, so that absence says nothing about what the runtime sent.
// It grades as "transport" derivation, not eval.RecognizeInventory.
func protocolTokenInventory(fixture *sharedFixture) (sessionID string, paths []evidence.TokenObservation, inventory gradedObservation, extension *evidence.ExtensionReading) {
	adapterUsed, _ := fixture.usage.result()
	reading := inventoryRawExtension(readProtocolWire(fixture.wireTraceDir), adapterUsed)
	extension = reading.evidenceMembers()

	switch reading.verdict {
	case extensionSourceNotObserved:
		return "", nil, transportGraded(evidence.Observation{
			Grade:   evidence.GradeNotObserved,
			Outcome: evidence.OutcomeFixtureInductionFailed,
			Detail:  reading.account(),
		}), extension
	case extensionSourcePresent:
		if reading.admitted() && reading.sessionID != "" {
			return reading.sessionID, reading.sources(), transportGraded(evidence.Observation{
				Grade:   evidence.GradeGap,
				Outcome: evidence.OutcomePass,
				Detail:  reading.account() + "; " + ceilingStopUnverified,
			}), extension
		}
	}
	return "", nil, transportGraded(evidence.Observation{
		Grade:   evidence.GradeGap,
		Outcome: evidence.OutcomePass,
		Detail:  reading.account(),
	}), extension
}

// ceilingStopUnverified names the assertion no inventory makes: the
// per-issue ceiling runs in the orchestrator's own lane, which nothing
// this measurer launches exercises.
const ceilingStopUnverified = "no max_tokens stop was induced, so ceiling enforcement stays unverified"

// protocolExtensionMember is the protocol's extension point on a
// result. Whatever a runtime reports beyond the protocol's vocabulary
// arrives under it, so inventorying it needs no vendor key.
const protocolExtensionMember = "_meta"

// budgetCounterKinds names, in report order, the counters a budget
// needs before a source counts as accounting rather than a lower
// bound; a source missing one is inventoried but not admitted.
var budgetCounterKinds = []string{"input", "output", "cache_read", "reasoning", "tool"}

// budgetCounterWords maps each counter onto the words runtimes name
// it with, since the extension carries no schema to match against
// directly.
var budgetCounterWords = map[string][]string{
	"input":      {"input", "prompt"},
	"output":     {"output", "candidate", "completion"},
	"cache_read": {"cache", "cached"},
	"reasoning":  {"reasoning", "thought"},
	"tool":       {"tool"},
}

// The three verdicts a raw-transport reading can reach, kept distinct
// because unread output is not a measured absence, and real zeros
// count as present, not missing.
const (
	extensionSourcePresent     = "extension_source_present"
	extensionSourceAbsent      = "extension_source_absent"
	extensionSourceNotObserved = "not_observed"
)

type rawPromptResult struct {
	sessionID string
	result    map[string]any
}

// rawExtensionReading keeps its four answers distinct: collapsing any
// two is how an absent reading becomes a proven zero.
type rawExtensionReading struct {
	verdict     string
	results     int
	traces      int
	sessionID   string
	counters    []string
	covered     []string
	missing     []string
	adapterUsed bool
}

// admitted reports whether this source may stand as the accounting
// figure a budget is kept in. Short of a counter, it's a lower bound
// that must not be spent as a total.
func (r rawExtensionReading) admitted() bool {
	return r.verdict == extensionSourcePresent && len(r.missing) == 0
}

// evidenceMembers states the reading's presence and admission answers
// as the evidence members a validator and a comparison row can read.
func (r rawExtensionReading) evidenceMembers() *evidence.ExtensionReading {
	source := evidence.ExtensionSourceNotObserved
	switch r.verdict {
	case extensionSourcePresent:
		source = evidence.ExtensionSourcePresent
	case extensionSourceAbsent:
		source = evidence.ExtensionSourceAbsent
	}
	return &evidence.ExtensionReading{Source: source, Admitted: r.admitted()}
}

// sources renders the inventoried counters as spend observations. Only
// an admitted reading calls it.
func (r rawExtensionReading) sources() []evidence.TokenObservation {
	observations := make([]evidence.TokenObservation, 0, len(r.counters))
	for _, path := range r.counters {
		observations = append(observations, evidence.TokenObservation{EvidencePath: path, Kind: "spend"})
	}
	return observations
}

// account states the reading's four answers in one bounded line, so a
// source that exists but is not admitted reads as its own state.
func (r rawExtensionReading) account() string {
	adapter := "the effective adapter consumed none of it"
	if r.adapterUsed {
		adapter = "the effective adapter consumed it"
	}
	if r.verdict == extensionSourceNotObserved {
		return fmt.Sprintf("source: %s, no raw result was read from %d launch trace(s), so what the transport carried stays unknown; %s",
			extensionSourceNotObserved, r.traces, adapter)
	}
	if r.verdict == extensionSourceAbsent {
		return fmt.Sprintf("source: %s, read %d raw result(s) and none carried a token-bearing extension; %s",
			extensionSourceAbsent, r.results, adapter)
	}
	admission := fmt.Sprintf("not admitted to a budget, it omits %s", strings.Join(r.missing, ", "))
	if r.admitted() {
		admission = "admitted to a budget"
	}
	return fmt.Sprintf("source: %s in %d raw result(s); carries %s; %s; %s",
		extensionSourcePresent, r.results, strings.Join(r.covered, ", "), admission, adapter)
}

// inventoryRawExtension classifies every raw result one collection read
// and reports what their extensions carried. adapterUsed is carried
// beside the reading, never folded into it.
func inventoryRawExtension(traces []rawPromptResult, adapterUsed bool) rawExtensionReading {
	reading := rawExtensionReading{verdict: extensionSourceNotObserved, adapterUsed: adapterUsed}
	seen := map[string]bool{}
	for _, trace := range traces {
		if trace.result == nil {
			reading.traces++
			continue
		}
		reading.results++
		if reading.sessionID == "" {
			reading.sessionID = trace.sessionID
		}
		for _, path := range tokenBearingPaths(trace.result[protocolExtensionMember], []string{protocolExtensionMember}) {
			if seen[path] {
				continue
			}
			seen[path] = true
			reading.counters = append(reading.counters, path)
		}
	}

	switch {
	case reading.results == 0:
		return reading
	case len(reading.counters) == 0:
		reading.verdict = extensionSourceAbsent
		return reading
	}

	slices.Sort(reading.counters)
	reading.verdict = extensionSourcePresent
	for _, kind := range budgetCounterKinds {
		if coversCounter(reading.counters, kind) {
			reading.covered = append(reading.covered, kind)
			continue
		}
		reading.missing = append(reading.missing, kind)
	}
	return reading
}

func coversCounter(paths []string, kind string) bool {
	for _, path := range paths {
		lowered := strings.ToLower(path)
		for _, word := range budgetCounterWords[kind] {
			if strings.Contains(lowered, word) {
				return true
			}
		}
	}
	return false
}

// tokenBearingPaths walks one extension subtree and reports the path
// of every numeric leaf whose key names a token count, including a
// zero leaf: spending nothing still reports zeros, not an omitted block.
func tokenBearingPaths(value any, at []string) []string {
	switch typed := value.(type) {
	case map[string]any:
		var paths []string
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			paths = append(paths, tokenBearingPaths(typed[key], append(slices.Clone(at), key))...)
		}
		return paths
	case []any:
		var paths []string
		for i, element := range typed {
			paths = append(paths, tokenBearingPaths(element, append(slices.Clone(at), strconv.Itoa(i)))...)
		}
		return paths
	case float64:
		if !strings.Contains(strings.ToLower(at[len(at)-1]), "token") {
			return nil
		}
		return []string{"/" + strings.Join(at, "/")}
	}
	return nil
}

// readProtocolWire reads every per-launch transport capture under
// dir, one empty entry per capture with none, so an unread launch is
// counted rather than dropped.
func readProtocolWire(dir string) []rawPromptResult {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var results []rawPromptResult
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), wireTraceFilePrefix) || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // a capture this collection's own launch wrote under its own trace directory
		if readErr != nil {
			results = append(results, rawPromptResult{})
			continue
		}
		captured := promptResultsIn(string(content))
		if len(captured) == 0 {
			results = append(results, rawPromptResult{})
			continue
		}
		results = append(results, captured...)
	}
	return results
}

// promptResultsIn reads one launch's captured transport, attributing
// each prompt result to the session that launch reported.
func promptResultsIn(content string) []rawPromptResult {
	var sessionID string
	var results []rawPromptResult
	for line := range strings.SplitSeq(content, "\n") {
		var message struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &message); err != nil || message.Result == nil {
			continue
		}
		if id, ok := message.Result["sessionId"].(string); ok && sessionID == "" {
			sessionID = id
		}
		if _, isPromptResult := message.Result["stopReason"]; !isPromptResult {
			continue
		}
		results = append(results, rawPromptResult{result: message.Result})
	}
	for i := range results {
		results[i].sessionID = sessionID
	}
	return results
}

// nativeTokenInventory grades via eval.RecognizeInventory, journaled
// as "inventory" derivation with no launch or streams of its own: it
// replays the surface's own "recognizer" journal entries.
func nativeTokenInventory(p profile.RuntimeProfile, surface evidence.Surface, outputs []string) (sessionID string, paths []evidence.TokenObservation, inventory gradedObservation) {
	streams := make([]evidence.StreamCapture, len(outputs))
	for i, output := range outputs {
		streams[i] = boundStreamCapture(output)
	}
	sid, resolvedPaths, obs := eval.RecognizeInventory(p, surface, streams)
	return sid, resolvedPaths, gradedObservation{obs: obs, derivation: evidence.DerivationInventory}
}

// compensatedTokenPath names where a reading Sortie supplied outside
// the protocol arrives: the adapter's turn result, which is not a
// pointer into the transport and so collides with no wire inventory.
const compensatedTokenPath = "sortie/session/turn/usage"

// compensatedTokenDetail accounts for that reading: a returned figure
// is the reading a budget is kept in, not the budget itself, and
// carries the same unverified-stop caveat as the inventory rows.
const compensatedTokenDetail = "the effective adapter returned a measured figure for this session from a source outside the protocol; " + ceilingStopUnverified

// tokenCompensation is what this collection observed about spend
// reaching Sortie outside the protocol: the session a figure was
// returned for, or the zero value.
type tokenCompensation struct {
	supplied  bool
	sessionID string
}

// protocolTokenCompensation latches only when a turn's out-of-wire
// drain returns a record: asking whether a reader was merely configured
// would credit a figure no operator received.
func protocolTokenCompensation(fixture *sharedFixture) tokenCompensation {
	measured, sessionID := fixture.usage.result()
	if !measured || sessionID == "" {
		return tokenCompensation{}
	}
	return tokenCompensation{supplied: true, sessionID: sessionID}
}
