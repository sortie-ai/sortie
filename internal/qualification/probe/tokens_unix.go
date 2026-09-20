//go:build unix

package probe

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// ceilingStopUnverified names the assertion neither surface's inventory
// makes: the per-issue ceiling is applied by the orchestrator against a
// reported figure, and nothing this measurer launches runs that lane,
// so a resolved figure never stands in for an observed stop.
const ceilingStopUnverified = "no max_tokens stop was induced, so ceiling enforcement stays unverified"

// tokenWildcard is the TokenPath.Path segment naming a level's dynamic
// key. A runtime reporting usage per model carries several keys there,
// all real sources.
const tokenWildcard = "*"

// tokenSurfaceReading is what one surface's token induction resolved.
// An unsupported path is an unknown, not a zero, so reporting the
// surface as source-free would turn an unread value into a proven
// absence.
type tokenSurfaceReading struct {
	observations int
	sessionID    string
	sources      []qualification.TokenObservation
	unsupported  []string
}

// gradeTokenReading turns one surface's reading into the inventory
// observation, so native and protocol readings are graded by one
// ladder. inductionFailed is the outcome a failed induction carries.
func gradeTokenReading(reading tokenSurfaceReading, inductionFailed qualification.Outcome) (string, []qualification.TokenObservation, qualification.Observation) {
	unknown := func(detail string) (string, []qualification.TokenObservation, qualification.Observation) {
		return "", nil, qualification.Observation{
			Grade:   qualification.GradeNotObserved,
			Outcome: inductionFailed,
			Detail:  detail,
		}
	}

	switch {
	case reading.observations == 0:
		return unknown("no observation was recognized, so this surface's token sources stay unread")
	case len(reading.unsupported) > 0:
		return unknown(fmt.Sprintf("read %s and reached a shape this inventory cannot consume at %s",
			countedObservations(reading.observations), strings.Join(reading.unsupported, ", ")))
	case len(reading.sources) > 0 && reading.sessionID == "":
		return unknown(fmt.Sprintf("read %s carrying a token-bearing path that no observation attributed to a session",
			countedObservations(reading.observations)))
	case len(reading.sources) == 0:
		return "", nil, qualification.Observation{
			Grade:   qualification.GradeGap,
			Outcome: qualification.OutcomePass,
			Detail:  fmt.Sprintf("read %s carrying no token-bearing path", countedObservations(reading.observations)),
		}
	}

	return reading.sessionID, reading.sources, qualification.Observation{
		Grade:   qualification.GradeGap,
		Outcome: qualification.OutcomePass,
		Detail: fmt.Sprintf("read %s and resolved %d token-bearing path(s); %s",
			countedObservations(reading.observations), len(reading.sources), ceilingStopUnverified),
	}
}

func countedObservations(count int) string {
	return fmt.Sprintf("%d recognized observation(s)", count)
}

// protocolTokenInventory reads the protocol surface's token inventory
// from the raw results this collection's launches wrote on the wire,
// rather than the domain usage event: the adapter emits none for this
// transport, so the event's absence is a fact about our own
// normalization, not about what the runtime sent. Whether the adapter
// consumed the source is reported beside the reading, never in place of
// it.
func protocolTokenInventory(fixture *sharedFixture) (sessionID string, paths []qualification.TokenObservation, inventory qualification.Observation, extension *qualification.ExtensionReading) {
	adapterUsed, _ := fixture.usage.result()
	reading := inventoryRawExtension(readProtocolWire(fixture.wireTraceDir), adapterUsed)
	extension = reading.evidenceMembers()

	switch reading.verdict {
	case extensionSourceNotObserved:
		return "", nil, qualification.Observation{
			Grade:   qualification.GradeNotObserved,
			Outcome: qualification.OutcomeFixtureInductionFailed,
			Detail:  reading.account(),
		}, extension
	case extensionSourcePresent:
		if reading.admitted() && reading.sessionID != "" {
			return reading.sessionID, reading.sources(), qualification.Observation{
				Grade:   qualification.GradeGap,
				Outcome: qualification.OutcomePass,
				Detail:  reading.account() + "; " + ceilingStopUnverified,
			}, extension
		}
	}
	return "", nil, qualification.Observation{
		Grade:   qualification.GradeGap,
		Outcome: qualification.OutcomePass,
		Detail:  reading.account(),
	}, extension
}

// The three verdicts a reading of the raw transport can reach. They
// stay distinct because an output nothing read is not a measured
// absence, and a block reporting only zeros is present, not missing: a
// runtime that handles a request without spending answers with real
// zeros, and folding that into absence would report a measured zero as
// an unread source.
const (
	extensionSourcePresent     = "extension_source_present"
	extensionSourceAbsent      = "extension_source_absent"
	extensionSourceNotObserved = "not_observed"
)

// protocolExtensionMember is the protocol's extension point on a
// result. Whatever a runtime reports beyond the protocol's vocabulary
// arrives under it, so inventorying it needs no vendor key.
const protocolExtensionMember = "_meta"

// budgetCounterKinds names, in report order, the counters a per-issue
// token budget needs before any source can carry accounting rather than
// a lower bound. A source missing one can still be inventoried but not
// admitted.
var budgetCounterKinds = []string{"input", "output", "cache_read", "reasoning", "tool"}

// budgetCounterWords maps each budget counter onto the words runtimes
// name it with. Matching is on words rather than one runtime's schema
// because the block is an extension with no schema, which is why the
// reading reports completeness separately from presence.
var budgetCounterWords = map[string][]string{
	"input":      {"input", "prompt"},
	"output":     {"output", "candidate", "completion"},
	"cache_read": {"cache", "cached"},
	"reasoning":  {"reasoning", "thought"},
	"tool":       {"tool"},
}

type rawPromptResult struct {
	sessionID string
	result    map[string]any
}

// rawExtensionReading is what one collection established about a
// token-bearing extension on its transport: whether a source was read,
// what it carries and omits, whether the rules admit it into a budget,
// and whether the effective adapter consumed it. Collapsing any into
// another is how an absent reading became a proven zero.
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

// admitted reports whether the rules let this source stand as the
// accounting figure a budget is kept in. A source short of a counter is
// a lower bound, so admitting it would let a partial reading be spent
// as a total.
func (r rawExtensionReading) admitted() bool {
	return r.verdict == extensionSourcePresent && len(r.missing) == 0
}

// evidenceMembers states the reading's presence and admission answers
// as the evidence members a validator and a comparison row can read.
func (r rawExtensionReading) evidenceMembers() *qualification.ExtensionReading {
	source := qualification.ExtensionSourceNotObserved
	switch r.verdict {
	case extensionSourcePresent:
		source = qualification.ExtensionSourcePresent
	case extensionSourceAbsent:
		source = qualification.ExtensionSourceAbsent
	}
	return &qualification.ExtensionReading{Source: source, Admitted: r.admitted()}
}

// sources renders the inventoried counters as spend observations. Only
// an admitted reading calls it.
func (r rawExtensionReading) sources() []qualification.TokenObservation {
	observations := make([]qualification.TokenObservation, 0, len(r.counters))
	for _, path := range r.counters {
		observations = append(observations, qualification.TokenObservation{EvidencePath: path, Kind: "spend"})
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

// tokenBearingPaths walks one extension subtree and reports the path of
// every numeric leaf whose key names a token count. A leaf reporting
// zero is included: a handled request that spends nothing reports zeros
// rather than omitting the block.
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

// readProtocolWire reads every per-launch transport capture under dir
// and reports the prompt results they carried, one empty entry per
// capture that carried none so an unread launch is counted rather than
// dropped. A missing directory reports nothing.
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

// nativeTokenInventory reads one native surface's token inventory from
// every terminal its induction recognized. A wholly unrecognized run
// reports the induction failure rather than a source count it never
// established.
func nativeTokenInventory(t *testing.T, coords Coordinates, surface qualification.Surface, recognizedOutputs []string) (sessionID string, paths []qualification.TokenObservation, inventory qualification.Observation) {
	t.Helper()

	recognizer, ok := coords.Profile.Recognizers[surface]
	if !ok {
		return "", nil, qualification.Observation{
			Grade:   qualification.GradeNotObserved,
			Outcome: qualification.OutcomeFixtureInductionFailed,
			Detail:  "the surface states no recognizer, so no terminal could be read",
		}
	}
	return gradeTokenReading(readNativeTokens(recognizer, recognizedOutputs), qualification.OutcomeFixtureInductionFailed)
}

func readNativeTokens(recognizer qualification.Recognizer, recognizedOutputs []string) tokenSurfaceReading {
	reading := tokenSurfaceReading{}
	seen := map[string]bool{}
	for _, output := range recognizedOutputs {
		if _, found := recognizer.Terminal(output); !found {
			continue
		}
		terminal, found := recognizer.RawTerminal(output)
		if !found {
			continue
		}
		reading.observations++
		if reading.sessionID == "" {
			if sid, sidOK := recognizer.SessionID(output); sidOK {
				reading.sessionID = sid
			}
		}
		for _, declared := range recognizer.TokenPaths {
			resolved, unsupported := resolveTokenSources(terminal, declared)
			for _, name := range unsupported {
				if !slices.Contains(reading.unsupported, name) {
					reading.unsupported = append(reading.unsupported, name)
				}
			}
			for _, source := range resolved {
				if seen[source.EvidencePath] {
					continue
				}
				seen[source.EvidencePath] = true
				reading.sources = append(reading.sources, source)
			}
		}
	}
	return reading
}

// resolveTokenSources resolves one declared path against one recognized
// terminal, reporting the inventoried sources and the concrete paths it
// reached a value at but could not consume. A wildcard segment expands
// over every key at its level; a level with no such key is the one case
// where the terminal says the source is absent.
func resolveTokenSources(terminal map[string]any, declared qualification.TokenPath) (sources []qualification.TokenObservation, unsupported []string) {
	type frame struct {
		object   map[string]any
		resolved []string
	}
	frames := []frame{{object: terminal}}

	for depth, segment := range declared.Path {
		last := depth == len(declared.Path)-1
		var next []frame
		for _, current := range frames {
			for _, key := range tokenKeysAt(current.object, segment) {
				// An explicit null is the absent case, not a shape the
				// inventory failed to consume.
				value, present := current.object[key]
				if !present || value == nil {
					continue
				}
				resolved := append(slices.Clone(current.resolved), key)
				if last {
					source, ok := tokenSourceFor(value, resolved, declared.Kind)
					if !ok {
						unsupported = append(unsupported, "/"+strings.Join(resolved, "/"))
						continue
					}
					sources = append(sources, source)
					continue
				}
				object, ok := value.(map[string]any)
				if !ok {
					unsupported = append(unsupported, "/"+strings.Join(resolved, "/"))
					continue
				}
				next = append(next, frame{object: object, resolved: resolved})
			}
		}
		if last {
			break
		}
		frames = next
	}
	return sources, unsupported
}

// tokenKeysAt names the keys one path segment selects: a literal names
// itself, the wildcard names every key present, in a stable order.
func tokenKeysAt(object map[string]any, segment string) []string {
	if segment != tokenWildcard {
		return []string{segment}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// tokenSourceFor admits a resolved value only as the figure its
// declared kind requires. A spend figure is accumulated into a total,
// so a negative or fractional count is rejected rather than letting the
// presence of a number stand for a budget the runtime cannot support.
func tokenSourceFor(value any, resolved []string, kind string) (qualification.TokenObservation, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return qualification.TokenObservation{}, false
	}
	if kind == "spend" && number != math.Trunc(number) {
		return qualification.TokenObservation{}, false
	}
	return qualification.TokenObservation{EvidencePath: "/" + strings.Join(resolved, "/"), Kind: kind}, true
}

// compensatedTokenPath names where a reading Sortie supplied outside
// the protocol arrives: the adapter's turn result, which is not a
// pointer into the transport and so collides with no wire inventory.
const compensatedTokenPath = "sortie/session/turn/usage"

// compensatedTokenDetail accounts for that reading: a figure was
// returned, and it is the reading a budget is kept in rather than the
// budget being kept. The same unverified stop the inventory rows carry
// is stated so a returned figure is not read as an enforced ceiling.
const compensatedTokenDetail = "the effective adapter returned a measured figure for this session from a source outside the protocol; " + ceilingStopUnverified

// tokenCompensation is what this collection observed about spend
// reaching Sortie outside the protocol: the session a figure was
// returned for, or the zero value.
type tokenCompensation struct {
	supplied  bool
	sessionID string
}

// protocolTokenCompensation reports the reading Sortie supplied outside
// the protocol during this collection. It is derived from the adapter's
// measurement verdict, which this transport latches only when a turn's
// out-of-wire drain returns a record; asking whether a reader was merely
// configured would credit the product with a figure no operator
// received.
func protocolTokenCompensation(fixture *sharedFixture) tokenCompensation {
	measured, sessionID := fixture.usage.result()
	if !measured || sessionID == "" {
		return tokenCompensation{}
	}
	return tokenCompensation{supplied: true, sessionID: sessionID}
}
