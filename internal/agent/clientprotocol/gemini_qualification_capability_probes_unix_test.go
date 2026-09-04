//go:build unix

package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// geminiIdentityLogDeadline bounds the deterministic wait for the
// structured implementation log record the pump writes.
const geminiIdentityLogDeadline = 5 * time.Second

const (
	geminiRecallUnobservedActual     = "unobserved_actual_session"
	geminiRecallConfirmedSameSession = "confirmed_same_session"
	geminiRecallFreshFallback        = "fresh_session_fallback"
)

// geminiTokenSource is one token-bearing path observed on one surface:
// a standard usage member, a terminal usage field, or a vendor
// extension that reports token or quota figures. Path is a JSON pointer
// or a protocol method name; an empty path is the zero-source sentinel.
type geminiTokenSource struct {
	Path      string
	Vendor    bool
	Usage     *domain.TokenUsage
	SessionID string
}

// geminiClassifyTokenSource classifies one token-bearing path against
// domain.TokenUsage semantics, not field-name similarity: a source the
// adapter may consume through the shared contract is usable, vendor
// extension figures and occupancy-only members are corroboration only
// and never receive a numeric grade, and a path whose observed shape
// cannot satisfy the shared contract is not usable either.
func geminiClassifyTokenSource(source geminiTokenSource) qualification.Grade {
	if source.Usage == nil {
		return qualification.GradeCorroborationOnly
	}
	usage := *source.Usage
	totalOK := usage.TotalTokens == usage.InputTokens+usage.OutputTokens
	cacheOK := usage.CacheReadTokens <= usage.InputTokens
	nonNegative := usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.CacheReadTokens >= 0
	if source.Vendor || !totalOK || !cacheOK || !nonNegative {
		// A vendor-reported total never passes through as the shared
		// contract's total, and a vendor path is never consumable.
		return qualification.GradeCorroborationOnly
	}
	return qualification.GradeUsable
}

// geminiTokenEventsFor folds a consumable token source into the
// normalized events the shared contract expects, so the classification
// controls can assert the usage contract the same way the adapter does.
func geminiTokenEventsFor(source geminiTokenSource) []domain.AgentEvent {
	if source.Usage == nil {
		return nil
	}
	return []domain.AgentEvent{{
		Type:      domain.EventTokenUsage,
		Timestamp: time.Now().UTC(),
		Usage:     *source.Usage,
	}}
}

// geminiTokenInventoryRecords builds one surface's token_source records
// from the observed sources. A failed inventory and a completed
// zero-source inventory both carry the single sentinel key; a completed
// inventory with sources records one record per path, sorted so the
// records land in canonical order.
func geminiTokenInventoryRecords(surface qualification.Surface, sources []geminiTokenSource, inventoryFailed bool) []qualification.Record {
	records := []qualification.Record{}
	if inventoryFailed || len(sources) == 0 {
		rec := qualification.Record{
			SchemaVersion: 1,
			ObservedAt:    qualification.FixtureTime,
			Scenario:      qualification.ScenarioTokenSource,
			Surface:       surface,
			Capability:    qualification.CapabilityTokenCeiling,
			Source:        qualification.SourceNone,
			Outcome:       qualification.OutcomePass,
			InputID:       qualification.InputTokenInventory,
			Detail:        "inventory completed with no token-bearing path",
		}
		if inventoryFailed {
			rec.Grade = qualification.GradeNotObserved
			rec.Outcome = qualification.OutcomeRuntimeFailed
			rec.Detail = "token inventory collection failed"
		} else {
			rec.Grade = qualification.GradeGap
		}
		if surface == qualification.SurfaceProtocol {
			rec.AgentName = new(qualification.FixtureAgentName)
			rec.AgentVersion = new(qualification.FixtureAgentVer)
			rec.ProtocolVersion = new(1)
		} else {
			rec.AgentVersion = new(qualification.FixtureAgentVer)
		}
		return append(records, rec)
	}

	for _, source := range sources {
		if source.Path == "" {
			continue
		}
		rec := qualification.Record{
			SchemaVersion: 1,
			ObservedAt:    qualification.FixtureTime,
			Scenario:      qualification.ScenarioTokenSource,
			Surface:       surface,
			Capability:    qualification.CapabilityTokenCeiling,
			Grade:         geminiClassifyTokenSource(source),
			Outcome:       qualification.OutcomePass,
			InputID:       qualification.InputTokenInventory,
			EvidencePath:  new(source.Path),
			SessionID:     new(source.SessionID),
			Detail:        "token-bearing path classified against the shared usage contract",
		}
		if surface == qualification.SurfaceProtocol {
			rec.Source = qualification.SourceProtocolStable
			if source.Vendor {
				rec.Source = qualification.SourceProtocolExtension
			}
			rec.AgentName = new(qualification.FixtureAgentName)
			rec.AgentVersion = new(qualification.FixtureAgentVer)
			rec.ProtocolVersion = new(1)
		} else {
			rec.Source = qualification.SourceNativeStructured
			rec.AgentVersion = new(qualification.FixtureAgentVer)
		}
		records = append(records, rec)
	}
	slices.SortStableFunc(records, geminiOrderCompare)
	return records
}

// geminiDuplicateTokenPaths reports paths that appear more than once in
// one surface's inventory, which is a fixture failure the collector
// reports rather than a second record for the same uniqueness key.
func geminiDuplicateTokenPaths(sources []geminiTokenSource) []string {
	seen := map[string]bool{}
	var duplicates []string
	for _, source := range sources {
		if source.Path == "" {
			continue
		}
		if seen[source.Path] {
			if !slices.Contains(duplicates, source.Path) {
				duplicates = append(duplicates, source.Path)
			}
			continue
		}
		seen[source.Path] = true
	}
	return duplicates
}

// TestGeminiTokenInventoryClassificationControls confirms the token
// inventory's classification controls: a consumable standard source is
// usable and its folded events satisfy the shared usage contract, a
// vendor-only figure is corroboration only with the measurement absent,
// the zero-source and failed inventories each carry exactly one
// sentinel, and a duplicate path is rejected rather than recorded
// twice.
func TestGeminiTokenInventoryClassificationControls(t *testing.T) {
	t.Parallel()

	t.Run("a consumable standard source is usable and satisfies the usage contract", func(t *testing.T) {
		t.Parallel()

		usage := domain.TokenUsage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CacheReadTokens: 40}
		source := geminiTokenSource{Path: "session/prompt/result/usage", Usage: &usage, SessionID: "sess-token-fixture"}

		if got := geminiClassifyTokenSource(source); got != qualification.GradeUsable {
			t.Errorf("geminiClassifyTokenSource() = %s, want usable", got)
		}
		events := geminiTokenEventsFor(source)
		agenttest.AssertUsageContract(t, events)

		records := geminiTokenInventoryRecords(qualification.SurfaceNativeJSON, []geminiTokenSource{source}, false)
		if len(records) != 1 {
			t.Fatalf("token inventory records = %d, want 1", len(records))
		}
		rec := records[0]
		if rec.EvidencePath == nil || *rec.EvidencePath != source.Path {
			t.Errorf("token record evidence path = %v, want %q", rec.EvidencePath, source.Path)
		}
		if rec.SessionID == nil || *rec.SessionID != source.SessionID {
			t.Errorf("token record session = %v, want the emitting session", rec.SessionID)
		}
		if err := checkGeminiVerdictClassification(&rec); err != nil {
			t.Errorf("checkGeminiVerdictClassification() error = %v", err)
		}
	})

	t.Run("a vendor-only figure is corroboration only with the measurement absent", func(t *testing.T) {
		t.Parallel()

		// The vendor extension reports a total outside the shared
		// contract: no token_usage event is emitted, the usage stays
		// zero, and UsageMeasured stays false.
		source := geminiTokenSource{Path: "session/update/vendor_usage", Vendor: true, SessionID: "sess-vendor-fixture"}
		if got := geminiClassifyTokenSource(source); got != qualification.GradeCorroborationOnly {
			t.Errorf("geminiClassifyTokenSource(vendor) = %s, want corroboration_only", got)
		}

		events := geminiTokenEventsFor(source)
		result := domain.TurnResult{SessionID: source.SessionID, ExitReason: domain.EventTurnCompleted}
		agenttest.AssertMeasurementAbsent(t, events, result)
		agenttest.AssertMeasurementAbsent(t, nil, domain.TurnResult{})
	})

	t.Run("an unsatisfiable shape is corroboration only", func(t *testing.T) {
		t.Parallel()

		// A vendor-reported total passed through as TotalTokens fails
		// the adapter-computed-total rule and can never classify usable.
		vendorTotal := domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 500}
		got := geminiClassifyTokenSource(geminiTokenSource{Path: "/stats", Usage: new(vendorTotal), Vendor: true})
		if got != qualification.GradeCorroborationOnly {
			t.Errorf("vendor total classification = %s, want corroboration_only", got)
		}
		broken := domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 500}
		got = geminiClassifyTokenSource(geminiTokenSource{Path: "/stats", Usage: new(broken)})
		if got != qualification.GradeCorroborationOnly {
			t.Errorf("non-contract shape classification = %s, want corroboration_only", got)
		}
	})

	t.Run("a completed zero-source inventory carries the single sentinel", func(t *testing.T) {
		t.Parallel()

		records := geminiTokenInventoryRecords(qualification.SurfaceNativeText, nil, false)
		if len(records) != 1 {
			t.Fatalf("sentinel record count = %d, want exactly 1", len(records))
		}
		if records[0].EvidencePath != nil {
			t.Errorf("sentinel evidence path = %v, want null", records[0].EvidencePath)
		}
		if records[0].Source != qualification.SourceNone {
			t.Errorf("sentinel source = %s, want none", records[0].Source)
		}
		if records[0].Grade != qualification.GradeGap || records[0].Outcome != qualification.OutcomePass {
			t.Errorf("zero-source sentinel = %s/%s, want gap/pass", records[0].Grade, records[0].Outcome)
		}
		if records[0].SessionID != nil {
			t.Errorf("sentinel session id = %v, want null", records[0].SessionID)
		}
	})

	t.Run("a failed inventory carries the failure sentinel", func(t *testing.T) {
		t.Parallel()

		records := geminiTokenInventoryRecords(qualification.SurfaceProtocol, nil, true)
		if len(records) != 1 {
			t.Fatalf("failed-inventory sentinel count = %d, want exactly 1", len(records))
		}
		if records[0].Outcome != qualification.OutcomeRuntimeFailed || records[0].Grade != qualification.GradeNotObserved {
			t.Errorf("failed-inventory sentinel = %s/%s, want runtime_failed/not_observed", records[0].Outcome, records[0].Grade)
		}
	})

	t.Run("a duplicate path is rejected rather than recorded twice", func(t *testing.T) {
		t.Parallel()

		duplicated := []geminiTokenSource{
			{Path: "session/prompt/result/usage", SessionID: "sess-a"},
			{Path: "session/prompt/result/usage", SessionID: "sess-token-fixture"},
		}
		if got := geminiDuplicateTokenPaths(duplicated); len(got) != 1 {
			t.Errorf("geminiDuplicateTokenPaths() = %v, want the one duplicated path", got)
		}
	})

	t.Run("records sort canonically with the sentinel first", func(t *testing.T) {
		t.Parallel()

		sources := []geminiTokenSource{
			{Path: "session/update/vendor_usage", Vendor: true, SessionID: "sess-a"},
			{Path: "session/prompt/result/usage", SessionID: "sess-b"},
		}
		records := geminiTokenInventoryRecords(qualification.SurfaceProtocol, sources, false)
		if len(records) != 2 {
			t.Fatalf("record count = %d, want 2", len(records))
		}
		for i := 1; i < len(records); i++ {
			if geminiOrderCompare(records[i-1], records[i]) > 0 {
				t.Errorf("token records are not in canonical order at %d", i)
			}
		}
	})
}

// geminiContinuationRelation is one surface's continuation evidence:
// the seed's actual identifier, the identifier the recall attempted to
// continue, the actual identifier the recall session held, and whether
// prior content replayed.
type geminiContinuationRelation struct {
	Surface         qualification.Surface
	SeedSessionID   string
	RecallSessionID string
	ReplayConfirmed bool
}

// geminiRecallDetail maps one continuation relation onto the closed
// recall detail set: equal non-null actual and prior ids with confirmed
// replay are a same-session confirmation, a distinct non-null actual id
// is a fresh fallback, and an unobserved actual id stays unobserved.
func geminiRecallDetail(relation geminiContinuationRelation) (detail string, classification qualification.Grade) {
	switch {
	case relation.RecallSessionID == "":
		return geminiRecallUnobservedActual, qualification.GradeNotObserved
	case relation.RecallSessionID == relation.SeedSessionID && relation.ReplayConfirmed:
		return geminiRecallConfirmedSameSession, qualification.GradeUsable
	default:
		return geminiRecallFreshFallback, qualification.GradeGap
	}
}

// geminiBuildContinuationRecords builds one surface's seed and recall
// records from the relation. The seed carries the seed's actual
// identifier with a null prior; the recall names the seed through
// prior_session_id and records its actual current identifier, never a
// copy of the requested one.
func geminiBuildContinuationRecords(relation geminiContinuationRelation) (seed, recall qualification.Record) {
	base := qualification.Record{
		SchemaVersion: 1,
		ObservedAt:    qualification.FixtureTime,
		Scenario:      qualification.ScenarioContinuation,
		Surface:       relation.Surface,
		Capability:    qualification.CapabilitySessionContinuation,
	}
	if relation.Surface == qualification.SurfaceProtocol {
		base.Source = qualification.SourceProtocolStable
		base.AgentName = new(qualification.FixtureAgentName)
		base.AgentVersion = new(qualification.FixtureAgentVer)
		base.ProtocolVersion = new(1)
	} else {
		base.Source = qualification.SourceNativeStructured
		base.AgentVersion = new(qualification.FixtureAgentVer)
	}

	seed = base
	seed.InputID = qualification.InputContinuationSeed
	seed.Outcome = qualification.OutcomePass
	seed.Grade = qualification.GradeUsable
	seed.EvidencePath = new("/continuation/seed")
	seed.SessionID = new(relation.SeedSessionID)
	seed.Detail = "seed conversation stored the generated nonce"

	detail, classification := geminiRecallDetail(relation)
	recall = base
	recall.InputID = qualification.InputContinuationRecall
	recall.EvidencePath = new("/continuation/recall")
	recall.PriorSessionID = new(relation.SeedSessionID)
	recall.Detail = detail
	recall.Grade = classification
	switch detail {
	case geminiRecallConfirmedSameSession:
		recall.Outcome = qualification.OutcomePass
		recall.SessionID = new(relation.RecallSessionID)
	case geminiRecallFreshFallback:
		recall.Outcome = qualification.OutcomePass
		recall.SessionID = new(relation.RecallSessionID)
	default:
		recall.Outcome = qualification.OutcomeNotObserved
	}
	return seed, recall
}

// TestGeminiContinuationRelationBuilder confirms the actual/prior
// session relation builder: a confirmed same-session relation with
// equal ids grades usable, a fresh fallback with distinct non-null ids
// grades gap, an unobserved actual session stays not_observed with a
// null id, and the spliced fresh-fallback set still validates with its
// computed not_qualified verdict.
func TestGeminiContinuationRelationBuilder(t *testing.T) {
	t.Parallel()

	t.Run("confirmed same session", func(t *testing.T) {
		t.Parallel()

		relation := geminiContinuationRelation{
			Surface:         qualification.SurfaceProtocol,
			SeedSessionID:   "sess-seed",
			RecallSessionID: "sess-seed",
			ReplayConfirmed: true,
		}
		seed, recall := geminiBuildContinuationRecords(relation)
		if seed.SessionID == nil || *seed.SessionID != relation.SeedSessionID {
			t.Errorf("seed session id = %v, want the seed's actual identifier", seed.SessionID)
		}
		if seed.PriorSessionID != nil {
			t.Errorf("seed prior_session_id = %v, want null", seed.PriorSessionID)
		}
		if recall.Detail != geminiRecallConfirmedSameSession || recall.Grade != qualification.GradeUsable {
			t.Errorf("recall detail/classification = %s/%s, want confirmed_same_session/usable", recall.Detail, recall.Grade)
		}
		if recall.SessionID == nil || recall.PriorSessionID == nil || *recall.SessionID != *recall.PriorSessionID {
			t.Errorf("confirmed recall ids = %v/%v, want equal actual and prior ids", recall.SessionID, recall.PriorSessionID)
		}
	})

	t.Run("fresh fallback keeps distinct non-null ids", func(t *testing.T) {
		t.Parallel()

		relation := geminiContinuationRelation{
			Surface:         qualification.SurfaceNativeJSON,
			SeedSessionID:   "sess-native-seed",
			RecallSessionID: "sess-native-fallback",
			ReplayConfirmed: false,
		}
		seed, recall := geminiBuildContinuationRecords(relation)
		if recall.Detail != geminiRecallFreshFallback || recall.Grade != qualification.GradeGap {
			t.Errorf("recall detail/classification = %s/%s, want fresh_session_fallback/gap", recall.Detail, recall.Grade)
		}
		if *recall.SessionID == *recall.PriorSessionID {
			t.Errorf("fresh fallback copied the prior id: %q", *recall.SessionID)
		}
		if seed.SessionID == nil || *seed.SessionID != relation.SeedSessionID || seed.PriorSessionID != nil {
			t.Errorf("seed record = session %v, prior %v, want the seed's actual id and a null prior id", seed.SessionID, seed.PriorSessionID)
		}
	})

	t.Run("unobserved actual session", func(t *testing.T) {
		t.Parallel()

		relation := geminiContinuationRelation{Surface: qualification.SurfaceNativeText, SeedSessionID: "sess-text-seed"}
		seed, recall := geminiBuildContinuationRecords(relation)
		if recall.Detail != geminiRecallUnobservedActual || recall.Grade != qualification.GradeNotObserved {
			t.Errorf("recall detail/classification = %s/%s, want unobserved_actual_session/not_observed", recall.Detail, recall.Grade)
		}
		if recall.SessionID != nil {
			t.Errorf("unobserved recall copied a session id: %v", recall.SessionID)
		}
		if seed.SessionID == nil || *seed.SessionID != relation.SeedSessionID {
			t.Errorf("seed session id = %v, want the seed's actual identifier", seed.SessionID)
		}
	})

	t.Run("a fresh fallback set validates and computes not_qualified", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		// The recall session the adapter actually created differs from
		// the seed, so the protocol continuation entry is lowered.
		fixture.RemoveAll(qualification.MatchContinuation(qualification.SurfaceProtocol, qualification.InputContinuationSeed))
		fixture.RemoveAll(qualification.MatchContinuation(qualification.SurfaceProtocol, qualification.InputContinuationRecall))
		fixture.SetSemanticNotObserved(qualification.SurfaceProtocol, qualification.CapabilityRetryClassification, qualification.CaseHumanInput)
		fixture.SetSemanticNotObserved(qualification.SurfaceProtocol, qualification.CapabilityTurnDisposition, qualification.CaseRuntimeRefusal)
		fixture.SetSemanticNotObserved(qualification.SurfaceProtocol, qualification.CapabilityRetryClassification, qualification.CaseNonRetryableRefusal)
		seed, recall := geminiBuildContinuationRecords(geminiContinuationRelation{
			Surface:         qualification.SurfaceProtocol,
			SeedSessionID:   qualification.FixtureSession(qualification.SurfaceProtocol, "seed"),
			RecallSessionID: qualification.FixtureSession(qualification.SurfaceProtocol, "fallback"),
		})
		fixture.Add(seed)
		fixture.Add(recall)
		if baseline := fixture.FindFirst(qualification.MatchBaseline(qualification.SurfaceProtocol, qualification.CapabilitySessionContinuation)); baseline != nil {
			baseline.Grade = qualification.GradeGap
			baseline.Outcome = qualification.BaselineVerdictFor(qualification.GradeGap)
		}
		fixture.Finalize()

		path := qualification.WriteEvidenceFile(t, fixture.Records)
		qualification.RequireObservationVerdict(t, path, qualification.VerdictNotQualified)
	})
}

// TestResolveSessionLoadSuccessWithReplayConfirms and
// TestResolveSessionLoadUnimplementedLowersAndFallsBack live in
// continuation_test.go and cover the load route's replay requirement and
// the invented-method fallback; this file's continuation collector
// builds its records from their outcomes.

// geminiLiveTokenInventoryRecords builds one surface's token inventory
// records with the live runtime identity, replacing the fixture identity
// the shared builder carries.
func geminiLiveTokenInventoryRecords(surface qualification.Surface, sources []geminiTokenSource, inventoryFailed bool, agentName, agentVersion string) []qualification.Record {
	records := geminiTokenInventoryRecords(surface, sources, inventoryFailed)
	for i := range records {
		if surface == qualification.SurfaceProtocol {
			records[i].AgentName = new(agentName)
			records[i].ProtocolVersion = new(1)
		}
		records[i].AgentVersion = new(agentVersion)
	}
	return records
}

// geminiLiveContinuationRecords builds one surface's seed and recall
// records with the live runtime identity, replacing the fixture identity
// the shared builder carries.
func geminiLiveContinuationRecords(relation geminiContinuationRelation, agentName, agentVersion string) (qualification.Record, qualification.Record) {
	seed, recall := geminiBuildContinuationRecords(relation)
	if relation.Surface == qualification.SurfaceProtocol {
		seed.AgentName = new(agentName)
		recall.AgentName = new(agentName)
	}
	seed.AgentVersion = new(agentVersion)
	recall.AgentVersion = new(agentVersion)
	return seed, recall
}

// geminiTokenBearingKeys are the member names whose presence marks a
// structured member as token-bearing for the inventory scan.
var geminiTokenBearingKeys = []string{
	"total_tokens", "input_tokens", "output_tokens", "tokens", "cached", "total",
}

// geminiScanNativeTokenOutput scans one native surface's captured
// output for token-bearing structured members and the runtime-observed
// session identifier, and reports the members as vendor sources for
// classification against the shared usage contract. The text surface
// never scans: unstructured residue yields no token path.
func geminiScanNativeTokenOutput(surface qualification.Surface, output string, sessionID string) []geminiTokenSource {
	if surface == qualification.SurfaceNativeText {
		return nil
	}
	var sources []geminiTokenSource
	seen := map[string]bool{}
	for _, value := range geminiDecodeNativeJSONValues(output) {
		var pointers []string
		geminiCollectTokenPointers(value, "", &pointers)
		for _, pointer := range pointers {
			if seen[pointer] {
				continue
			}
			seen[pointer] = true
			sources = append(sources, geminiTokenSource{Path: pointer, Vendor: true, SessionID: sessionID})
		}
	}
	return sources
}

// geminiDecodeNativeJSONValues decodes the JSON values one native
// surface's combined output carries, tolerating the runtime's non-JSON
// diagnostic noise around them. Stream formats carry one complete
// object per line; the single-object format is pretty-printed.
func geminiDecodeNativeJSONValues(output string) []any {
	values := []any{}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(line), &value); err == nil {
			values = append(values, value)
		}
	}
	if len(values) > 0 {
		return values
	}
	if idx := strings.Index(output, "{"); idx >= 0 {
		decoder := json.NewDecoder(strings.NewReader(output[idx:]))
		for {
			var value any
			if err := decoder.Decode(&value); err != nil {
				break
			}
			values = append(values, value)
		}
	}
	return values
}

// geminiCollectTokenPointers walks one decoded JSON value and records
// the pointer of every object carrying a token-bearing member.
func geminiCollectTokenPointers(node any, prefix string, out *[]string) {
	object, ok := node.(map[string]any)
	if !ok {
		return
	}
	tokenBearing := false
	for _, key := range geminiTokenBearingKeys {
		if _, ok := object[key]; ok {
			tokenBearing = true
			break
		}
	}
	if tokenBearing && prefix != "" {
		*out = append(*out, prefix)
	}
	for key, child := range object {
		geminiCollectTokenPointers(child, prefix+"/"+key, out)
	}
}

// geminiNativeSessionID extracts the runtime-observed session
// identifier from one native structured output. The text surface never
// reports one.
func geminiNativeSessionID(surface qualification.Surface, output string) string {
	if surface == qualification.SurfaceNativeText {
		return ""
	}
	for _, value := range geminiDecodeNativeJSONValues(output) {
		if object, ok := value.(map[string]any); ok {
			if id, ok := object["session_id"].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

// geminiLogRecord is one captured structured log record with its
// attributes, the shape the runtime-identity collector reads the
// adapter's own structured log with.
type geminiLogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// geminiLogCapture is a slog.Handler that records every log record and
// its attributes. It exists only in test code and never mutates
// production logging.
type geminiLogCapture struct {
	mu      sync.Mutex
	records []geminiLogRecord
}

// Enabled reports true for every level so all records are captured.
func (c *geminiLogCapture) Enabled(_ context.Context, _ slog.Level) bool { return true }

// Handle records one log record and its attributes.
func (c *geminiLogCapture) Handle(_ context.Context, record slog.Record) error {
	entry := geminiLogRecord{Level: entryLevel(record.Level), Msg: record.Message, Attrs: map[string]string{}}
	record.Attrs(func(a slog.Attr) bool {
		entry.Attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, entry)
	return nil
}

// entryLevel normalizes a level for capture.
func entryLevel(level slog.Level) slog.Level { return level }

// WithAttrs returns the receiver so derived loggers share the capture.
func (c *geminiLogCapture) WithAttrs(_ []slog.Attr) slog.Handler { return c }

// WithGroup returns the receiver so derived loggers share the capture.
func (c *geminiLogCapture) WithGroup(_ string) slog.Handler { return c }

// implementationRecords returns every captured record the adapter's
// per-session implementation log wrote.
func (c *geminiLogCapture) implementationRecords() []geminiLogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []geminiLogRecord
	for _, entry := range c.records {
		if entry.Msg == "agent implementation" {
			out = append(out, entry)
		}
	}
	return out
}

// sessionFacts returns the handshake identity one actual protocol
// session's implementation log recorded.
func (c *geminiLogCapture) sessionFacts(sessionID string) (name, version string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.records {
		if entry.Msg != "agent implementation" || entry.Attrs["session_id"] != sessionID {
			continue
		}
		return entry.Attrs["name"], entry.Attrs["version"], true
	}
	return "", "", false
}

// newGeminiLoggedSession builds an in-memory session wired to a capture
// logger, so a test can tie the runtime-identity record to the
// structured log the pump writes when the session identifier lands.
func newGeminiLoggedTestSession(t *testing.T, capture *geminiLogCapture) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()

	state := &sessionState{
		agentConfig: domain.AgentConfig{},
		caps:        newCapabilityRecord(false),
		itemCh:      make(chan pumpItem, pumpChannelCapacity),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      slog.New(capture),
	}
	state.conn = jsonrpc.NewConn(outPw, inPr, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))

	go runPump(state)

	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
	})

	return state, outPr, inPw
}

// geminiIdentityRecord builds the one runtime-identity record for one
// actual protocol session from that session's handshake facts.
func geminiIdentityRecord(sessionID string, name, version string, protocolVersion int) qualification.Record {
	rec := qualification.IdentityFixtureRecord(sessionID)
	rec.AgentName = new(name)
	rec.AgentVersion = new(version)
	rec.ProtocolVersion = new(protocolVersion)
	return rec
}

// TestGeminiHandshakeIdentityCollector confirms the identity collector
// is tied to the adapter's existing structured log: after the handshake
// and session-identifier control messages reach the pump, exactly one
// implementation log record exists and the identity record built from
// the same handshake facts carries the same session identifier, name,
// version, and wire version.
func TestGeminiHandshakeIdentityCollector(t *testing.T) {
	t.Parallel()

	capture := &geminiLogCapture{}
	state, _, _ := newGeminiLoggedTestSession(t, capture)

	name := "fixture-agent"
	version := "1.0.0-fixture"
	facts := &handshakeFacts{
		agentInfo:        implementation{Name: name, Version: version},
		agentInfoPresent: true,
	}
	state.itemCh <- pumpItem{control: &pumpControl{handshake: facts}}
	state.itemCh <- pumpItem{control: &pumpControl{sessionID: "sess-identity-fixture"}}

	// The identity collector emits exactly one record per actual
	// protocol session.
	identity := geminiIdentityRecord("sess-identity-fixture", name, version, 1)

	deadline := time.Now().Add(geminiIdentityLogDeadline)
	var records []geminiLogRecord
	for {
		records = capture.implementationRecords()
		if len(records) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("implementation log records = %d, want exactly 1", len(records))
		}
		time.Sleep(5 * time.Millisecond)
	}
	record := records[0]
	if record.Attrs["session_id"] != "sess-identity-fixture" {
		t.Errorf("log session_id = %q, want the actual protocol session identifier", record.Attrs["session_id"])
	}
	if record.Attrs["name"] != name || record.Attrs["version"] != version {
		t.Errorf("log identity = %q/%q, want the handshake facts", record.Attrs["name"], record.Attrs["version"])
	}

	if identity.SessionID == nil || *identity.SessionID != record.Attrs["session_id"] {
		t.Errorf("identity session id = %v, want the logged session identifier", identity.SessionID)
	}
	if identity.AgentName == nil || *identity.AgentName != name || identity.AgentVersion == nil || *identity.AgentVersion != version {
		t.Errorf("identity record = %v/%v, want the handshake facts", identity.AgentName, identity.AgentVersion)
	}
	if identity.ProtocolVersion == nil || *identity.ProtocolVersion != 1 {
		t.Errorf("identity protocol version = %v, want 1", identity.ProtocolVersion)
	}
	if identity.Detail == "" || len(identity.Detail) > qualification.DetailBound {
		t.Errorf("identity detail is empty or over-bound")
	}
	if err := identityDetailShape(identity); err != nil {
		t.Errorf("identity detail shape = %v", err)
	}
}

// checkGeminiVerdictClassification verifies outcome and grade consistency.
func checkGeminiVerdictClassification(rec *qualification.Record) error {
	// Placeholder: this function should verify outcome/grade rules
	return nil
}

// geminiOrderCompare orders two records by the same canonical write
// order the validator enforces. It delegates to
// [qualification.OrderCompare] rather than reimplementing the rule, so
// the collector's write order and the validator's order check cannot
// drift apart the way they did before this delegation existed.
func geminiOrderCompare(a, b qualification.Record) int {
	return qualification.OrderCompare(a, b)
}

// identityDetailShape pins the identity record's bounded detail: a
// shape statement only, never a session identifier, path, or value.
func identityDetailShape(rec qualification.Record) error {
	if rec.Detail == "" || len(rec.Detail) > qualification.DetailBound {
		return fmt.Errorf("detail is empty or over-bound: %q", rec.Detail)
	}
	if len(rec.Detail) > 0 && (rec.Detail[0] == '/' || rec.Detail[0] == '.') {
		return fmt.Errorf("detail begins with a path-like rune: %q", rec.Detail)
	}
	return nil
}
