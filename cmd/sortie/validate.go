package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
)

type validateOutput struct {
	Valid    bool           `json:"valid"`
	Errors   []validateDiag `json:"errors"`
	Warnings []validateDiag `json:"warnings"`
}

type validateDiag struct {
	Severity string `json:"severity"`
	Check    string `json:"check"`
	Message  string `json:"message"`
}

func runValidate(_ context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if containsHelpFlag(args) {
		printValidateHelp(stdout)
		return 0
	}

	fs := flag.NewFlagSet("sortie validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "text", `Output format: "text" or "json"`)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printValidateHelp(stdout)
			return 0
		}
		emitDiags(stdout, stderr, *format, []validateDiag{{Severity: "error", Check: "args", Message: err.Error()}}, nil)
		return 1
	}

	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "sortie validate: invalid --format value %q: must be \"text\" or \"json\"\n", *format) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	path, err := resolveWorkflowPath(fs.Args())
	if err != nil {
		emitDiags(stdout, stderr, *format, []validateDiag{{Severity: "error", Check: "args", Message: err.Error()}}, nil)
		return 1
	}

	// Load raw workflow for schema analysis (owned by this goroutine).
	wf, err := workflow.Load(path)
	if err != nil {
		emitDiags(stdout, stderr, *format, mapManagerError(err), nil)
		return 1
	}

	cfg, err := config.NewServiceConfig(wf.Config)
	if err != nil {
		emitDiags(stdout, stderr, *format, mapManagerError(err), nil)
		return 1
	}

	// wf.Config is the post-env-override raw map. Sole ownership — safe to read.
	var warningDiags []validateDiag
	for _, w := range config.ValidateFrontMatter(wf.Config, cfg) {
		msg := w.Message
		if w.Field != "" {
			msg = w.Field + ": " + msg
		}
		warningDiags = append(warningDiags, validateDiag{
			Severity: "warning",
			Check:    w.Check,
			Message:  msg,
		})
	}

	// Template static analysis.
	tmpl, parseErr := prompt.Parse(wf.PromptTemplate, path, wf.FrontMatterLines)
	if parseErr == nil {
		for _, w := range prompt.AnalyzeTemplate(tmpl) {
			warningDiags = append(warningDiags, validateDiag{
				Severity: "warning",
				Check:    templateWarnCheck(w.Kind),
				Message:  w.Message,
			})
		}
	}

	logger := slog.New(slog.DiscardHandler)

	mgr, err := workflow.NewManager(path, logger,
		workflow.WithValidateFunc(orchestrator.ValidateConfigForPromotion),
		workflow.WithAgentKindProbe(registry.Agents.Has))
	if err != nil {
		emitDiags(stdout, stderr, *format, mapManagerError(err), warningDiags)
		return 1
	}

	preflightParams := orchestrator.PreflightParams{
		ReloadWorkflow:  mgr.Reload,
		ConfigFunc:      mgr.Config,
		TrackerRegistry: registry.Trackers,
		AgentRegistry:   registry.Agents,
	}

	validation := orchestrator.ValidateDispatchConfig(preflightParams)

	for _, w := range validation.Warnings {
		warningDiags = append(warningDiags, validateDiag{
			Severity: "warning",
			Check:    w.Check,
			Message:  w.Message,
		})
	}

	// Fold the offline forge diagnostics into the same exit decision as
	// the preflight errors. A reaction- or activation-only fault must
	// block dispatch even when the dispatch preflight passes, so both the
	// exit code and the JSON valid flag key on the union of preflight,
	// reaction, and activation errors.
	var forgeDiags []validateDiag
	for _, rd := range orchestrator.ValidateReactionConfigs(cfg) {
		forgeDiags = append(forgeDiags, validateDiag{Severity: rd.Severity, Check: rd.Check, Message: rd.Message})
	}
	forgeDiags = append(forgeDiags, activationChecks(cfg)...)

	var forgeErrors []validateDiag
	for _, fd := range forgeDiags {
		if fd.Severity == "warning" {
			warningDiags = append(warningDiags, fd)
			continue
		}
		forgeErrors = append(forgeErrors, fd)
	}

	combinedErrors := mapPreflightErrors(validation.Errors)
	combinedErrors = append(combinedErrors, forgeErrors...)
	if len(combinedErrors) > 0 {
		emitDiags(stdout, stderr, *format, combinedErrors, warningDiags)
		return 1
	}

	// Success path: emit warnings (if any) with valid=true.
	emitDiags(stdout, stderr, *format, nil, warningDiags)
	return 0
}

func emitDiags(stdout io.Writer, stderr io.Writer, format string, errs []validateDiag, warnings []validateDiag) {
	if errs == nil {
		errs = []validateDiag{}
	}
	if warnings == nil {
		warnings = []validateDiag{}
	}
	if format == "json" {
		out := validateOutput{
			Valid:    len(errs) == 0,
			Errors:   errs,
			Warnings: warnings,
		}
		if err := writeJSON(stdout, out); err != nil {
			for _, d := range errs {
				fmt.Fprintf(stderr, "error: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
			}
			for _, d := range warnings {
				fmt.Fprintf(stderr, "warning: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
			}
		}
		return
	}
	for _, d := range errs {
		fmt.Fprintf(stderr, "error: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
	}
	for _, d := range warnings {
		fmt.Fprintf(stderr, "warning: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
	}
}

func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// mapManagerError converts a workflow manager load error into one or
// more validateDiag entries. Dispatch-section *ConfigError values
// arrive here unchanged from BuildDispatchConfig and route through
// the "config." + Field arm, producing entries such as
// "config.dispatch.rules[0].agent". Per-rule template parse failures
// arrive as *prompt.TemplateError and route through "template_parse"
// with the offending absolute path included verbatim.
func mapManagerError(err error) []validateDiag {
	var we *workflow.WorkflowError
	if errors.As(err, &we) {
		check := "workflow_load"
		if we.Kind == workflow.ErrFrontMatterNotMap {
			check = "workflow_front_matter"
		}
		return []validateDiag{{Severity: "error", Check: check, Message: err.Error()}}
	}

	var ce *config.ConfigError
	if errors.As(err, &ce) {
		return []validateDiag{{Severity: "error", Check: "config." + ce.Field, Message: ce.Message}}
	}

	var te *prompt.TemplateError
	if errors.As(err, &te) {
		return []validateDiag{{Severity: "error", Check: "template_parse", Message: err.Error()}}
	}

	return []validateDiag{{Severity: "error", Check: "workflow_load", Message: err.Error()}}
}

func mapPreflightErrors(errs []orchestrator.PreflightError) []validateDiag {
	diags := make([]validateDiag, len(errs))
	for i, e := range errs {
		diags[i] = validateDiag{Severity: "error", Check: e.Check, Message: e.Message}
	}
	return diags
}

// activationChecks reports the offline-decidable SCM and CI activation
// faults in the resolved config: an active SCM reaction naming an
// unregistered provider, active SCM reactions disagreeing on the provider,
// and a ci_feedback.kind naming no registered CI provider. It reuses
// scmProviderConflict and the adapter registries the runtime consults at
// construction, so it constructs no adapter and opens no socket.
func activationChecks(cfg config.ServiceConfig) []validateDiag {
	var diags []validateDiag

	reviewRC := cfg.Reactions["review_comments"]
	autoMergeRC := cfg.Reactions["auto_merge"]
	botReviewRC := cfg.Reactions["bot_review"]
	mergeConflictRC := cfg.Reactions["merge_conflicts"]
	labelReviewActive := cfg.LabelCommands.Provider != "" && cfg.LabelCommands.ReviewLabel != ""
	labelFixActive := cfg.LabelCommands.Provider != "" && cfg.LabelCommands.FixLabel != ""

	activeSCMKinds := []scmReactionKind{
		{name: "review_comments", active: reviewRC.Provider != "", provider: reviewRC.Provider},
		{name: "auto_merge", active: autoMergeRC.Provider != "", provider: autoMergeRC.Provider},
		{name: "bot_review", active: botReviewRC.Provider != "", provider: botReviewRC.Provider},
		{name: "merge_conflicts", active: mergeConflictRC.Provider != "", provider: mergeConflictRC.Provider},
		{name: "label_commands", active: labelReviewActive || labelFixActive, provider: cfg.LabelCommands.Provider},
	}

	activeKinds, providers := scmProviderConflict(activeSCMKinds)
	if len(providers) > 1 {
		diags = append(diags, validateDiag{
			Severity: "error",
			Check:    "reactions.scm_provider_conflict",
			Message:  fmt.Sprintf("active SCM reactions %v must use a single provider, found %v", activeKinds, providers),
		})
	} else if len(providers) == 1 && !registry.SCMAdapters.Has(providers[0]) {
		diags = append(diags, validateDiag{
			Severity: "error",
			Check:    "scm_adapter",
			Message:  fmt.Sprintf("no registered SCM adapter for kind %q named by active reactions", providers[0]),
		})
	}

	if cfg.CIFeedback.Kind != "" && !registry.CIProviders.Has(cfg.CIFeedback.Kind) {
		diags = append(diags, validateDiag{
			Severity: "error",
			Check:    "ci_provider",
			Message:  fmt.Sprintf("no registered CI provider for kind %q", cfg.CIFeedback.Kind),
		})
	}

	return diags
}

func templateWarnCheck(k prompt.WarnKind) string {
	switch k {
	case prompt.WarnDotContext:
		return "dot_context"
	case prompt.WarnUnknownVar:
		return "unknown_var"
	case prompt.WarnUnknownField:
		return "unknown_field"
	default:
		return "template_warning"
	}
}
