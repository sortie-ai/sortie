package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func validWorkflow(intervalMS int) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: %d\n---\nDo the task for {{ .issue.title }}.\n", intervalMS)
}

func retentionWorkflow(days int) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: 5000\nworkspace:\n  retention_days: %d\n---\nDo the task for {{ .issue.title }}.\n", days)
}

func turnTimeoutWorkflow(ms int) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: 5000\nagent:\n  turn_timeout_ms: %d\n---\nDo the task for {{ .issue.title }}.\n", ms)
}

func ciWatchWindowWorkflow(ms int) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: 5000\nreactions:\n  ci_failure:\n    provider: github-actions\n    watch_window_ms: %d\n---\nDo the task for {{ .issue.title }}.\n", ms)
}

// The endpoint is spliced in unquoted so a numeric literal decodes as a YAML
// integer rather than a string.
func trackerEndpointWorkflow(endpoint string) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: 5000\ntracker:\n  kind: jira\n  endpoint: %s\n---\nDo the task for {{ .issue.title }}.\n", endpoint)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// On Windows the rename can fail with "Access is denied" while fsnotify
// holds a handle on the target, so it is retried with a brief back-off.
func writeWorkflow(t *testing.T, path string, content []byte) {
	t.Helper()
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".workflow.tmp")
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	var err error
	for range 5 {
		err = os.Rename(tmp, path)
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rename temp to target: %v", err)
}

func intervalIs(mgr *Manager, want int) func() error {
	return func() error {
		if got := mgr.Config().Polling.IntervalMS; got != want {
			return fmt.Errorf("Polling.IntervalMS = %d, want %d", got, want)
		}
		return nil
	}
}

func loadErrorSet(mgr *Manager) func() error {
	return func() error {
		if mgr.LastLoadError() == nil {
			return errors.New("LastLoadError is nil, want the reload failure")
		}
		return nil
	}
}

func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

func TestManager_FilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
	}{
		{name: "standard name", filename: "WORKFLOW.md"},
		{name: "custom prefix", filename: "backend.WORKFLOW.md"},
		{name: "all lowercase", filename: "workflow.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, tt.filename)
			if err := os.WriteFile(path, validWorkflow(5000), 0o644); err != nil {
				t.Fatalf("write workflow file: %v", err)
			}
			m, err := NewManager(path, testLogger())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			m.Stop()

			if got := m.FilePath(); got != tt.filename {
				t.Errorf("FilePath() = %q, want %q", got, tt.filename)
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	t.Parallel()

	t.Run("ValidFile", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, validWorkflow(5000))

		mgr, err := NewManager(path, testLogger())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if mgr.Config().Polling.IntervalMS != 5000 {
			t.Errorf("Polling.IntervalMS = %d, want 5000", mgr.Config().Polling.IntervalMS)
		}
		if mgr.PromptTemplate() == nil {
			t.Error("PromptTemplate() is nil, want non-nil")
		}
		if mgr.LastLoadError() != nil {
			t.Errorf("LastLoadError() = %v, want nil", mgr.LastLoadError())
		}
	})

	t.Run("MissingFile", func(t *testing.T) {
		t.Parallel()

		_, err := NewManager(filepath.Join(t.TempDir(), "nonexistent.md"), testLogger())
		if err == nil {
			t.Fatal("NewManager() error = nil, want error")
		}
	})

	t.Run("InvalidYAML", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, []byte("---\n: : : bad {{{\n---\nprompt\n"))

		_, err := NewManager(path, testLogger())
		if err == nil {
			t.Fatal("NewManager() error = nil, want error")
		}
	})

	t.Run("InvalidTemplate", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "WORKFLOW.md")
		mustWriteFile(t, path, []byte("---\nk: v\n---\n{{ .issue.title\n"))

		_, err := NewManager(path, testLogger())
		if err == nil {
			t.Fatal("NewManager() error = nil, want error")
		}
	})
}

func TestManager_Reload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mustWriteFile(t, path, validWorkflow(9999))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if mgr.Config().Polling.IntervalMS != 9999 {
		t.Errorf("after Reload: Polling.IntervalMS = %d, want 9999", mgr.Config().Polling.IntervalMS)
	}
	if mgr.LastLoadError() != nil {
		t.Errorf("after successful Reload: LastLoadError = %v, want nil", mgr.LastLoadError())
	}
}

func TestManager_LoadNotifierSeesOutcome(t *testing.T) {
	t.Parallel()

	type seen struct {
		intervalMS int
		failures   int
	}
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))
	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var notified []seen
	mgr.SetLoadNotifier(func() {
		notified = append(notified, seen{mgr.Config().Polling.IntervalMS, mgr.LoadStatus().ConsecutiveFailures})
	})

	mustWriteFile(t, path, validWorkflow(9999))
	mgr.reload()
	mustWriteFile(t, path, []byte("---\n[[[invalid\n---\nprompt\n"))
	mgr.reload()
	mustWriteFile(t, path, validWorkflow(7777))
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	want := []seen{{intervalMS: 9999}, {intervalMS: 9999, failures: 1}}
	if !slices.Equal(notified, want) {
		t.Errorf("notifier saw (interval_ms, failures) %v after a valid edit, an invalid edit and a Reload, want %v", notified, want)
	}
}

const partialsWorkflow = "---\npolling:\n  interval_ms: 5000\nagent:\n  kind: mock\ndispatch:\n  partials: [\"*.part\"]\n  rules:\n    - name: bug\n      match:\n        labels: [bug]\n      template: rule.tmpl\n---\n{{ template \"greet\" . }}\n"

func TestManager_ReloadFailSafe(t *testing.T) {
	t.Parallel()

	greet := func(body string) string { return "{{ define \"greet\" }}\n" + body + "\n{{ end }}\n" }
	tests := []struct {
		name, edit, wantField string
		before, after         []byte
		remove                bool
	}{
		{name: "invalid YAML", before: validWorkflow(5000), after: []byte("---\n[[[invalid\n---\nprompt\n")},
		{name: "mistyped tracker endpoint", before: trackerEndpointWorkflow("https://jira.example.com"), after: trackerEndpointWorkflow("123"), wantField: "tracker.endpoint"},
		{name: "rule block for another kind than the rule runs", before: ruleBlockWorkflow("kind-a", "kind-a"), after: ruleBlockWorkflow("kind-a", "kind-b"), wantField: "dispatch.rules[0].kind-b"},
		{name: "default kind changed under a rule block", before: ruleBlockWorkflow("kind-a", "kind-a"), after: ruleBlockWorkflow("kind-b", "kind-a"), wantField: "dispatch.rules[0].kind-a"},
		{name: "retention days in the rejected range", before: retentionWorkflow(30), after: retentionWorkflow(5)},
		{name: "zero turn timeout", before: turnTimeoutWorkflow(1800000), after: turnTimeoutWorkflow(0)},
		{name: "watch window above the ceiling", before: ciWatchWindowWorkflow(3600000), after: ciWatchWindowWorkflow(9223372036855)},
		{name: "malformed dispatch glob", before: validWorkflow(5000), after: []byte("---\ndispatch:\n  rules:\n    - name: bad-glob\n      match:\n        labels: [\"[unclosed\"]\n      agent: mock\n---\nPrompt.\n")},
		{name: "partial syntax error", before: []byte(partialsWorkflow), edit: greet("{{ if }}")},
		{name: "partial calls an undefined name", before: []byte(partialsWorkflow), edit: greet(`{{ template "missing" . }}`)},
		{name: "deleted partial whose entry expands to nothing", before: []byte(partialsWorkflow), remove: true, wantField: "dispatch.partials[0]"},
		{name: "operator table file gone", before: priceTableWorkflow("prices.json"), after: priceTableWorkflow("gone.json"), wantField: "price_table.replace_file"},
		{name: "operator table file invalid", before: priceTableWorkflow("prices.json"), after: priceTableWorkflow("invalid.json"), wantField: "price_table.replace_file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
			}
			path, partial := filepath.Join(dir, "WORKFLOW.md"), filepath.Join(dir, "greet.part")
			mustWriteFile(t, path, tt.before)
			mustWriteFile(t, partial, []byte(greet("hello v1")))
			mustWriteFile(t, filepath.Join(dir, "rule.tmpl"), []byte(`{{ template "greet" . }}`))
			mustWriteFile(t, filepath.Join(dir, "prices.json"), []byte(operatorTableA))
			mustWriteFile(t, filepath.Join(dir, "invalid.json"), []byte("{}"))
			mgr, err := NewManager(path, testLogger())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			templates := func() []*prompt.Template {
				return []*prompt.Template{mgr.PromptTemplate(), mgr.PromptTemplateByID(""), mgr.PromptTemplateByID(filepath.Join(dir, "rule.tmpl"))}
			}
			wantConfig, wantTemplates, wantRates := mgr.Config(), templates(), mgr.Rates()
			if tt.after != nil {
				mustWriteFile(t, path, tt.after)
			}
			mustWriteFile(t, partial, []byte(tt.edit))
			if tt.remove {
				mustRemove(t, partial)
			}
			err = mgr.Reload()

			var ce *config.ConfigError
			if err == nil || !errors.Is(mgr.LastLoadError(), err) {
				t.Errorf("Reload() error = %v, LastLoadError() = %v, want the same non-nil error", err, mgr.LastLoadError())
			}
			if tt.wantField != "" && (!errors.As(err, &ce) || ce.Field != tt.wantField) {
				t.Errorf("Reload() error = %v, want a *config.ConfigError with Field %q", err, tt.wantField)
			}
			if !reflect.DeepEqual(mgr.Config(), wantConfig) || !slices.Equal(templates(), wantTemplates) || mgr.Rates() != wantRates {
				t.Error("Config(), a prompt template or Rates() changed after the failed reload, want the previous ones")
			}
			if tt.after == nil {
				mustWriteFile(t, partial, []byte(greet("hello v2")))
				reloadErr := mgr.Reload()
				got, renderErr := mgr.PromptTemplate().Render(map[string]any{}, 1, prompt.RunContext{})
				if reloadErr != nil || renderErr != nil || !strings.Contains(got, "hello v2") {
					t.Errorf("Reload() = %v, Render() = %q, %v after fixing the partial, want the fixed partial text", reloadErr, got, renderErr)
				}
			}
		})
	}
}

func priceTableWorkflow(replaceFile string) []byte {
	return fmt.Appendf(nil, "---\npolling:\n  interval_ms: 5000\nprice_table:\n  replace_file: %s\n---\nPrompt.\n", replaceFile)
}

const (
	operatorTableA  = `{"zz-model-a": {"input_cost_per_token": 0.000001}}`
	operatorTableAB = `{"zz-model-a": {"input_cost_per_token": 0.000001}, "zz-model-b": {"input_cost_per_token": 0.000001}}`
)

func TestManager_ReloadAppliesEditedOperatorTable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path, table := filepath.Join(dir, "WORKFLOW.md"), filepath.Join(dir, "prices.json")
	mustWriteFile(t, path, priceTableWorkflow("prices.json"))
	mustWriteFile(t, table, []byte(operatorTableA))
	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mustWriteFile(t, table, []byte(operatorTableAB))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() = %v, want nil for an edited valid operator table", err)
	}

	usage := domain.TokenUsage{InputTokens: 1_000_000, TotalTokens: 1_000_000}
	spend := pricing.Spend{Measured: true, Totals: usage, Parts: map[string]domain.TokenUsage{"zz-model-b": usage}}
	if _, got := pricing.PriceSession(spend, mgr.Rates()); got != pricing.CostEstimated {
		t.Errorf("PriceSession(a model the edited table lists) status = %q, want %q", got, pricing.CostEstimated)
	}
}

const repricedModel = "claude-sonnet-4-5-20250929"

func repricedTable(t *testing.T) []byte {
	t.Helper()

	embedded, err := pricing.EmbeddedTable()
	if err != nil {
		t.Fatalf("EmbeddedTable() = %v, want nil", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(embedded.Encode(), &doc); err != nil {
		t.Fatalf("decoding the embedded table: %v", err)
	}
	doc[repricedModel] = json.RawMessage(`{"litellm_provider": "anthropic", "input_cost_per_token": 0.00001, "output_cost_per_token": 0.00005}`)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding the repriced table: %v", err)
	}
	return body
}

type heldDownload struct {
	mgr     *Manager
	release chan struct{}
	landed  chan struct{}
}

func startHeldDownload(t *testing.T, body []byte) heldDownload {
	t.Helper()

	held := heldDownload{release: make(chan struct{}), landed: make(chan struct{}, 1)}
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-held.release:
			_, _ = w.Write(body)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, fmt.Appendf(nil, "---\nprice_table:\n  fetch_url: %s\n---\nPrompt.\n", srv.URL))
	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	downloader := pricing.NewDownloader(filepath.Join(dir, "sortie.db"), testLogger())
	mgr.SetDownloader(downloader)
	held.mgr = mgr

	var running sync.WaitGroup
	running.Go(func() {
		downloader.Run(t.Context(), func() {
			mgr.RefreshRates()
			held.landed <- struct{}{}
		})
	})
	t.Cleanup(running.Wait)
	<-entered
	return held
}

func priceRepricedModel(mgr *Manager) pricing.Price {
	usage := domain.TokenUsage{InputTokens: 1_000_000, TotalTokens: 1_000_000}
	return pricing.PriceRun(pricing.Spend{Measured: true, Totals: usage, Parts: map[string]domain.TokenUsage{repricedModel: usage}}, mgr.Rates())
}

func assertPricedFrom(t *testing.T, price pricing.Price, source pricing.PriceSource, version string) {
	t.Helper()

	if part := price.Parts[repricedModel]; price.Status != pricing.CostEstimated || part.Source != source || part.TableVersion != version {
		t.Errorf("PriceRun(%s) status, source, version = %q, %q, %q, want %q, %q, %q",
			repricedModel, price.Status, part.Source, part.TableVersion, pricing.CostEstimated, source, version)
	}
}

func TestManager_StalledDownloadBlocksNothing(t *testing.T) {
	t.Parallel()

	held := startHeldDownload(t, repricedTable(t))
	embedded, err := pricing.EmbeddedTable()
	if err != nil {
		t.Fatalf("EmbeddedTable() = %v, want nil", err)
	}

	if err := held.mgr.Reload(); err != nil {
		t.Fatalf("Reload() with a download in flight = %v, want nil", err)
	}
	held.mgr.RefreshRates()

	assertPricedFrom(t, priceRepricedModel(held.mgr), pricing.SourceEmbedded, embedded.Version())
}

func TestManager_LandedTableLeavesRecordedCostsAlone(t *testing.T) {
	t.Parallel()

	body := repricedTable(t)
	landedTable, err := pricing.ParseTable(body, nil)
	if err != nil {
		t.Fatalf("ParseTable(repriced table) = %v, want nil", err)
	}
	embedded, err := pricing.EmbeddedTable()
	if err != nil {
		t.Fatalf("EmbeddedTable() = %v, want nil", err)
	}
	held := startHeldDownload(t, body)
	recorded := priceRepricedModel(held.mgr)
	recordedCost := *recorded.CostUSD

	close(held.release)
	<-held.landed
	later := priceRepricedModel(held.mgr)

	assertPricedFrom(t, recorded, pricing.SourceEmbedded, embedded.Version())
	if *recorded.CostUSD != recordedCost {
		t.Errorf("recorded cost after the landing = %v, want %v", *recorded.CostUSD, recordedCost)
	}
	assertPricedFrom(t, later, pricing.SourceFetched, landedTable.Version())
	if math.Abs(*later.CostUSD-10) > 1e-6 {
		t.Errorf("PriceRun(%s) cost after the landing = %v, want 10", repricedModel, *later.CostUSD)
	}
}

func ruleBlockWorkflow(agentKind, ruleBlockKind string) []byte {
	return fmt.Appendf(nil, `---
polling:
  interval_ms: 5000
agent:
  kind: %s
dispatch:
  rules:
    - name: cheap
      match:
        labels: [cheap]
      %s:
        model: provider/cheap
---
Do the task for {{ .issue.title }}.
`, agentKind, ruleBlockKind)
}

// TestManager_WatchPicksUpChange verifies that workflow file changes are
// detected and trigger re-read/re-apply without restart.
func TestManager_WatchPicksUpChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	writeWorkflow(t, path, validWorkflow(10000))

	waittest.Until(t, intervalIs(mgr, 10000))
}

// TestManager_WatchInvalidRetainsGood verifies that an invalid workflow
// reload keeps the last known good effective configuration.
func TestManager_WatchInvalidRetainsGood(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	writeWorkflow(t, path, []byte("---\n[[[bad yaml\n---\nprompt\n"))

	waittest.Until(t, loadErrorSet(mgr))

	if mgr.Config().Polling.IntervalMS != 5000 {
		t.Errorf("after invalid reload: Polling.IntervalMS = %d, want 5000",
			mgr.Config().Polling.IntervalMS)
	}
}

func TestManager_ConcurrentReadSafety(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	// Readers spin until the reload is confirmed to have completed, ensuring
	// concurrent access actually overlaps with the write under -race.
	var reloaded atomic.Bool
	var wg sync.WaitGroup
	const readers = 10

	for range readers {
		wg.Go(func() {
			for !reloaded.Load() {
				_ = mgr.Config()
				_ = mgr.PromptTemplate()
				_ = mgr.LastLoadError()
			}
		})
	}

	writeWorkflow(t, path, validWorkflow(7777))

	waittest.Until(t, func() error {
		err := intervalIs(mgr, 7777)()
		if err == nil {
			reloaded.Store(true)
		}
		return err
	})
	wg.Wait()
}

func TestManager_DebounceCoalescence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(1000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	// Write 5 times in rapid succession.
	for i := range 5 {
		val := 2000 + i*1000 // 2000, 3000, 4000, 5000, 6000
		writeWorkflow(t, path, validWorkflow(val))
		time.Sleep(5 * time.Millisecond)
	}

	waittest.Until(t, intervalIs(mgr, 6000))
}

func TestManager_DeleteAndRecreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	// Simulate editor delete-and-recreate.
	mustRemove(t, path)
	time.Sleep(50 * time.Millisecond)
	writeWorkflow(t, path, validWorkflow(8888))

	waittest.Until(t, intervalIs(mgr, 8888))

	// Confirm watcher is still alive; write a third value.
	writeWorkflow(t, path, validWorkflow(9999))

	waittest.Until(t, intervalIs(mgr, 9999))
}

func TestManager_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	done := make(chan struct{})
	go func() {
		mgr.Stop()
		close(done)
	}()

	waittest.Receive(t, done, "Stop() return after context cancellation")
}

func TestManager_StopIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Double stop should not panic.
	mgr.Stop()
	mgr.Stop()
}

func TestManager_RecoverAfterInvalidReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	writeWorkflow(t, path, []byte("---\n[[[bad\n---\nprompt\n"))
	waittest.Until(t, loadErrorSet(mgr))

	if mgr.Config().Polling.IntervalMS != 5000 {
		t.Fatalf("after invalid reload: Polling.IntervalMS = %d, want 5000",
			mgr.Config().Polling.IntervalMS)
	}

	// Write valid content again; the watcher should recover.
	writeWorkflow(t, path, validWorkflow(7777))

	waittest.Until(t, intervalIs(mgr, 7777))
	if mgr.LastLoadError() != nil {
		t.Errorf("after recovery: LastLoadError = %v, want nil", mgr.LastLoadError())
	}
}

// workflowWithStates returns valid WORKFLOW.md content with the given
// active and terminal state lists. An empty slice results in the key being
// absent from the front matter.
func workflowWithStates(active, terminal []string) []byte {
	var s strings.Builder
	s.WriteString("---\npolling:\n  interval_ms: 5000\ntracker:\n")
	if len(active) > 0 {
		s.WriteString("  active_states:\n")
		for _, st := range active {
			fmt.Fprintf(&s, "    - %s\n", st)
		}
	}
	if len(terminal) > 0 {
		s.WriteString("  terminal_states:\n")
		for _, st := range terminal {
			fmt.Fprintf(&s, "    - %s\n", st)
		}
	}
	s.WriteString("---\nDo the task for {{ .issue.title }}.\n")
	return []byte(s.String())
}

// rejectBothEmpty is a ValidateFunc that rejects configs where both
// active_states and terminal_states are empty.
func rejectBothEmpty(cfg config.ServiceConfig) error {
	if len(cfg.Tracker.ActiveStates) == 0 && len(cfg.Tracker.TerminalStates) == 0 {
		return errors.New("both state lists empty")
	}
	return nil
}

func TestManager_ReloadValidatorRejects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with both state lists empty.
	mustWriteFile(t, path, workflowWithStates(nil, nil))

	err = mgr.Reload()
	if err == nil {
		t.Fatal("Reload() error = nil, want error from validator")
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 1 || got[0] != "To Do" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [To Do]", got)
	}
	if mgr.LastLoadError() == nil {
		t.Error("LastLoadError() = nil, want non-nil")
	}
}

func TestManager_ReloadValidatorAccepts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with different but valid state lists.
	mustWriteFile(t, path, workflowWithStates([]string{"In Progress"}, []string{"Closed"}))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 1 || got[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", got)
	}
	if mgr.LastLoadError() != nil {
		t.Errorf("LastLoadError() = %v, want nil", mgr.LastLoadError())
	}
}

func TestManager_ReloadWithoutValidatorPromotesBothEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Overwrite with both state lists empty; no validator so should promote.
	mustWriteFile(t, path, workflowWithStates(nil, nil))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 0 {
		t.Errorf("Config().Tracker.ActiveStates = %v, want empty", got)
	}
}

func TestNewManager_ValidatorRejectsInitialLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates(nil, nil))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err == nil {
		t.Fatal("NewManager() error = nil, want error from validator")
	}
	if mgr != nil {
		t.Errorf("NewManager() returned non-nil Manager on validation failure")
	}
}

func TestManager_ReloadEmptyActiveNonEmptyTerminalPromotes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Empty active_states but non-empty terminal_states; should pass.
	mustWriteFile(t, path, workflowWithStates(nil, []string{"Done"}))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.TerminalStates; len(got) != 1 || got[0] != "Done" {
		t.Errorf("Config().Tracker.TerminalStates = %v, want [Done]", got)
	}
}

func TestManager_ReloadNonEmptyActiveEmptyTerminalPromotes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, workflowWithStates([]string{"To Do"}, []string{"Done"}))

	mgr, err := NewManager(path, testLogger(), WithValidateFunc(rejectBothEmpty))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Non-empty active_states but empty terminal_states; should pass.
	mustWriteFile(t, path, workflowWithStates([]string{"In Progress"}, nil))

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload() unexpected error: %v", err)
	}
	if got := mgr.Config().Tracker.ActiveStates; len(got) != 1 || got[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", got)
	}
}

func TestManager_SetLogger(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	var bufA, bufB bytes.Buffer
	loggerA := slog.New(slog.NewTextHandler(&bufA, &slog.HandlerOptions{Level: slog.LevelDebug}))
	loggerB := slog.New(slog.NewTextHandler(&bufB, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mgr, err := NewManager(path, loggerA)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.SetLogger(loggerB)

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	writeWorkflow(t, path, []byte("---\n[[[invalid\n---\nprompt\n"))

	waittest.Until(t, loadErrorSet(mgr))

	// Stop before reading buffers to ensure the watcher goroutine has exited
	// and no concurrent writes remain.
	mgr.Stop()

	if !strings.Contains(bufB.String(), "workflow reload failed") {
		t.Errorf("loggerB output does not contain %q: %s", "workflow reload failed", bufB.String())
	}
	if strings.Contains(bufA.String(), "workflow reload failed") {
		t.Errorf("loggerA output unexpectedly contains %q: %s", "workflow reload failed", bufA.String())
	}
}

func TestManager_SetLoggerNil(t *testing.T) {
	// No t.Parallel; this test mutates the global slog.Default.
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	var explicitBuf, defaultBuf bytes.Buffer
	explicitLogger := slog.New(slog.NewTextHandler(&explicitBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defaultLogger := slog.New(slog.NewTextHandler(&defaultBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	prevDefault := slog.Default()
	slog.SetDefault(defaultLogger)
	defer slog.SetDefault(prevDefault)

	mgr, err := NewManager(path, explicitLogger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mgr.SetLogger(nil)

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	writeWorkflow(t, path, []byte("---\n[[[invalid\n---\nprompt\n"))

	waittest.Until(t, loadErrorSet(mgr))

	mgr.Stop()

	if !strings.Contains(defaultBuf.String(), "workflow reload failed") {
		t.Errorf("default logger output does not contain %q: %s", "workflow reload failed", defaultBuf.String())
	}
	if strings.Contains(explicitBuf.String(), "workflow reload failed") {
		t.Errorf("explicit logger output unexpectedly contains %q: %s", "workflow reload failed", explicitBuf.String())
	}
}

func TestManager_SetLoggerConcurrentWithReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			mgr.SetLogger(testLogger())
		}
	})

	for i := range 5 {
		writeWorkflow(t, path, validWorkflow(5000+i))
		waittest.Until(t, intervalIs(mgr, 5000+i))
	}

	stop.Store(true)
	wg.Wait()
	mgr.Stop()
}

// workflowWithDispatch returns WORKFLOW.md content with dispatch rules
// that reference template files that exist under dir.
func workflowWithDispatch(t *testing.T, dir string) []byte {
	t.Helper()
	tmplDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bugTmpl := filepath.Join(tmplDir, "bug.md")
	if err := os.WriteFile(bugTmpl, []byte("bug template {{ .issue.identifier }}"), 0o644); err != nil {
		t.Fatalf("write bug.md: %v", err)
	}
	return []byte(`---
polling:
  interval_ms: 5000
agent:
  kind: mock
dispatch:
  rules:
    - name: bug
      match:
        labels: ["bug"]
      template: prompts/bug.md
---
You are assigned to {{ .issue.identifier }}.
`)
}

func TestManager_PromptTemplateByID_EmptyIDReturnsBodyTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := mgr.PromptTemplateByID("")
	if got == nil {
		t.Fatal("PromptTemplateByID(\"\") = nil, want body template")
	}
	body := mgr.PromptTemplate()
	if got != body {
		t.Errorf("PromptTemplateByID(\"\") != PromptTemplate(): pointers differ")
	}
}

func TestManager_PromptTemplateByID_UnknownIDReturnsNil(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, validWorkflow(5000))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := mgr.PromptTemplateByID("/nonexistent/path.md")
	if got != nil {
		t.Errorf("PromptTemplateByID(\"/nonexistent/path.md\") = non-nil, want nil")
	}
}

func TestManager_PromptTemplateByID_KnownRuleTemplate(t *testing.T) {
	t.Parallel()

	// Canonicalize the temp dir via EvalSymlinks so the expected key
	// matches the resolved absolute path the manager registers (on
	// Windows, t.TempDir() may return an 8.3 short name that the
	// manager's symlink-evaluation step rewrites to the long form).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	content := workflowWithDispatch(t, dir)
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, content)

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// The template is registered under its resolved absolute path.
	bugTmplAbs := filepath.Join(dir, "prompts", "bug.md")

	got := mgr.PromptTemplateByID(bugTmplAbs)
	if got == nil {
		t.Fatalf("PromptTemplateByID(%q) = nil, want non-nil", bugTmplAbs)
	}
}

// TestManager_PromptTemplateByID_CanonicalKeyAcrossSymlinks locks the
// invariant that the per-rule template index is keyed by the canonical,
// EvalSymlinks-resolved absolute path, even when the workflow file is
// reached through a symlinked workflow directory. Windows CI runners
// surfaced this contract via 8.3 short-name paths (RUNNER~1 -> long
// form); creating an explicit symlink reproduces the same canonical-
// vs-raw drift on Linux and macOS so the regression is caught on every
// supported runner. Skips gracefully on hosts where os.Symlink is
// unsupported or forbidden.
func TestManager_PromptTemplateByID_CanonicalKeyAcrossSymlinks(t *testing.T) {
	t.Parallel()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()) error = %v, want nil", err)
	}
	canonical := filepath.Join(base, "real")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", canonical, err)
	}

	via := filepath.Join(base, "via")
	if err := os.Symlink(canonical, via); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("os.Symlink(%q -> %q) unsupported on this filesystem: %v", canonical, via, err)
		}
		t.Skipf("os.Symlink(%q -> %q) = %v (skipping symlink-based assertion)", canonical, via, err)
	}

	// Write the workflow and its template under the canonical
	// directory so the on-disk layout is independent of which path
	// (canonical or symlinked) the manager is asked to load from.
	content := workflowWithDispatch(t, canonical)
	mustWriteFile(t, filepath.Join(canonical, "WORKFLOW.md"), content)

	mgr, err := NewManager(filepath.Join(via, "WORKFLOW.md"), testLogger())
	if err != nil {
		t.Fatalf("NewManager(via=%q) error = %v, want nil", via, err)
	}

	wantKey := filepath.Join(canonical, "prompts", "bug.md")
	if got := mgr.PromptTemplateByID(wantKey); got == nil {
		t.Fatalf("PromptTemplateByID(canonical=%q) = nil, want non-nil", wantKey)
	}
}

func TestManager_WithAgentKindProbe_AcceptsKnownKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	// Use a workflow with agent kind "mock"; probe accepts it.
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 5000
agent:
  kind: mock
dispatch:
  rules:
    - name: test
      agent: mock
---
Prompt.
`))

	probe := func(kind string) bool { return kind == "mock" }
	mgr, err := NewManager(path, testLogger(), WithAgentKindProbe(probe))
	if err != nil {
		t.Fatalf("NewManager with probe: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewManager returned nil manager")
	}
}

func TestManager_WithAgentKindProbe_RejectsUnknownKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	// Dispatch rule references "nonexistent-agent".
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 5000
agent:
  kind: mock
dispatch:
  rules:
    - name: bad
      agent: nonexistent-agent
---
Prompt.
`))

	probe := func(kind string) bool { return kind == "mock" }
	_, err := NewManager(path, testLogger(), WithAgentKindProbe(probe))
	if err == nil {
		t.Fatal("NewManager with rejecting probe = nil error, want error")
	}
}

func TestManager_PerRuleTemplate_FrontMatterRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write a per-rule template that has front matter; must be rejected.
	tmplDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tmplPath := filepath.Join(tmplDir, "with_fm.md")
	if err := os.WriteFile(tmplPath, []byte("---\nkey: value\n---\nTemplate body."), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, []byte(`---
polling:
  interval_ms: 5000
dispatch:
  rules:
    - name: with-fm
      match:
        labels: ["test"]
      template: prompts/with_fm.md
---
Prompt.
`))

	_, err := NewManager(path, testLogger())
	if err == nil {
		t.Fatal("NewManager with front-matter template = nil error, want error")
	}

	errMsg := err.Error()
	if !bytes.Contains([]byte(errMsg), []byte("front matter")) {
		t.Errorf("error = %q, want to contain %q", errMsg, "front matter")
	}
}

func retiredFixtureLookup(kind string) (registry.RetiredAgent, bool) {
	if kind != "legacy" {
		return registry.RetiredAgent{}, false
	}
	return registry.RetiredAgent{
		Replacement:   "modern",
		CredentialEnv: registry.DeclareCredentialEnv(),
		Convert: func(in registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
			if _, refused := in.Settings["bad"]; refused {
				return registry.AgentConversion{}, &registry.AgentConversionFault{Key: "bad", Message: "bad is refused"}
			}
			return registry.AgentConversion{Command: domain.AgentCommand{Line: strings.TrimSpace(in.Command.Line + " serve")}}, nil
		},
	}, true
}

func modernOnly(kind string) bool { return kind == "modern" }

func retiredWorkflow(agentCommand, legacyBlock string) []byte {
	return fmt.Appendf(nil, `---
polling:
  interval_ms: 5000
agent:
  kind: legacy
  command: %s
legacy:
%s
dispatch:
  rules:
    - name: backend
      match:
        labels: ["backend"]
      agent: legacy
---
Prompt.
`, agentCommand, legacyBlock)
}

func TestManager_WithRetiredAgents_ConvertsAtStartup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	content := retiredWorkflow("legacy-wrapper", "  model: m1")
	mustWriteFile(t, path, content)

	mgr, err := NewManager(path, testLogger(), WithRetiredAgents(retiredFixtureLookup), WithAgentKindProbe(modernOnly))
	if err != nil {
		t.Fatalf("NewManager() error = %v, want the converted kinds to pass the probe", err)
	}

	cfg := mgr.Config()
	if cfg.Agent.Kind != "modern" {
		t.Errorf("Config().Agent.Kind = %q, want %q", cfg.Agent.Kind, "modern")
	}
	if got := cfg.Dispatch.Rules[0].Selection.AgentKind; got != "modern" {
		t.Errorf("Config().Dispatch.Rules[0].Selection.AgentKind = %q, want %q", got, "modern")
	}
	if got := cfg.AgentCommand("modern", false); got.Line != "legacy-wrapper serve" || got.Argv != nil {
		t.Errorf("Config().AgentCommand(%q, false) = %+v, want the string %q", "modern", got, "legacy-wrapper serve")
	}
	if records := cfg.AgentKindConversions(); len(records) != 1 || records[0].Kind != "legacy" {
		t.Errorf("Config().AgentKindConversions() = %+v, want one record for %q", records, "legacy")
	}
	var checks []string
	for _, advisory := range cfg.Advisories() {
		checks = append(checks, advisory.Check)
	}
	if len(checks) != 1 || checks[0] != "agent.kind.retired" {
		t.Errorf("Config().Advisories() checks = %v, want [agent.kind.retired]", checks)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(written, content) {
		t.Errorf("workflow file after load = %q, want it untouched", written)
	}
}

func TestManager_WithoutRetiredAgents_ConvertsNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	mustWriteFile(t, path, retiredWorkflow("legacy-wrapper", "  model: m1"))

	mgr, err := NewManager(path, testLogger(), WithAgentKindProbe(func(kind string) bool { return kind == "legacy" }))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	cfg := mgr.Config()
	if cfg.Agent.Kind != "legacy" {
		t.Errorf("Config().Agent.Kind = %q, want %q", cfg.Agent.Kind, "legacy")
	}
	if records := cfg.AgentKindConversions(); len(records) != 0 {
		t.Errorf("Config().AgentKindConversions() = %+v, want none", records)
	}
}

func TestNewManager_WithRetiredAgents_FaultFailsStartup(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	mustWriteFile(t, path, retiredWorkflow("legacy-wrapper", "  bad: true"))

	_, err := NewManager(path, testLogger(), WithRetiredAgents(retiredFixtureLookup))

	var ce *config.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("NewManager() error = %v (%T), want *config.ConfigError", err, err)
	}
	if ce.Field != "legacy.bad" {
		t.Errorf("ConfigError.Field = %q, want %q", ce.Field, "legacy.bad")
	}
}

func TestManager_ReloadRetainsOnConversionFaultAndMalformedList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   []byte
		wantField string
	}{
		{"conversion fault", retiredWorkflow("legacy-wrapper", "  bad: true"), "legacy.bad"},
		{"empty command list", retiredWorkflow("[]", "  model: m1"), "agent.command"},
		{"empty element in the command list", retiredWorkflow(`["legacy-wrapper", ""]`, "  model: m1"), "agent.command[1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "WORKFLOW.md")
			mustWriteFile(t, path, retiredWorkflow("legacy-wrapper", "  model: m1"))
			mgr, err := NewManager(path, testLogger(), WithRetiredAgents(retiredFixtureLookup))
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}
			previous := mgr.Config()
			previousPrompt := mgr.PromptTemplate()
			mustWriteFile(t, path, tt.content)

			err = mgr.Reload()

			var ce *config.ConfigError
			if !errors.As(err, &ce) || ce.Field != tt.wantField {
				t.Fatalf("Reload() error = %v, want *config.ConfigError with Field %q", err, tt.wantField)
			}
			if !errors.Is(mgr.LastLoadError(), err) {
				t.Errorf("LastLoadError() = %v, want %v", mgr.LastLoadError(), err)
			}
			if got := mgr.Config(); got.Agent.Kind != previous.Agent.Kind || len(got.AgentKindConversions()) != len(previous.AgentKindConversions()) {
				t.Errorf("Config() after the failed reload = kind %q with %d conversions, want the previous %q with %d",
					got.Agent.Kind, len(got.AgentKindConversions()), previous.Agent.Kind, len(previous.AgentKindConversions()))
			}
			if mgr.PromptTemplate() != previousPrompt {
				t.Error("PromptTemplate() changed after the failed reload, want the previous template")
			}
		})
	}
}

func TestManager_LabelCommandsRecordsNoBranchAdvisory(t *testing.T) {
	t.Parallel()

	const exclusiveChain = "{{ if .label_review }}review{{ else if .label_fix }}fix{{ else }}work{{ end }}"

	tests := []struct {
		name         string
		body         string
		ruleTemplate string
	}{
		{name: "exclusive chain in a rule template, empty body", ruleTemplate: exclusiveChain},
		{name: "no branch anywhere", body: "Do the task for {{ .issue.title }}."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			workflow := "---\nreactions:\n  label_commands:\n    provider: github\n    review_label: \"sortie:review\"\n    fix_label: \"sortie:fix\"\n"
			if tt.ruleTemplate != "" {
				mustWriteFile(t, filepath.Join(dir, "chain.md"), []byte(tt.ruleTemplate))
				workflow += "dispatch:\n  rules:\n    - name: all\n      template: chain.md\n"
			}
			path := filepath.Join(dir, "WORKFLOW.md")
			mustWriteFile(t, path, []byte(workflow+"---\n"+tt.body+"\n"))

			mgr, err := NewManager(path, testLogger())
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			mgr.Stop()

			for _, a := range mgr.Config().Advisories() {
				if strings.HasPrefix(a.Check, "reactions.label_commands.") {
					t.Errorf("Config().Advisories() has %q, want no label_commands advisory", a.Check)
				}
			}
		})
	}
}

func TestManager_WithLiteralSettings_KeepsListedKeyLiteral(t *testing.T) {
	t.Parallel()

	const doc = `{"$schema":"https://opencode.ai/config.json","model":"$m"}`
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	mustWriteFile(t, path, fmt.Appendf(nil, `---
agent:
  kind: testkind
testkind:
  env: {DOC: '%[1]s'}
  other: $PATH
dispatch:
  rules:
    - name: r
      match: {labels: [cheap]}
      testkind:
        env: {DOC: '%[1]s'}
---
Prompt.
`, doc))

	mgr, err := NewManager(path, testLogger(), WithLiteralSettings(func(string) []string { return []string{"env"} }))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mgr.Stop()
	cfg := mgr.Config()

	for _, rule := range []string{"", "r"} {
		got := config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: "testkind", RuleName: rule}, "").Passthrough
		if env, _ := got["env"].(map[string]any); env["DOC"] != doc {
			t.Errorf("rule %q env DOC = %q, want %q", rule, env["DOC"], doc)
		}
		if got["other"] != os.Getenv("PATH") {
			t.Errorf("rule %q other = %q, want $PATH resolved", rule, got["other"])
		}
	}
	if refs := cfg.ExtensionEnvRefPaths("testkind"); !refs["other"] || refs["env.DOC"] {
		t.Errorf("ExtensionEnvRefPaths(testkind) = %v, want other recorded and env.DOC not", refs)
	}
}
