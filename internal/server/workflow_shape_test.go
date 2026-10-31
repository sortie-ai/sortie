package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
)

const pathAbsent = "<absent>"

const shapeBaseYAML = `
tracker: {kind: jira, active_states: [To Do, In Progress], terminal_states: [Done], handoff_state: In Review}
`

const shapeSSHYAML = `
tracker:
  kind: jira
  endpoint: https://svc:SENTINEL-ENDPOINT@jira.example.com
  api_key: SENTINEL-APIKEY
  active_states: [To Do, In Progress]
  terminal_states: [Done]
  handoff_state: In Review
agent: {kind: journaled, command: claude --token SENTINEL-COMMAND, max_turns: 12, stall_timeout_ms: 240000, max_retry_backoff_ms: 120000}
journaled: {api_token: SENTINEL-KINDBLOCK}
worker: {ssh_hosts: [build01.internal, build02.internal], max_concurrent_agents_per_host: 2}
hooks: {after_create: echo SENTINEL-HOOK}
dispatch:
  max_consecutive_hops: 15
  rules:
    - name: quiet
      journaled: {api_token: SENTINEL-RULEBLOCK}
token_rates:
  claude-code: {input_per_mtok: 3.0, output_per_mtok: 15.0, cache_read_per_mtok: 0.3, cache_write_per_mtok: 3.75}
  codex: {input_per_mtok: 2.5}
reactions:
  ci_failure: {provider: github, max_retries: 3, triage: {script: echo SENTINEL-TRIAGE}}
  review_comments: {provider: github, escalation_label: triage-me, debounce_ms: 30000, poll_interval_ms: 90000, token: SENTINEL-EXTRA}
  bot_review: {provider: github, escalation: none, poll_interval_ms: 45000}
  merge_conflicts: {provider: github, escalation: comment, poll_interval_ms: 75000}
  auto_merge: {provider: github, max_retries: 0, poll_interval_ms: 50000}
  merge_completion: {provider: github, target_state: Done, poll_interval_ms: 65000}
  label_commands: {provider: github, poll_interval_ms: 40000}
`

type shapeAgentRegistry map[string]registry.AgentMeta

func (shapeAgentRegistry) Get(string) (registry.AgentConstructor, error) {
	return nil, errors.New("agent construction is outside the route tests")
}

func (r shapeAgentRegistry) Meta(kind string) (registry.AgentMeta, bool) {
	meta, ok := r[kind]
	return meta, ok
}

type shapeCIProvider struct{ domain.CIStatusProvider }

type shapeSCMAdapter struct{ domain.SCMAdapter }

func reportingAgentMeta() registry.AgentMeta {
	return registry.AgentMeta{UsageSources: usage.Sources{Live: usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, DuringTurn: true},
	}}}
}

func journaledAgentMeta() registry.AgentMeta {
	return registry.AgentMeta{UsageSources: usage.Sources{
		Live: usage.Source{
			Category: usage.SourceOutput,
			Strategy: usage.StrategyLive,
			Spend:    usage.SpendDeclaration{Amounts: []usage.AmountDeclaration{{Unit: "premium requests", Shape: usage.AmountSessionCumulative}}},
		},
		Records: usage.Source{
			Category: usage.SourceJournal,
			Strategy: usage.StrategyRecords,
			Spend:    usage.SpendDeclaration{Shape: usage.SpendPerRequest, ServedModel: true},
		},
	}}
}

func buildReaction[T any](t *testing.T, cfg config.ServiceConfig, key string, build func(config.ReactionConfig) (T, error)) (T, bool) {
	t.Helper()

	var zero T
	rc, ok := cfg.Reactions[key]
	if !ok {
		return zero, false
	}
	built, err := build(rc)
	if err != nil {
		t.Fatalf("building reaction %q: %v", key, err)
	}
	return built, true
}

func writeShapeWorkflow(t *testing.T, path, frontMatter string) {
	t.Helper()

	if err := os.WriteFile(path, []byte("---"+frontMatter+"---\nFix the issue.\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

type shapeHarness struct {
	url  string
	path string
	mgr  *workflow.Manager
}

func newShapeHarness(t *testing.T, frontMatter string) *shapeHarness {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeShapeWorkflow(t, path, frontMatter)
	mgr, err := workflow.NewManager(path, logger)
	if err != nil {
		t.Fatalf("workflow.NewManager(%q) = %v, want nil", path, err)
	}
	cfg := mgr.Config()

	review, hasReview := buildReaction(t, cfg, "review_comments", orchestrator.BuildReviewReactionConfig)
	bot, hasBot := buildReaction(t, cfg, "bot_review", orchestrator.BuildBotReviewReactionConfig)
	conflicts, hasConflicts := buildReaction(t, cfg, "merge_conflicts", orchestrator.BuildMergeConflictReactionConfig)
	autoMerge, hasAutoMerge := buildReaction(t, cfg, "auto_merge", orchestrator.BuildAutoMergeReactionConfig)
	completion, hasCompletion := buildReaction(t, cfg, "merge_completion", func(rc config.ReactionConfig) (orchestrator.MergeCompletionReactionConfig, error) {
		return orchestrator.BuildMergeCompletionReactionConfig(rc, cfg.Tracker)
	})
	hasLabels := cfg.LabelCommands.Provider != ""

	params := orchestrator.OrchestratorParams{
		State:                             orchestrator.NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, orchestrator.AgentTotals{}),
		Logger:                            logger,
		WorkflowManager:                   mgr,
		TrackerKind:                       cfg.Tracker.Kind,
		TrackerProject:                    cfg.Tracker.Project,
		PreflightParams:                   orchestrator.PreflightParams{AgentRegistry: shapeAgentRegistry{"claude-code": reportingAgentMeta(), "codex": reportingAgentMeta(), "journaled": journaledAgentMeta()}},
		SCMAdapter:                        shapeSCMAdapter{},
		ReviewConfig:                      review,
		ReviewConfigured:                  hasReview,
		BotReviewConfig:                   bot,
		BotReviewConfigured:               hasBot,
		MergeConflictConfig:               conflicts,
		MergeConflictReactionConfigured:   hasConflicts,
		AutoMergeConfig:                   autoMerge,
		AutoMergeReactionConfigured:       hasAutoMerge,
		MergeCompletionConfig:             completion,
		MergeCompletionReactionConfigured: hasCompletion,
		LabelReviewConfig:                 orchestrator.BuildLabelReviewReactionConfig(cfg.LabelCommands),
		LabelReviewReactionConfigured:     hasLabels,
		LabelFixConfig:                    orchestrator.BuildLabelFixReactionConfig(cfg.LabelCommands),
		LabelFixReactionConfigured:        hasLabels,
	}
	if cfg.CIFeedback.Kind != "" {
		params.CIProvider = shapeCIProvider{}
	}
	orch := orchestrator.NewOrchestrator(params)

	srv := New(Params{
		SnapshotFn: func() (orchestrator.RuntimeSnapshotResult, error) {
			return orchestrator.RuntimeSnapshotResult{Rates: mgr.Rates()}, nil
		},
		RefreshFn:       func() bool { return true },
		Logger:          logger,
		WorkflowShapeFn: orch.WorkflowShape,
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	return &shapeHarness{url: ts.URL, path: path, mgr: mgr}
}

func (h *shapeHarness) state(t *testing.T) (string, map[string]any) {
	t.Helper()

	resp, err := http.Get(h.url + "/api/v1/state")
	if err != nil {
		t.Fatalf("GET /api/v1/state: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading GET /api/v1/state: %v", err)
	}
	var decoded struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Config == nil {
		t.Fatalf("GET /api/v1/state = %s (decode error %v), want a config member", body, err)
	}
	return string(body), decoded.Config
}

func lookupConfigPath(cfg map[string]any, path string) (any, bool) {
	var node any = cfg
	for key := range strings.SplitSeq(path, ".") {
		obj, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = obj[key]; !ok {
			return nil, false
		}
	}
	return node, true
}

func assertConfigPaths(t *testing.T, cfg map[string]any, want map[string]string) {
	t.Helper()

	for path, wantJSON := range want {
		got, found := lookupConfigPath(cfg, path)
		if wantJSON == pathAbsent {
			if _, parentFound := lookupConfigPath(cfg, path[:strings.LastIndex(path, ".")]); !parentFound || found {
				t.Errorf("config.%s = %v (present %t, parent present %t), want absent under a present parent", path, got, found, parentFound)
			}
			continue
		}
		var wantValue any
		if err := json.Unmarshal([]byte(wantJSON), &wantValue); err != nil {
			t.Fatalf("want for config.%s is not JSON: %v", path, err)
		}
		if !found || !reflect.DeepEqual(got, wantValue) {
			t.Errorf("config.%s = %v (present %t), want %s", path, got, found, wantJSON)
		}
	}
}

func TestStateConfig_DispatchShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want map[string]string
	}{
		{
			name: "no rules and no reaction",
			want: map[string]string{
				"tracker.project":               pathAbsent,
				"dispatch.rules":                "[]",
				"dispatch.chains":               "[]",
				"dispatch.default":              "null",
				"dispatch.max_consecutive_hops": "null",
				"dispatch.hop_ceiling":          "10",
				"reactions":                     "{}",
			},
		},
		{
			name: "match rules, catch-all and default",
			yaml: `
dispatch:
  default: {agent: codex}
  rules:
    - {name: bugs, agent: claude-code, match: {labels: [bug]}}
    - {name: docs, match: {labels: [docs]}}
    - {name: everything, agent: claude-code}
`,
			want: map[string]string{
				"dispatch.rules": `[
					{"name":"bugs","selected_by":"match","match":{"labels":["bug"]},"agent_kind":"claude-code","usage_arrival":"incremental"},
					{"name":"docs","selected_by":"match","match":{"labels":["docs"]},"agent_kind":"codex","usage_arrival":"incremental"},
					{"name":"everything","selected_by":"catch_all","agent_kind":"claude-code","usage_arrival":"incremental"}
				]`,
				"dispatch.default": `{"agent_kind":"codex","usage_arrival":"incremental"}`,
				"dispatch.chains":  "[]",
			},
		},
		{
			name: "chain of four stages listed out of next order",
			yaml: `
dispatch:
  rules:
    - {name: review, stage: stage-review}
    - {name: implement, stage: stage-implement, next: review}
    - {name: specify, next: plan, match: {labels: [feature]}}
    - {name: plan, stage: stage-plan, next: implement}
`,
			want: map[string]string{
				"dispatch.rules": `[
					{"name":"review","selected_by":"stage","stage":"stage-review","agent_kind":"claude-code","usage_arrival":"incremental"},
					{"name":"implement","selected_by":"stage","stage":"stage-implement","next":"review","agent_kind":"claude-code","usage_arrival":"incremental"},
					{"name":"specify","selected_by":"match","match":{"labels":["feature"]},"next":"plan","agent_kind":"claude-code","usage_arrival":"incremental"},
					{"name":"plan","selected_by":"stage","stage":"stage-plan","next":"implement","agent_kind":"claude-code","usage_arrival":"incremental"}
				]`,
				"dispatch.chains": `[["specify","plan","implement","review"]]`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newShapeHarness(t, shapeBaseYAML+tt.yaml)

			_, cfg := h.state(t)

			assertConfigPaths(t, cfg, tt.want)
		})
	}
}

func TestStateConfig_NoCredential(t *testing.T) {
	const notifyURL = "https://hooks.example.com/SENTINEL-NOTIFY"
	t.Setenv("SORTIE_TEST_NOTIFY_URL", notifyURL)
	sentinels := regexp.MustCompile(`SENTINEL-[A-Z]+`).FindAllString(shapeSSHYAML+notifyURL, -1)
	h := newShapeHarness(t, shapeSSHYAML+"notifications: [{kind: webhook, url: $SORTIE_TEST_NOTIFY_URL, events: [session.completed]}]\n")

	body, _ := h.state(t)

	for _, sentinel := range sentinels {
		if strings.Contains(body, sentinel) {
			t.Errorf("GET /api/v1/state body contains %q, want no credential value", sentinel)
		}
	}
}

func TestStateConfig_LimitsAndReactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want map[string]string
	}{
		{
			name: "ssh workers with every reaction",
			yaml: shapeSSHYAML,
			want: map[string]string{
				"agent.max_turns":                              "12",
				"agent.stall_timeout_ms":                       "240000",
				"agent.max_retry_backoff_ms":                   "120000",
				"agent.usage_arrival":                          `"turn_end"`,
				"worker.ssh_hosts":                             `["build01.internal","build02.internal"]`,
				"worker.max_concurrent_agents_per_host":        "2",
				"dispatch.max_consecutive_hops":                "15",
				"dispatch.hop_ceiling":                         "15",
				"dispatch.rules":                               `[{"name":"quiet","selected_by":"catch_all","agent_kind":"journaled","usage_arrival":"turn_end"}]`,
				"token_rates.claude-code.input_per_mtok":       "3.0",
				"token_rates.claude-code.output_per_mtok":      "15.0",
				"token_rates.claude-code.cache_read_per_mtok":  "0.3",
				"token_rates.claude-code.cache_write_per_mtok": "3.75",
				"token_rates.codex.input_per_mtok":             "2.5",
				"token_rates.codex.output_per_mtok":            "null",
				"token_rates.codex.cache_read_per_mtok":        "null",
				"token_rates.codex.cache_write_per_mtok":       "null",
				"reactions.ci_failure.max_retries":             "3",
				"reactions.ci_failure.triage":                  "true",
				"reactions.ci_failure.escalation_label":        `"needs-human"`,
				"reactions.review_comments.triage":             "false",
				"reactions.review_comments.escalation_label":   `"triage-me"`,
				"reactions.review_comments.debounce_ms":        "30000",
				"reactions.review_comments.poll_interval_ms":   "90000",
				"reactions.bot_review.triage":                  "false",
				"reactions.bot_review.escalation_label":        pathAbsent,
				"reactions.bot_review.poll_interval_ms":        "45000",
				"reactions.merge_conflicts.triage":             "false",
				"reactions.merge_conflicts.escalation_label":   pathAbsent,
				"reactions.merge_conflicts.poll_interval_ms":   "75000",
				"reactions.auto_merge.max_retries":             "null",
				"reactions.auto_merge.poll_interval_ms":        "50000",
				"reactions.merge_completion.poll_interval_ms":  "65000",
				"reactions.label_commands.poll_interval_ms":    "40000",
			},
		},
		{
			name: "local workers with a journaled rule and unlimited budgets",
			yaml: shapeBaseYAML + `
agent: {stall_timeout_ms: 0}
dispatch:
  rules:
    - {name: journaled-rule, agent: journaled}
`,
			want: map[string]string{
				"agent.usage_arrival":                   `"incremental"`,
				"agent.stall_timeout_ms":                "null",
				"agent.max_sessions":                    "null",
				"agent.max_tokens":                      "null",
				"worker.ssh_hosts":                      "[]",
				"worker.max_concurrent_agents_per_host": "null",
				"dispatch.rules":                        `[{"name":"journaled-rule","selected_by":"catch_all","agent_kind":"journaled","usage_arrival":"turn_end"}]`,
			},
		},
		{
			name: "triage scripts on every kind but ci failure",
			yaml: shapeBaseYAML + `
reactions:
  ci_failure: {provider: github}
  review_comments: {provider: github, triage: {script: echo review}}
  bot_review: {provider: github, triage: {script: echo bot}}
  merge_conflicts: {provider: github, triage: {script: echo conflict}}
`,
			want: map[string]string{
				"reactions.ci_failure.triage":      "false",
				"reactions.review_comments.triage": "true",
				"reactions.bot_review.triage":      "true",
				"reactions.merge_conflicts.triage": "true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newShapeHarness(t, tt.yaml)

			_, cfg := h.state(t)

			assertConfigPaths(t, cfg, tt.want)
		})
	}
}

func TestStateConfig_ReloadKeepsConstructedShape(t *testing.T) {
	t.Parallel()

	h := newShapeHarness(t, `
tracker: {kind: jira, project: PROJ, active_states: [To Do, In Progress], terminal_states: [Done], handoff_state: In Review}
reactions:
  ci_failure: {provider: github, max_retries: 1}
  bot_review: {provider: github, poll_interval_ms: 45000}
`)
	writeShapeWorkflow(t, h.path, `
tracker: {kind: github, project: acme/widgets, active_states: [Open], terminal_states: [Closed], handoff_state: In Review}
reactions:
  ci_failure: {provider: github, max_retries: 5}
  merge_conflicts: {provider: github}
`)
	if err := h.mgr.Reload(); err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}

	_, cfg := h.state(t)

	assertConfigPaths(t, cfg, map[string]string{
		"tracker.kind":                          `"jira"`,
		"tracker.project":                       `"PROJ"`,
		"tracker.active_states":                 `["Open"]`,
		"reactions.bot_review.poll_interval_ms": "45000",
		"reactions.ci_failure.max_retries":      "5",
		"reactions.merge_conflicts":             pathAbsent,
	})
}
