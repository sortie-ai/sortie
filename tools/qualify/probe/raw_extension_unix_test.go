//go:build unix

package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

func TestProtocolTokenInventoryNeverProvesAZeroFromAnUnreadTransport(t *testing.T) {
	t.Parallel()

	fixture := &sharedFixture{usage: &usageTracker{}, wireTraceDir: t.TempDir()}
	fixture.usage.observe("a-session", false)
	fixture.usage.observe("a-session", false)

	_, paths, inventory, _ := protocolTokenInventory(fixture)

	if len(paths) != 0 {
		t.Errorf("protocolTokenInventory(...) resolved %d token path(s), want none: nothing was read", len(paths))
	}
	if inventory.obs.Grade != evidence.GradeNotObserved {
		t.Errorf("protocolTokenInventory(...).Grade = %s, want %s: an unread transport is an unknown, never a measured absence",
			inventory.obs.Grade, evidence.GradeNotObserved)
	}
}

// Verbatim prompt results a live capture of this runtime carried, reproduced as
// fixtures so the classifier runs against a shape a paid run actually produced.
const (
	capturedResultWithExtension = `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","_meta":{"quota":{` +
		`"token_count":{"input_tokens":11835,"output_tokens":1},` +
		`"model_usage":[{"model":"gemini-3.5-flash","token_count":{"input_tokens":11835,"output_tokens":1}}]}}}}`

	capturedResultWithZeroExtension = `{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn","_meta":{"quota":{` +
		`"token_count":{"input_tokens":0,"output_tokens":0},"model_usage":[]}}}}`

	capturedCancelledResult = `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"cancelled"}}`

	capturedSessionNewResult = `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"a-captured-session"}}`

	capturedTransportError = `{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"bad key"}}`

	// No runtime measured so far reports every counter a budget needs over this
	// transport; this stands in for one that would, exercising the admitted arm.
	capturedResultWithCompleteExtension = `{"jsonrpc":"2.0","id":6,"result":{"stopReason":"end_turn","_meta":{"quota":{` +
		`"input_tokens":100,"output_tokens":10,"cached_content_token_count":8,"thought_tokens":4,"tool_tokens":2}}}}`
)

func completeExtensionTrace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeWireTrace(t, dir, capturedSessionNewResult, capturedResultWithCompleteExtension)
	return dir
}

func writeWireTrace(t *testing.T, dir string, lines ...string) {
	t.Helper()
	path := filepath.Join(dir, wireTraceFilePrefix+"1234.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write the transport capture %q: %v", path, err)
	}
}

func TestRawExtensionClassifierReachesEachVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lines   []string
		want    string
		results int
	}{
		{
			name:    "a result carrying the extension block",
			lines:   []string{capturedSessionNewResult, capturedResultWithExtension},
			want:    extensionSourcePresent,
			results: 1,
		},
		{
			name:    "a block reporting zeros is present, not missing",
			lines:   []string{capturedSessionNewResult, capturedResultWithZeroExtension},
			want:    extensionSourcePresent,
			results: 1,
		},
		{
			name:    "a result read that carried no such block",
			lines:   []string{capturedSessionNewResult, capturedCancelledResult},
			want:    extensionSourceAbsent,
			results: 1,
		},
		{
			name:    "an answer that carried no result at all",
			lines:   []string{capturedSessionNewResult, capturedTransportError},
			want:    extensionSourceNotObserved,
			results: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeWireTrace(t, dir, tt.lines...)

			reading := inventoryRawExtension(readProtocolWire(dir), false)
			if reading.verdict != tt.want {
				t.Errorf("inventoryRawExtension(...).verdict = %s, want %s", reading.verdict, tt.want)
			}
			if reading.results != tt.results {
				t.Errorf("inventoryRawExtension(...).results = %d, want %d", reading.results, tt.results)
			}
		})
	}
}

func TestRawExtensionClassifierReadsNothingFromNoCapture(t *testing.T) {
	t.Parallel()

	reading := inventoryRawExtension(readProtocolWire(t.TempDir()), false)
	if reading.verdict != extensionSourceNotObserved {
		t.Errorf("inventoryRawExtension(no capture).verdict = %s, want %s", reading.verdict, extensionSourceNotObserved)
	}
}

func TestRawExtensionPresenceIsNotAdmission(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeWireTrace(t, dir, capturedSessionNewResult, capturedResultWithExtension)

	reading := inventoryRawExtension(readProtocolWire(dir), false)
	if reading.verdict != extensionSourcePresent {
		t.Fatalf("inventoryRawExtension(...).verdict = %s, want %s", reading.verdict, extensionSourcePresent)
	}
	if reading.admitted() {
		t.Error("inventoryRawExtension(...).admitted() = true, want false: the block carries neither cache reads nor reasoning tokens")
	}
	for _, kind := range []string{"cache_read", "reasoning", "tool"} {
		if !slices.Contains(reading.missing, kind) {
			t.Errorf("inventoryRawExtension(...).missing = %v, want it to name %s", reading.missing, kind)
		}
	}
	for _, kind := range []string{"input", "output"} {
		if !slices.Contains(reading.covered, kind) {
			t.Errorf("inventoryRawExtension(...).covered = %v, want it to name %s", reading.covered, kind)
		}
	}
}

func TestRawExtensionAccountKeepsTheFourAnswersApart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeWireTrace(t, dir, capturedSessionNewResult, capturedResultWithExtension)

	account := inventoryRawExtension(readProtocolWire(dir), false).account()
	for _, want := range []string{extensionSourcePresent, "carries", "not admitted", "adapter"} {
		if !strings.Contains(account, want) {
			t.Errorf("account() = %q, want it to state %q", account, want)
		}
	}
	if len([]rune(account)) > evidence.DetailBound {
		t.Errorf("account() is %d runes, want at most %d: a longer one is truncated in the published record", len([]rune(account)), evidence.DetailBound)
	}
}

func TestRawExtensionReportsAdapterUseSeparately(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeWireTrace(t, dir, capturedSessionNewResult, capturedResultWithExtension)
	traces := readProtocolWire(dir)

	unused := inventoryRawExtension(traces, false)
	used := inventoryRawExtension(traces, true)
	if unused.verdict != used.verdict {
		t.Errorf("the verdict changed with adapter use: %s against %s, want the wire reading to be independent of it", unused.verdict, used.verdict)
	}
	if unused.account() == used.account() {
		t.Errorf("account() = %q either way, want it to state whether the effective adapter consumed the source", used.account())
	}
}

// quotaReportingACPScenario answers the handshake and every prompt with the
// extension block a live capture of a real runtime carried.
const quotaReportingACPScenario = "quota-reporting-acp-agent"

type quotaReportingACPParams struct{}

func runQuotaReportingACPAgent(_ []string, _ quotaReportingACPParams) int {
	out := bufio.NewWriter(os.Stdout)
	respond := func(id json.RawMessage, payload string) int {
		if _, err := fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,%s}`+"\n", id, payload); err != nil {
			return 2
		}
		if err := out.Flush(); err != nil {
			return 2
		}
		return 0
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var header struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &header); err != nil || header.Method == "" || len(header.ID) == 0 {
			continue
		}
		var code int
		switch header.Method {
		case "initialize":
			code = respond(header.ID, `"result":{"protocolVersion":1,"agentCapabilities":{}}`)
		case "session/new":
			code = respond(header.ID, `"result":{"sessionId":"quota-session"}`)
		case "session/prompt":
			code = respond(header.ID, `"result":{"stopReason":"end_turn","_meta":{"quota":{"token_count":{"input_tokens":11835,"output_tokens":1}}}}`)
		default:
			code = respond(header.ID, `"error":{"code":-32601,"message":"method not found"}`)
		}
		if code != 0 {
			return code
		}
	}
	return 0
}

func init() {
	probeScenarios[quotaReportingACPScenario] = agenttest.Typed(runQuotaReportingACPAgent)
}

func TestProtocolLaunchCapturesTheRawTransport(t *testing.T) {
	dir := t.TempDir()
	traceDir := t.TempDir()
	script := agenttest.FakeRuntime(t, dir, "acp-agent", quotaReportingACPScenario, quotaReportingACPParams{})
	coords := envSurfaceCoordinates(script)

	fixture := &sharedFixture{
		workspaceRoot: t.TempDir(),
		tracker:       &groupTracker{},
		usage:         &usageTracker{},
		wireTraceDir:  traceDir,
		env:           launchEnvironment(coords),
		envWrapper:    writeLaunchEnvWrapper(t, dir, launchEnvNames(coords), traceDir),
	}
	adapter, session, err := startInductionSession(t, coords, []string{"--acp"}, fixture.newLaunchWorkspace(t), "")
	if err != nil {
		t.Fatalf("start the induction session: %v", err)
	}
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "report",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("run one turn through the capturing wrapper: %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(...) exit reason = %s, want %s: capturing the transport must not change what the launch does",
			result.ExitReason, domain.EventTurnCompleted)
	}
	fixture.usage.observe(session.ID, result.UsageMeasured)

	reading := inventoryRawExtension(readProtocolWire(traceDir), result.UsageMeasured)
	if reading.verdict != extensionSourcePresent {
		t.Errorf("inventoryRawExtension(a real capture).verdict = %s, want %s; account: %s", reading.verdict, extensionSourcePresent, reading.account())
	}
	if reading.sessionID != "quota-session" {
		t.Errorf("inventoryRawExtension(a real capture).sessionID = %q, want the session the runtime reported", reading.sessionID)
	}
	if result.UsageMeasured {
		t.Error("RunTurn(...).UsageMeasured = true, want false: the adapter emits no usage for this transport, which is exactly why the wire has to be read")
	}
	if reading.admitted() {
		t.Error("inventoryRawExtension(a real capture).admitted() = true, want false: this block carries neither cache reads nor reasoning tokens")
	}
}

func TestEachProtocolLaunchLeavesItsOwnCapture(t *testing.T) {
	dir := t.TempDir()
	traceDir := t.TempDir()
	script := agenttest.FakeRuntime(t, dir, "acp-agent", quotaReportingACPScenario, quotaReportingACPParams{})
	coords := envSurfaceCoordinates(script)

	fixture := &sharedFixture{
		workspaceRoot: t.TempDir(),
		tracker:       &groupTracker{},
		usage:         &usageTracker{},
		wireTraceDir:  traceDir,
		env:           launchEnvironment(coords),
		envWrapper:    writeLaunchEnvWrapper(t, dir, launchEnvNames(coords), traceDir),
	}
	for range 2 {
		adapter, session, err := startInductionSession(t, coords, []string{"--acp"}, fixture.newLaunchWorkspace(t), "")
		if err != nil {
			t.Fatalf("start the induction session: %v", err)
		}
		if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "report",
			OnEvent: func(domain.AgentEvent) {},
		}); err != nil {
			t.Fatalf("run one turn through the capturing wrapper: %v", err)
		}
	}

	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("read the trace directory %q: %v", traceDir, err)
	}
	captures := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), wireTraceFilePrefix) && strings.HasSuffix(entry.Name(), ".jsonl") {
			captures++
		}
	}
	if captures != 2 {
		t.Errorf("two launches left %d capture(s), want one each: %v", captures, entries)
	}
}
