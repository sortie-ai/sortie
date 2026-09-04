package qualification

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The synthetic identifiers every fixture uses. They are public test
// values only: no credential, path, or captured runtime value.
const (
	FixtureTime      = "2026-03-04T05:06:07Z"
	FixtureAgentName = "fixture-agent"
	FixtureAgentVer  = "1.0.0-fixture"

	FixtureQualified    = "qualified"
	FixtureNotQualified = "not_qualified"
)

// FixtureSession builds the synthetic session identifier for one
// named session on one Surface.
func FixtureSession(Surface Surface, name string) string {
	return fmt.Sprintf("sess-%s-%s", Surface, name)
}

// Fixture is a complete, canonically ordered evidence set. The
// qualified variant records every required observation; the
// not_qualified variant leaves the runtime-refusal disposition and
// non-retryable-refusal retry Cases unobserved on every Surface.
type Fixture struct {
	Records []Record
}

// NewFixture builds the non-final records of one variant in
// canonical order. The runtime-identity records are added by Finalize.
func NewFixture(variant string) *Fixture {
	f := &Fixture{}
	f.addWorkspaceSecurity()
	f.addPolicyPrecondition()
	f.addSemanticProbes()
	f.addBaselines()
	f.addTokenInventories()
	f.addPermission()
	f.addToolServer()
	f.addContinuation()
	f.addEndToEnd()
	f.addProcessCleanup()
	if variant == FixtureNotQualified {
		for _, Surface := range measuredSurfaces {
			f.SetSemanticNotObserved(Surface, CapabilityTurnDisposition, CaseRuntimeRefusal)
			f.SetSemanticNotObserved(Surface, CapabilityRetryClassification, CaseNonRetryableRefusal)
		}
	}
	return f
}

// base returns a Record stub with the fixed identity fields every
// fixture Record shares.
func (f *Fixture) base() Record {
	return Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    FixtureTime,
	}
}

// Add appends one Record.
func (f *Fixture) Add(rec Record) {
	f.Records = append(f.Records, rec)
}

// Finalize appends one runtime-identity record per distinct non-null
// actual protocol session id referenced by the current records, sorts
// the whole set into canonical order, and renumbers the sequences.
func (f *Fixture) Finalize() {
	referenced := map[string]bool{}
	for i := range f.Records {
		rec := &f.Records[i]
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
			referenced[*rec.SessionID] = true
		}
	}
	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		f.Add(IdentityFixtureRecord(id))
	}
	slices.SortStableFunc(f.Records, OrderCompare)
	f.Renumber()
}

// Renumber assigns one-based contiguous Sequence numbers in slice order.
func (f *Fixture) Renumber() {
	for i := range f.Records {
		f.Records[i].Sequence = i + 1
	}
}

// FindFirst returns the first Record matching match, or nil.
func (f *Fixture) FindFirst(match func(*Record) bool) *Record {
	for i := range f.Records {
		rec := &f.Records[i]
		if match(rec) {
			return rec
		}
	}
	return nil
}

// Remove deletes the Record pointed to by target, which must be a
// pointer into the fixture's own slice.
func (f *Fixture) Remove(target *Record) {
	for i := range f.Records {
		if &f.Records[i] == target {
			f.Records = slices.Delete(f.Records, i, i+1)
			return
		}
	}
}

// RemoveAll deletes every Record matching match.
func (f *Fixture) RemoveAll(match func(*Record) bool) {
	f.Records = slices.DeleteFunc(f.Records, func(rec Record) bool {
		return match(&rec)
	})
}

// SetSemanticNotObserved rewrites one semantic Case as not observed and
// rewrites the owning Capability's baseline to the newly derived Grade.
func (f *Fixture) SetSemanticNotObserved(Surface Surface, Capability Capability, caseID Case) {
	rec := f.FindFirst(MatchSemantic(Surface, Capability, caseID))
	if rec == nil {
		return
	}
	rec.Outcome = OutcomeNotObserved
	rec.Grade = GradeNotObserved
	rec.SessionID = nil
	rec.EvidencePath = nil
	rec.Detail = fmt.Sprintf("%s %s Case was not observed on %s", Capability, caseID, Surface)
	f.UpdateSemanticBaseline(Surface, Capability)
}

// UpdateSemanticBaseline recomputes one Surface-Capability baseline from
// that Capability's current Case records and writes the derived Grade.
func (f *Fixture) UpdateSemanticBaseline(Surface Surface, Capability Capability) {
	var classes []Grade
	for _, caseID := range CapabilityCases[Capability] {
		rec := f.FindFirst(MatchSemantic(Surface, Capability, caseID))
		if rec == nil {
			return
		}
		classes = append(classes, rec.Grade)
	}
	baseline := f.FindFirst(MatchBaseline(Surface, Capability))
	if baseline != nil {
		baseline.Grade = DeriveBaselineGrade(classes)
		baseline.Outcome = BaselineVerdictFor(baseline.Grade)
	}
}

// addWorkspaceSecurity adds the single aggregate workspace observation.
func (f *Fixture) addWorkspaceSecurity() {
	rec := f.base()
	rec.Scenario = ScenarioWorkspaceSecurity
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityWorkspaceSecurity
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputSecurity
	rec.EvidencePath = new("/workspace/settings")
	rec.Detail = "controlled checkout without project settings; run-scoped homes; allowlisted environment names only; skip-trust accepted"
	f.Add(rec)
}

// addPolicyPrecondition adds the single policy-denial precondition
// Record, referencing the protocol control session.
func (f *Fixture) addPolicyPrecondition() {
	rec := f.base()
	rec.Scenario = ScenarioPolicyPrecondition
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityPermissionHandling
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputPolicyControl
	rec.EvidencePath = new("policy.deny_marker")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "policy"))
	rec.AgentVersion = new(FixtureAgentVer)
	rec.Detail = "policy deny marker returned and the probe side effect stayed absent"
	f.Add(rec)
}

// addSemanticProbes adds all 36 semantic records in canonical order.
func (f *Fixture) addSemanticProbes() {
	for _, Surface := range measuredSurfaces {
		for _, Capability := range []Capability{CapabilityTurnDisposition, CapabilityRetryClassification} {
			for _, caseID := range CapabilityCases[Capability] {
				f.Add(f.semanticRecord(Surface, Capability, caseID))
			}
		}
	}
}

// semanticRecord builds one semantic probe Record. The native text
// Surface is characterized as unstructured residue, so its Cases Grade
// gap; every other Surface Grades usable. Refusal retry records reuse
// the refusal disposition session, and the protocol human-input Record
// reuses the permission attempt session.
func (f *Fixture) semanticRecord(Surface Surface, Capability Capability, caseID Case) Record {
	rec := f.base()
	rec.Scenario = ScenarioSemanticProbe
	rec.Surface = Surface
	rec.Capability = Capability
	rec.SemanticCase = new(caseID)
	rec.InputID = CaseInputs[caseID]
	rec.Outcome = OutcomePass
	rec.Grade = GradeUsable
	switch Surface {
	case SurfaceProtocol:
		rec.Source = SourceProtocolStable
		rec.EvidencePath = new("/turn/stop_reason")
		rec.AgentName = new(FixtureAgentName)
		rec.AgentVersion = new(FixtureAgentVer)
		rec.ProtocolVersion = new(1)
	case SurfaceNativeText:
		rec.Source = SourceNativeText
		rec.Grade = GradeGap
		rec.EvidencePath = new("/text/final")
		rec.AgentVersion = new(FixtureAgentVer)
	default:
		rec.Source = SourceNativeStructured
		if Surface == SurfaceNativeJSON {
			rec.EvidencePath = new("/response/terminal")
		} else {
			rec.EvidencePath = new("/stream/terminal")
		}
		rec.AgentVersion = new(FixtureAgentVer)
	}

	switch {
	case caseID == CaseRuntimeRefusal:
		rec.SessionID = new(FixtureSession(Surface, "runtime-refusal"))
	case caseID == CaseNonRetryableRefusal:
		rec.SessionID = new(FixtureSession(Surface, "runtime-refusal"))
	case caseID == CaseHumanInput && Surface == SurfaceProtocol:
		rec.SessionID = new(FixtureSession(Surface, "permission"))
	case caseID == CaseHumanInput:
		rec.SessionID = new(FixtureSession(Surface, "human-input"))
	default:
		rec.SessionID = new(FixtureSession(Surface, string(caseID)))
	}
	rec.Detail = fmt.Sprintf("%s %s Case completed with a distinct Outcome on %s", Capability, caseID, Surface)
	return rec
}

// BaselineVerdictFor returns the Outcome a derived baseline Record
// carries for its Grade: not_observed Grades are not_observed Outcomes,
// every other Grade is a completed probe.
func BaselineVerdictFor(classification Grade) Outcome {
	if classification == GradeNotObserved {
		return OutcomeNotObserved
	}
	return OutcomePass
}

// addBaselines adds the 16 derived per-Surface Capability summaries.
func (f *Fixture) addBaselines() {
	for _, Surface := range measuredSurfaces {
		for _, Capability := range comparisonCapabilities {
			rec := f.base()
			rec.Scenario = ScenarioSurfaceBaseline
			rec.Surface = Surface
			rec.Capability = Capability
			rec.Source = SourceComparison
			rec.InputID = InputBaseline
			rec.EvidencePath = new(fmt.Sprintf("/comparison/%s/%s", Surface, Capability))
			rec.Detail = fmt.Sprintf("derived %s Grade for %s", Capability, Surface)
			switch Capability {
			case CapabilityTurnDisposition, CapabilityRetryClassification:
				var classes []Grade
				for _, caseID := range CapabilityCases[Capability] {
					classes = append(classes, f.FindFirst(MatchSemantic(Surface, Capability, caseID)).Grade)
				}
				rec.Grade = DeriveBaselineGrade(classes)
			case CapabilityTokenCeiling:
				rec.Grade = GradeUsable
				if Surface == SurfaceNativeText {
					rec.Grade = GradeGap
				}
			case CapabilitySessionContinuation:
				rec.Grade = GradeUsable
			}
			rec.Outcome = BaselineVerdictFor(rec.Grade)
			f.Add(rec)
		}
	}
}

// addTokenInventories adds each Surface's token-bearing paths, with the
// native text Surface carried by the zero-Source sentinel.
func (f *Fixture) addTokenInventories() {
	f.Add(f.tokenRecord(SurfaceProtocol, "session/prompt/result/usage",
		SourceProtocolStable, GradeUsable,
		FixtureSession(SurfaceProtocol, "success"),
		"standard usage member on the final prompt result"))
	f.Add(f.tokenRecord(SurfaceProtocol, "session/update/vendor_usage",
		SourceProtocolExtension, GradeCorroborationOnly,
		FixtureSession(SurfaceProtocol, "runtime-failure"),
		"vendor extension reports totals outside the shared contract"))
	f.Add(f.tokenRecord(SurfaceNativeJSON, "/response/stats",
		SourceNativeStructured, GradeUsable,
		FixtureSession(SurfaceNativeJSON, "success"),
		"structured response carries the usage stats member"))
	f.Add(f.tokenRecord(SurfaceNativeStreamJSON, "/stream/final/usage",
		SourceNativeStructured, GradeUsable,
		FixtureSession(SurfaceNativeStreamJSON, "success"),
		"final stream event carries the usage member"))

	sentinel := f.base()
	sentinel.Scenario = ScenarioTokenSource
	sentinel.Surface = SurfaceNativeText
	sentinel.Capability = CapabilityTokenCeiling
	sentinel.Source = SourceNone
	sentinel.Grade = GradeGap
	sentinel.Outcome = OutcomePass
	sentinel.InputID = InputTokenInventory
	sentinel.Detail = "inventory completed with no token-bearing path"
	f.Add(sentinel)
}

// tokenRecord builds one non-sentinel token inventory Record.
func (f *Fixture) tokenRecord(Surface Surface, path string, Source Source, classification Grade, SessionID, Detail string) Record {
	rec := f.base()
	rec.Scenario = ScenarioTokenSource
	rec.Surface = Surface
	rec.Capability = CapabilityTokenCeiling
	rec.Source = Source
	rec.Grade = classification
	rec.Outcome = OutcomePass
	rec.InputID = InputTokenInventory
	rec.EvidencePath = new(path)
	rec.SessionID = new(SessionID)
	rec.Detail = Detail
	if Surface == SurfaceProtocol {
		rec.AgentName = new(FixtureAgentName)
		rec.AgentVersion = new(FixtureAgentVer)
		rec.ProtocolVersion = new(1)
	} else {
		rec.AgentVersion = new(FixtureAgentVer)
	}
	return rec
}

// addPermission adds the single protocol permission Record.
func (f *Fixture) addPermission() {
	rec := f.base()
	rec.Scenario = ScenarioPermissionRequest
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityPermissionHandling
	rec.Source = SourceProtocolStable
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputPermissionProbe
	rec.EvidencePath = new("session/request_permission")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "permission"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "request answered with a refusing option and no request left pending"
	f.Add(rec)
}

// addToolServer adds the single protocol MCP delivery Record.
func (f *Fixture) addToolServer() {
	rec := f.base()
	rec.Scenario = ScenarioToolServer
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityToolServerDelivery
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputMCPProbe
	rec.EvidencePath = new("mcp_server.receipt")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "mcp"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "test server received the generated nonce and the turn consumed it"
	f.Add(rec)
}

// addContinuation adds one seed and one confirmed same-session recall per
// Surface.
func (f *Fixture) addContinuation() {
	for _, Surface := range measuredSurfaces {
		seedSession := FixtureSession(Surface, "seed")

		seed := f.base()
		seed.Scenario = ScenarioContinuation
		seed.Surface = Surface
		seed.Capability = CapabilitySessionContinuation
		seed.InputID = InputContinuationSeed
		seed.Outcome = OutcomePass
		seed.Grade = GradeUsable
		seed.Source = SourceNativeStructured
		if Surface == SurfaceProtocol {
			seed.Source = SourceProtocolStable
			seed.AgentName = new(FixtureAgentName)
			seed.AgentVersion = new(FixtureAgentVer)
			seed.ProtocolVersion = new(1)
		} else {
			seed.AgentVersion = new(FixtureAgentVer)
		}
		seed.EvidencePath = new("/continuation/seed")
		seed.SessionID = new(seedSession)
		seed.Detail = "seed conversation stored the generated nonce"
		f.Add(seed)

		recall := f.base()
		recall.Scenario = ScenarioContinuation
		recall.Surface = Surface
		recall.Capability = CapabilitySessionContinuation
		recall.InputID = InputContinuationRecall
		recall.Outcome = OutcomePass
		recall.Grade = GradeUsable
		recall.Source = seed.Source
		recall.EvidencePath = new("/continuation/recall")
		recall.SessionID = new(seedSession)
		recall.PriorSessionID = new(seedSession)
		recall.Detail = recallConfirmedSameSession
		if Surface == SurfaceProtocol {
			recall.AgentName = new(FixtureAgentName)
			recall.AgentVersion = new(FixtureAgentVer)
			recall.ProtocolVersion = new(1)
		} else {
			recall.AgentVersion = new(FixtureAgentVer)
		}
		f.Add(recall)
	}
}

// addEndToEnd adds the single isolated end-to-end Record.
func (f *Fixture) addEndToEnd() {
	rec := f.base()
	rec.Scenario = ScenarioEndToEnd
	rec.Surface = SurfaceProtocol
	rec.Capability = CapabilityTurnDisposition
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputE2E
	rec.EvidencePath = new("/run_history/status")
	rec.SessionID = new(FixtureSession(SurfaceProtocol, "e2e"))
	rec.AgentName = new(FixtureAgentName)
	rec.AgentVersion = new(FixtureAgentVer)
	rec.ProtocolVersion = new(1)
	rec.Detail = "one succeeded history row and the issue reached its handoff state"
	f.Add(rec)
}

// addProcessCleanup adds the single aggregate cleanup Record.
func (f *Fixture) addProcessCleanup() {
	rec := f.base()
	rec.Scenario = ScenarioProcessCleanup
	rec.Surface = SurfaceAggregate
	rec.Capability = CapabilityProcessCleanup
	rec.Source = SourceProcessObservation
	rec.Grade = GradeUsable
	rec.Outcome = OutcomePass
	rec.InputID = InputCleanup
	rec.EvidencePath = new("process_group.liveness")
	rec.Detail = "checked_groups=9"
	f.Add(rec)
}

// MatchSemantic matches one semantic tuple.
func MatchSemantic(Surface Surface, Capability Capability, caseID Case) func(*Record) bool {
	return func(rec *Record) bool {
		if rec.Scenario != ScenarioSemanticProbe || rec.Surface != Surface ||
			rec.Capability != Capability || rec.SemanticCase == nil {
			return false
		}
		return *rec.SemanticCase == caseID
	}
}

// MatchBaseline matches one Surface-Capability baseline Record.
func MatchBaseline(Surface Surface, Capability Capability) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioSurfaceBaseline &&
			rec.Surface == Surface && rec.Capability == Capability
	}
}

// MatchContinuation matches one Surface's seed or recall Record.
func MatchContinuation(Surface Surface, InputID InputID) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioContinuation && rec.Surface == Surface && rec.InputID == InputID
	}
}

// WriteEvidenceFile serializes records as UTF-8 JSON Lines under
// the test's temporary directory and returns the file path.
func WriteEvidenceFile(T *testing.T, records []Record) string {
	T.Helper()
	dir := filepath.Join(T.TempDir(), "qualification")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		T.Fatalf("create evidence directory: %v", err)
	}
	path := filepath.Join(dir, "evidence.jsonl")
	var lines []string
	for _, rec := range records {
		line, err := MarshalRecord(rec)
		if err != nil {
			T.Fatalf("marshal evidence Record %d: %v", rec.Sequence, err)
		}
		lines = append(lines, string(line))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		T.Fatalf("write evidence file: %v", err)
	}
	return path
}

// WriteFinalEvidenceFile serializes a non-final set plus the single
// terminal aggregate Record and returns the file path.
func WriteFinalEvidenceFile(T *testing.T, records []Record, classification Grade) string {
	T.Helper()
	complete := append(slices.Clone(records), aggregateFixtureRecord(classification))
	for i := range complete {
		complete[i].Sequence = i + 1
	}
	return WriteEvidenceFile(T, complete)
}

// RequireObservationVerdict validates a first-pass file and fails T
// unless it validates with the wanted Verdict.
func RequireObservationVerdict(T *testing.T, path string, wantVerdict Verdict) {
	T.Helper()
	v, err := ValidateObservations(path)
	if err != nil {
		T.Errorf("ValidateObservations(%s) error = %v, want nil", path, err)
		return
	}
	if v != wantVerdict {
		T.Errorf("ValidateObservations(%s) = %q, want %q", path, v, wantVerdict)
	}
}

// IdentityFixtureRecord builds one runtime-identity record for one
// actual protocol session.
func IdentityFixtureRecord(SessionID string) Record {
	return Record{
		SchemaVersion:   1,
		Sequence:        0,
		ObservedAt:      FixtureTime,
		Scenario:        ScenarioRuntimeIdentity,
		Surface:         SurfaceProtocol,
		Capability:      CapabilityRuntimeIdentity,
		Source:          SourceProtocolStable,
		Grade:           GradeUsable,
		Outcome:         OutcomePass,
		InputID:         InputIdentity,
		EvidencePath:    new("/handshake/agent_info"),
		SessionID:       new(SessionID),
		AgentName:       new(FixtureAgentName),
		AgentVersion:    new(FixtureAgentVer),
		ProtocolVersion: new(1),
		Detail:          "handshake reported agent name and version",
	}
}

// aggregateFixtureRecord builds the terminal qualification Record
// with the given classification.
func aggregateFixtureRecord(classification Grade) Record {
	return Record{
		SchemaVersion: 1,
		Sequence:      0,
		ObservedAt:    FixtureTime,
		Scenario:      ScenarioQualification,
		Surface:       SurfaceAggregate,
		Capability:    CapabilityEligibility,
		Source:        SourceComparison,
		Grade:         classification,
		Outcome:       OutcomePass,
		InputID:       InputAggregate,
		EvidencePath:  new(EvidencePathQualificationVerdict),
		Detail:        "aggregate qualification Verdict recomputed from the closed non-final evidence set",
	}
}

// RequireFinalVerdict validates a final-pass file and fails T unless it
// validates with the wanted Verdict.
func RequireFinalVerdict(T *testing.T, path string, wantVerdict Verdict) {
	T.Helper()
	v, err := ValidateEvidence(path)
	if err != nil {
		T.Errorf("ValidateEvidence(%s) error = %v, want nil", path, err)
		return
	}
	if v != wantVerdict {
		T.Errorf("ValidateEvidence(%s) = %q, want %q", path, v, wantVerdict)
	}
}

// SetTokenSentinel replaces one Surface's token inventory with the single
// sentinel Record: a zero-Source success when failed is false and a
// failed inventory otherwise. The token baseline is rewritten to the
// derived Grade.
func (f *Fixture) SetTokenSentinel(Surface Surface, failed bool) {
	f.RemoveAll(matchTokenSurface(Surface))
	sentinel := f.base()
	sentinel.Scenario = ScenarioTokenSource
	sentinel.Surface = Surface
	sentinel.Capability = CapabilityTokenCeiling
	sentinel.Source = SourceNone
	sentinel.Outcome = OutcomePass
	sentinel.InputID = InputTokenInventory
	if failed {
		sentinel.Grade = GradeNotObserved
		sentinel.Outcome = OutcomeRuntimeFailed
		sentinel.Detail = "token inventory collection failed"
	} else {
		sentinel.Grade = GradeGap
		sentinel.Detail = "inventory completed with no token-bearing path"
	}
	f.Add(sentinel)

	baseline := f.FindFirst(MatchBaseline(Surface, CapabilityTokenCeiling))
	if baseline != nil {
		baseline.Grade = GradeGap
		if failed {
			baseline.Grade = GradeNotObserved
		}
		baseline.Outcome = BaselineVerdictFor(baseline.Grade)
	}
}

// matchTokenSurface matches one Surface's token inventory records.
func matchTokenSurface(Surface Surface) func(*Record) bool {
	return func(rec *Record) bool {
		return rec.Scenario == ScenarioTokenSource && rec.Surface == Surface
	}
}

// SetTokenCorroborationOnly rewrites every non-sentinel token Record of
// one Surface to corroboration_only and the token baseline to gap, so
// the inventory completed but supplied no contract-usable Source.
func (f *Fixture) SetTokenCorroborationOnly(Surface Surface) {
	for i := range f.Records {
		rec := &f.Records[i]
		if matchTokenSurface(Surface)(rec) && rec.EvidencePath != nil {
			rec.Grade = GradeCorroborationOnly
		}
	}
	if baseline := f.FindFirst(MatchBaseline(Surface, CapabilityTokenCeiling)); baseline != nil {
		baseline.Grade = GradeGap
	}
}

// DuplicateAfter inserts a copy of target directly behind it, keeping
// the canonical ordering intact so the duplicate key check is the check
// that fires.
func (f *Fixture) DuplicateAfter(target *Record) {
	for i := range f.Records {
		if &f.Records[i] == target {
			f.Records = slices.Insert(f.Records, i+1, *target)
			return
		}
	}
}

// AppendIdentity adds one runtime-identity record for SessionID at its
// canonical position. Controls that introduce a new actual protocol
// session id after Finalize call this to keep the identity set complete.
func (f *Fixture) AppendIdentity(SessionID string) {
	f.Add(IdentityFixtureRecord(SessionID))
	slices.SortStableFunc(f.Records, OrderCompare)
	f.Renumber()
}

// TokenRecordCount counts records with the token-source scenario.
func TokenRecordCount(records []Record) int {
	count := 0
	for i := range records {
		if records[i].Scenario == ScenarioTokenSource {
			count++
		}
	}
	return count
}

// ProtocolSessionCount counts the distinct non-null actual protocol
// session ids a record set references.
func ProtocolSessionCount(records []Record) int {
	ids := map[string]bool{}
	for i := range records {
		rec := &records[i]
		if rec.Surface == SurfaceProtocol && rec.SessionID != nil {
			ids[*rec.SessionID] = true
		}
	}
	return len(ids)
}

// ComparisonCapabilities is the set of capabilities measured in
// comparison scenarios.
var ComparisonCapabilities = []Capability{
	CapabilityTurnDisposition, CapabilityRetryClassification,
	CapabilityTokenCeiling, CapabilitySessionContinuation,
}

// MeasuredSurfaces is the set of surfaces measured in protocol records.
var MeasuredSurfaces = []Surface{
	SurfaceProtocol, SurfaceNativeText,
	SurfaceNativeJSON, SurfaceNativeStreamJSON,
}

// ReadEvidenceFile reads a JSONL evidence file and strictly decodes
// all records.
func ReadEvidenceFile(path string) ([]Record, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller supplies a path to a file it wrote under its own temp directory
	if err != nil {
		return nil, err
	}
	var records []Record
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rec, err := DecodeRecord([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil, errors.New("evidence file contains no records")
	}
	return records, nil
}
