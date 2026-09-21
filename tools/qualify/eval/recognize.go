package eval

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// nativeTerminalFrom recognizes one native surface's terminal outcome from
// its output, treating a recognized terminal as authoritative over outcome
// since a non-zero exit is several runtimes' documented error terminal.
func nativeTerminalFrom(p profile.RuntimeProfile, surface evidence.Surface, output string, outcome evidence.LaunchOutcome) (terminal profile.Terminal, transportLoss, found bool) {
	if outcome == evidence.LaunchOutcomeLaunchFailed {
		return profile.Terminal{}, false, false
	}
	recognizer, ok := p.Recognizers[surface]
	if !ok {
		return profile.Terminal{}, false, false
	}
	if terminal, found = recognizer.Terminal(output); found {
		return terminal, false, true
	}
	if outcome == evidence.LaunchOutcomeRunFailed {
		return profile.Terminal{}, true, false
	}
	return profile.Terminal{}, false, false
}

// nativeFailureObservation grades a native launch with no recognized
// terminal, naming transport loss as its own runtime failure.
func nativeFailureObservation(sessionID string, transportLoss bool, fallbackOutcome evidence.Outcome, fallbackDetail string) evidence.Observation {
	if transportLoss {
		return evidence.Observation{
			Grade:     evidence.GradeNotObserved,
			Outcome:   evidence.OutcomeRuntimeFailed,
			Detail:    "the launch ended in a bounded exit or timeout, producing no recognized terminal",
			SessionID: sessionID,
		}
	}
	return evidence.Observation{
		Grade:     evidence.GradeNotObserved,
		Outcome:   fallbackOutcome,
		Detail:    fallbackDetail,
		SessionID: sessionID,
	}
}

// resolvedSessionID resolves surface's session identifier from a native
// launch's output, falling back to the seed only when it appears literally.
func resolvedSessionID(p profile.RuntimeProfile, surface evidence.Surface, l evidence.LaunchRecord, output string) string {
	recognizer, ok := p.Recognizers[surface]
	if ok {
		if sessionID, found := recognizer.SessionID(output); found && sessionID != "" {
			return sessionID
		}
	}
	if l.SeededSessionID != "" && strings.Contains(output, l.SeededSessionID) {
		return l.SeededSessionID
	}
	return ""
}

// caseMatch reports whether terminal, found, and transportLoss satisfy
// caseID's closed native induction rule.
func caseMatch(caseID evidence.Case, terminal profile.Terminal, found, transportLoss bool) bool {
	switch caseID {
	case evidence.CaseSuccess:
		return found && !terminal.Error && terminal.EndTurn
	case evidence.CaseRuntimeFailure:
		return found && terminal.Error
	case evidence.CaseRetryableTransport:
		if transportLoss {
			return true
		}
		return found && !terminal.Error && terminal.Case == caseID
	default:
		return found && !terminal.Error && terminal.Case == caseID
	}
}

// caseMatchDetail and caseMissDetail name the detail a matched or missed
// caseID carries, mirroring the induce* functions' own wording.
func caseMatchDetail(caseID evidence.Case) string {
	switch caseID {
	case evidence.CaseSuccess:
		return "the recognized terminal reported end of turn"
	case evidence.CaseRuntimeFailure:
		return "the recognized terminal carried an error member"
	case evidence.CaseRuntimeRefusal:
		return "the recognized terminal reported a refusal status"
	case evidence.CaseCancellation:
		return "the launch ended in cancellation after one SIGINT to its group"
	case evidence.CaseRetryableTransport:
		return "the launch reported transport loss after one SIGKILL to its group"
	case evidence.CaseHumanInput:
		return "the recognized terminal reported a human-input requirement"
	}
	return "the recognized terminal matched the induced case"
}

func caseMissDetail(caseID evidence.Case) string {
	switch caseID {
	case evidence.CaseSuccess:
		return "no recognized end-of-turn terminal was observed"
	case evidence.CaseRuntimeFailure:
		return "no recognized error terminal was observed"
	case evidence.CaseRuntimeRefusal:
		return "no recognized refusal terminal was observed"
	case evidence.CaseCancellation:
		return "the launch did not end in cancellation after one SIGINT to its group"
	case evidence.CaseRetryableTransport:
		return "the launch did not report transport loss after one SIGKILL to its group"
	case evidence.CaseHumanInput:
		return "the terminal was not recognized"
	}
	return "no recognized terminal matched the induced case"
}

// Recognize derives the observation one journal row grades from that row's
// own launch and output, never from the fields it is deriving, and reports
// whether a terminal was recognized.
func Recognize(p profile.RuntimeProfile, surface evidence.Surface, caseID evidence.Case, l evidence.LaunchRecord, s evidence.StreamCapture) (evidence.Observation, bool) {
	terminal, transportLoss, found := nativeTerminalFrom(p, surface, s.Combined(), l.Outcome)
	sessionID := resolvedSessionID(p, surface, l, s.Combined())

	if caseID == evidence.CaseHumanInput && found && !caseMatch(caseID, terminal, found, transportLoss) {
		// A recognized terminal that ends the turn without the probe's marker
		// grades gap rather than not_observed: the launch was graded, just not
		// as the induced case.
		detail := "a recognized terminal ended the turn without running the probe"
		if l.ProbeMarker {
			detail = "a recognized terminal ended the turn though the probe's marker file was present"
		}
		return evidence.Observation{
			Grade:        evidence.GradeGap,
			Outcome:      evidence.OutcomePass,
			Detail:       detail,
			SessionID:    sessionID,
			EvidencePath: evidence.SemanticEvidencePath(surface),
		}, true
	}

	if caseMatch(caseID, terminal, found, transportLoss) {
		return evidence.Observation{
			Grade:        evidence.GradeUsable,
			Outcome:      evidence.OutcomePass,
			Detail:       caseMatchDetail(caseID),
			SessionID:    sessionID,
			EvidencePath: evidence.SemanticEvidencePath(surface),
		}, found
	}
	return nativeFailureObservation(sessionID, transportLoss, evidence.OutcomeFixtureInductionFailed, caseMissDetail(caseID)), found
}

// RecognizedTerminal reports whether s carries a terminal the profile's
// recognizer for surface resolves. It grades no row, which is why
// corroborating a declared absence needs it rather than Recognize.
func RecognizedTerminal(p profile.RuntimeProfile, surface evidence.Surface, l evidence.LaunchRecord, s evidence.StreamCapture) bool {
	_, _, found := nativeTerminalFrom(p, surface, s.Combined(), l.Outcome)
	return found
}

// RecognizeContinuationSeed derives one surface's continuation seed
// observation from its own launch.
func RecognizeContinuationSeed(p profile.RuntimeProfile, surface evidence.Surface, l evidence.LaunchRecord, s evidence.StreamCapture) evidence.Observation {
	sessionID := resolvedSessionID(p, surface, l, s.Stdout)
	if surface == evidence.SurfaceProtocol {
		if l.Outcome == evidence.LaunchOutcomeCompleted {
			return evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the seed turn completed and left history", SessionID: l.SeededSessionID}
		}
		return evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the seed turn did not complete", SessionID: l.SeededSessionID}
	}
	terminal, transportLoss, found := nativeTerminalFrom(p, surface, s.Combined(), l.Outcome)
	if found && !terminal.Error {
		return evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the seed launch completed a turn that left history", SessionID: sessionID}
	}
	return nativeFailureObservation(sessionID, transportLoss, evidence.OutcomeRuntimeFailed, "the seed launch did not complete a turn")
}

// The closed recall-evidence states RecognizeContinuationRecall narrows a
// recall attempt to, before it is rendered as the closed detail set an
// evidence.Observation may carry.
type recallEvidence uint8

const (
	recallAttemptFailed recallEvidence = iota
	recallAnswered
	recallSameSessionSilent
	recallDeclined
	recallFreshSession
	recallSessionUnobserved
)

// recallObservation narrows one recall attempt to the record the evidence
// vocabulary admits.
func recallObservation(state recallEvidence, seedSessionID, actualSessionID string) evidence.Observation {
	switch state {
	case recallAnswered:
		return evidence.Observation{
			Grade:     evidence.GradeUsable,
			Outcome:   evidence.OutcomePass,
			Detail:    evidence.RecallConfirmedSameSession,
			SessionID: seedSessionID,
		}
	case recallFreshSession:
		return evidence.Observation{
			Grade:     evidence.GradeGap,
			Outcome:   evidence.OutcomePass,
			Detail:    evidence.RecallFreshFallback,
			SessionID: actualSessionID,
		}
	case recallSameSessionSilent:
		return evidence.Observation{
			Grade:     evidence.GradeGap,
			Outcome:   evidence.OutcomePass,
			Detail:    evidence.RecallSameSessionWithoutRecall,
			SessionID: actualSessionID,
		}
	case recallDeclined:
		return evidence.Observation{
			Grade:     evidence.GradeNotObserved,
			Outcome:   evidence.OutcomeFixtureInductionFailed,
			Detail:    evidence.RecallDeclined,
			SessionID: actualSessionID,
		}
	case recallAttemptFailed, recallSessionUnobserved:
	}
	return evidence.Observation{
		Grade:   evidence.GradeNotObserved,
		Outcome: evidence.OutcomeRuntimeFailed,
		Detail:  evidence.RecallUnobservedActual,
	}
}

// answerCarriesNonce reports whether text carries nonce. An empty nonce never
// matches: a launch that seeded nothing cannot have its memory confirmed by
// every answer a runtime gives.
func answerCarriesNonce(text, nonce string) bool {
	return nonce != "" && strings.Contains(text, nonce)
}

// declineMarkers are the phrases an answer carries when it states
// unwillingness to answer. Some safety training reads the recall prompt as a
// credential-replay request and answers with a policy statement.
var declineMarkers = []string{
	"i will not",
	"i won't",
	"i am not going to",
	"i'm not going to",
	"i am not designed to",
	"i'm not designed to",
	"i am not supposed to",
	"i'm not supposed to",
	"i am not comfortable",
	"i'm not comfortable",
	"i would rather not",
	"i'd rather not",
	"i decline",
}

// historyMarkers are the words an answer uses to speak about the turns before
// it. An answer reporting on its own history says something about the session
// it ran in, so the grade it earns stands.
var historyMarkers = []string{
	"prior",
	"previous",
	"earlier",
	"first message",
	"no record",
	"no memory",
}

// answerDeclinesRecall reports whether text states unwillingness to answer
// the recall prompt, deliberately narrow so a false positive never hides a
// real session loss as merely unmeasured.
func answerDeclinesRecall(text string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(text, "’", "'"))
	for _, marker := range historyMarkers {
		if strings.Contains(normalized, marker) {
			return false
		}
	}
	return slices.ContainsFunc(declineMarkers, func(marker string) bool {
		return strings.Contains(normalized, marker)
	})
}

// RecognizeContinuationRecall derives one surface's continuation recall
// observation from its own launch. For the protocol surface, s.Stdout
// carries accumulated notification text, not a literal stdout stream.
func RecognizeContinuationRecall(p profile.RuntimeProfile, surface evidence.Surface, l evidence.LaunchRecord, s evidence.StreamCapture) evidence.Observation {
	seedSessionID := l.PriorSessionID
	if surface == evidence.SurfaceProtocol {
		actualSessionID := l.SeededSessionID
		switch {
		case l.Outcome != evidence.LaunchOutcomeCompleted:
			return recallObservation(recallAttemptFailed, seedSessionID, actualSessionID)
		case actualSessionID != "" && actualSessionID != seedSessionID:
			return recallObservation(recallFreshSession, seedSessionID, actualSessionID)
		case answerCarriesNonce(s.Stdout, l.Nonce):
			return recallObservation(recallAnswered, seedSessionID, actualSessionID)
		case answerDeclinesRecall(s.Stdout):
			return recallObservation(recallDeclined, seedSessionID, actualSessionID)
		default:
			return recallObservation(recallSameSessionSilent, seedSessionID, actualSessionID)
		}
	}

	terminal, _, found := nativeTerminalFrom(p, surface, s.Combined(), l.Outcome)
	recognizer := p.Recognizers[surface]
	recallSessionID := resolvedSessionID(p, surface, l, s.Combined())
	seedPrompt := strings.ReplaceAll(p.ProbePrompts[promptKeyContinuationSeed], "{nonce}", l.Nonce)
	recallPrompt := p.ProbePrompts[promptKeyContinuationRecall]
	channels := nativeAnswerChannels(recognizer, s.Combined(), recallPrompt, seedPrompt)

	switch {
	case !found || terminal.Error:
		return recallObservation(recallAttemptFailed, seedSessionID, recallSessionID)
	case seedSessionID != "" && recallSessionID != "" && recallSessionID != seedSessionID:
		return recallObservation(recallFreshSession, seedSessionID, recallSessionID)
	case slices.ContainsFunc(channels, func(channel string) bool { return answerCarriesNonce(channel, l.Nonce) }):
		return recallObservation(recallAnswered, seedSessionID, recallSessionID)
	case seedSessionID != "" && recallSessionID != "" && slices.ContainsFunc(channels, answerDeclinesRecall):
		return recallObservation(recallDeclined, seedSessionID, recallSessionID)
	case seedSessionID != "" && recallSessionID != "":
		return recallObservation(recallSameSessionSilent, seedSessionID, recallSessionID)
	default:
		return recallObservation(recallSessionUnobserved, seedSessionID, recallSessionID)
	}
}

// recognizerMachineryMembers names the members recognizer resolves for
// something other than the turn's text, matched at any nesting depth.
func recognizerMachineryMembers(recognizer profile.Recognizer) map[string]bool {
	members := map[string]bool{}
	for _, name := range append([]string{recognizer.Locator.DiscriminatorKey, recognizer.StatusMember}, recognizer.ErrorMembers...) {
		if name != "" {
			members[name] = true
		}
	}
	heads := [][]string{recognizer.SessionIDPath, recognizer.ModelRequestPath}
	for _, path := range recognizer.TokenPaths {
		heads = append(heads, path.Path)
	}
	for _, path := range heads {
		if len(path) > 0 {
			members[path[0]] = true
		}
	}
	return members
}

// The evidence.RuntimeProfile.ProbePrompts keys this package resolves a
// continuation recall's own excluded prompts from, mirrored here because
// the map carries no named constants.
const (
	promptKeyContinuationSeed   = "continuation_seed"
	promptKeyContinuationRecall = "continuation_recall"
)

// nativeAnswerChannels resolves the text channels one native launch
// answered on, accumulating each streamed delta since a partial read would
// answer wrong.
func nativeAnswerChannels(recognizer profile.Recognizer, output string, prompts ...string) []string {
	channels := map[string]*strings.Builder{}
	machinery := recognizerMachineryMembers(recognizer)
	for _, record := range decodeLaunchRecords(output) {
		accumulateAnswerText(record, "", machinery, prompts, channels)
	}
	texts := make([]string, 0, len(channels))
	for _, channel := range channels {
		texts = append(texts, channel.String())
	}
	return texts
}

// accumulateAnswerText appends value's text to the channel each member path
// names, descending through objects and list elements, which share their
// member's channel since a split message is written across them.
func accumulateAnswerText(value any, path string, machinery map[string]bool, prompts []string, channels map[string]*strings.Builder) {
	switch typed := value.(type) {
	case map[string]any:
		for member, nested := range typed {
			if machinery[member] {
				continue
			}
			accumulateAnswerText(nested, path+"."+member, machinery, prompts, channels)
		}
	case []any:
		for _, element := range typed {
			accumulateAnswerText(element, path, machinery, prompts, channels)
		}
	case string:
		if echoesAPrompt(typed, prompts) {
			return
		}
		channel, open := channels[path]
		if !open {
			channel = &strings.Builder{}
			channels[path] = channel
		}
		channel.WriteString(typed)
	}
}

// echoesAPrompt reports whether text is a launch's echo of a prompt this
// run supplied, which is history rather than an answer.
func echoesAPrompt(text string, prompts []string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	for _, prompt := range prompts {
		if trimmed == strings.TrimSpace(prompt) {
			return true
		}
	}
	return false
}

// decodeLaunchRecords decodes one launch's captured output into the JSON
// objects it carries, the way the recognizer reads them: newline-delimited
// objects first, otherwise a stream from the first brace on.
func decodeLaunchRecords(output string) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err == nil {
			records = append(records, record)
		}
	}
	if len(records) > 0 {
		return records
	}
	brace := strings.Index(output, "{")
	if brace < 0 {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(output[brace:]))
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			return records
		}
		records = append(records, record)
	}
}

// ceilingStopUnverified states that no run here can observe a ceiling stop:
// that enforcement lane belongs to the orchestrator, not this measurer.
const ceilingStopUnverified = "no max_tokens stop was induced, so ceiling enforcement stays unverified"

// tokenWildcard is the TokenPath.Path segment naming a level's dynamic key. A
// runtime reporting usage per model carries several keys there, all real
// sources.
const tokenWildcard = "*"

// tokenSurfaceReading is what one surface's token induction resolved. An
// unsupported path is an unknown, not a zero, so reporting the surface as
// source-free would turn an unread value into a proven absence.
type tokenSurfaceReading struct {
	observations int
	sessionID    string
	sources      []evidence.TokenObservation
	unsupported  []string
}

// gradeTokenReading turns one surface's reading into the inventory
// observation, so native and protocol readings are graded by one ladder.
// inductionFailed is the outcome a failed induction carries.
func gradeTokenReading(reading tokenSurfaceReading, inductionFailed evidence.Outcome) (string, []evidence.TokenObservation, evidence.Observation) {
	unknown := func(detail string) (string, []evidence.TokenObservation, evidence.Observation) {
		return "", nil, evidence.Observation{
			Grade:   evidence.GradeNotObserved,
			Outcome: inductionFailed,
			Detail:  detail,
		}
	}

	switch {
	case reading.observations == 0:
		return unknown("no observation was recognized, so this surface's token sources stay unread")
	case len(reading.unsupported) > 0:
		return unknown("read " + countedObservations(reading.observations) + " and reached a shape this inventory cannot consume at " + strings.Join(reading.unsupported, ", "))
	case len(reading.sources) > 0 && reading.sessionID == "":
		return unknown("read " + countedObservations(reading.observations) + " carrying a token-bearing path that no observation attributed to a session")
	case len(reading.sources) == 0:
		return "", nil, evidence.Observation{
			Grade:   evidence.GradeGap,
			Outcome: evidence.OutcomePass,
			Detail:  "read " + countedObservations(reading.observations) + " carrying no token-bearing path",
		}
	}

	return reading.sessionID, reading.sources, evidence.Observation{
		Grade:   evidence.GradeGap,
		Outcome: evidence.OutcomePass,
		Detail: "read " + countedObservations(reading.observations) + " and resolved " + strconv.Itoa(len(reading.sources)) +
			" token-bearing path(s); " + ceilingStopUnverified,
	}
}

func countedObservations(count int) string {
	return strconv.Itoa(count) + " recognized observation(s)"
}

// RecognizeInventory derives one native surface's token_inventory row from
// every launch the journal retains for that surface, in journal order,
// since this row keys on a surface's whole launch set rather than on one
// launch.
func RecognizeInventory(p profile.RuntimeProfile, surface evidence.Surface, s []evidence.StreamCapture) (sessionID string, paths []evidence.TokenObservation, inventory evidence.Observation) {
	recognizer, ok := p.Recognizers[surface]
	if !ok {
		return "", nil, evidence.Observation{
			Grade:   evidence.GradeNotObserved,
			Outcome: evidence.OutcomeFixtureInductionFailed,
			Detail:  "the surface states no recognizer, so no terminal could be read",
		}
	}
	return gradeTokenReading(readNativeTokens(recognizer, s), evidence.OutcomeFixtureInductionFailed)
}

func readNativeTokens(recognizer profile.Recognizer, streams []evidence.StreamCapture) tokenSurfaceReading {
	reading := tokenSurfaceReading{}
	seen := map[string]bool{}
	for _, stream := range streams {
		output := stream.Combined()
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
// terminal. A level with no matching key is the one case read as the
// source being absent.
func resolveTokenSources(terminal map[string]any, declared profile.TokenPath) (sources []evidence.TokenObservation, unsupported []string) {
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

// tokenSourceFor admits a resolved value only as the figure its declared
// kind requires, rejecting a negative or fractional spend count.
func tokenSourceFor(value any, resolved []string, kind string) (evidence.TokenObservation, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return evidence.TokenObservation{}, false
	}
	if kind == "spend" && number != math.Trunc(number) {
		return evidence.TokenObservation{}, false
	}
	return evidence.TokenObservation{EvidencePath: "/" + strings.Join(resolved, "/"), Kind: kind}, true
}
