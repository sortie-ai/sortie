//go:build unix

package clientprotocol

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// geminiSemanticProbeTimeout bounds every deterministic wait a fake
// local probe control performs.
const geminiSemanticProbeTimeout = 10 * time.Second

// geminiQualificationRuntime is everything one qualification run
// resolves exactly once: the executable and its self-reported version,
// the one operator-selected model, the controlled workspace and its
// run-scoped homes, the run-scoped policy with its unique marker, the
// five local probes, the positive MCP fixture, the baseline prompt and
// timeout profile, and the subprocess environment allowlist. Every
// collector shares this resolution; only each declared input_id varies.
type geminiQualificationRuntime struct {
	Config           geminiQualificationConfig
	Version          string
	Workspace        geminiQualificationWorkspace
	PolicyPath       string
	PolicyDenyMarker string
	Probes           geminiQualificationProbes
	MCP              geminiMCPFixture
	Timeouts         domain.AgentConfig
	Env              []string
}

// geminiResolveQualificationRuntime resolves the run's shared inputs
// once: it captures the executable's self-reported version with one
// bounded call, builds the controlled workspace, writes the policy and
// the five probes under it, and creates the MCP fixture. Values that
// must never be printed are not: the version is ephemeral evidence and
// the deny marker is a public token.
func geminiResolveQualificationRuntime(t *testing.T, config geminiQualificationConfig) geminiQualificationRuntime {
	t.Helper()

	workspace := geminiNewQualificationWorkspace(t)
	probes := geminiWriteProbeExecutables(t, workspace.Checkout)
	policyPath, denyMarker := geminiWriteQualificationPolicy(t, t.TempDir(), probes)
	mcp := geminiNewMCPFixture(t, t.TempDir())

	env, err := geminiBuildQualificationSubprocessEnv(config, workspace.Home, workspace.CLIHome)
	if err != nil {
		t.Fatalf("build the qualification subprocess environment: %v", err)
	}

	return geminiQualificationRuntime{
		Config:           config,
		Version:          geminiCaptureVersion(t, config, env),
		Workspace:        workspace,
		PolicyPath:       policyPath,
		PolicyDenyMarker: denyMarker,
		Probes:           probes,
		MCP:              mcp,
		Timeouts: domain.AgentConfig{
			Kind:           "agent-client-protocol",
			Command:        config.CommandPath,
			ReadTimeoutMS:  30000,
			TurnTimeoutMS:  300000,
			StallTimeoutMS: 60000,
		},
		Env: env,
	}
}

// geminiCaptureVersion captures the executable's self-reported version
// once with a bounded call. The value is ephemeral per-session
// evidence; it never reaches notes or summaries.
//
// SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND can itself be a shim
// from a version manager such as asdf, whose own "#!/usr/bin/env
// node" shebang re-resolves node through PATH and can land on that
// manager's node shim rather than a pinned install. That shim needs
// the manager's own HOME-derived data directory to pick a node
// version; the run-scoped, deliberately isolated HOME this
// environment carries has none, so
// geminiQualificationToolchainEnvNames forwards that data-directory
// coordinate from the invoking environment when present, without
// widening the HOME isolation itself.
func geminiCaptureVersion(t *testing.T, config geminiQualificationConfig, env []string) string {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(config.CommandPath, "--version") //nolint:gosec // the operator-selected executable, resolved by the gate
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		diagnostic := strings.TrimSpace(stderr.String())
		if len(diagnostic) > 2000 {
			diagnostic = diagnostic[:2000]
		}
		t.Fatalf("capture the version coordinate: %v; stderr: %q", err, diagnostic)
	}
	return strings.TrimSpace(stdout.String())
}

// geminiProtocolTurnOutcome is one bounded protocol turn's normalized
// terminal outcome: the session/prompt response's stop reason, if one
// arrived, and the normalized error kind the adapter settled the turn
// with.
type geminiProtocolTurnOutcome struct {
	StopReason stopReason
	ErrKind    domain.AgentErrorKind
	Attributed bool // the collector attributed the outcome to this case's controlled operation
}

// geminiProtocolCase maps one terminal outcome to the one semantic case
// it distinctly and machine-readably represents, per the normalizer's
// closed mapping. A permission denial, a failed tool result, and an
// invalid configuration are not distinct representations of any
// required case and map to none of them.
func geminiProtocolCase(outcome geminiProtocolTurnOutcome) (qualification.Case, bool) {
	switch {
	case outcome.ErrKind == domain.ErrTurnInputRequired:
		return qualification.CaseHumanInput, true
	case outcome.ErrKind == domain.ErrTurnOutcomeUnknown:
		return qualification.CaseUnknownOutcome, true
	case outcome.ErrKind == domain.ErrTurnRefused:
		return qualification.CaseRuntimeRefusal, true
	case outcome.ErrKind == domain.ErrTurnCancelled:
		return qualification.CaseCancellation, true
	case outcome.ErrKind == domain.ErrPortExit:
		return qualification.CaseRetryableTransport, true
	case outcome.ErrKind == domain.ErrResponseError && outcome.Attributed:
		return qualification.CaseRuntimeFailure, true
	case outcome.StopReason == stopReasonEndTurn:
		return qualification.CaseSuccess, true
	case outcome.StopReason == stopReasonMaxTokens || outcome.StopReason == stopReasonMaxTurnRequests:
		return qualification.CaseLimitReached, true
	}
	return "", false
}

// geminiRetryCaseFor maps one terminal outcome to the retry case it
// distinctly represents, separately from the disposition case: the
// refusal and non-retryable-refusal tuples share one physical run but
// remain separate records.
func geminiRetryCaseFor(outcome geminiProtocolTurnOutcome) (qualification.Case, bool) {
	switch outcome.ErrKind {
	case domain.ErrTurnRefused:
		return qualification.CaseNonRetryableRefusal, true
	case domain.ErrPortExit:
		if classification := domain.ErrPortExit.RetryClassification(); classification.Retryable {
			return qualification.CaseRetryableTransport, true
		}
	}
	return "", false
}

// geminiSemanticObservation is what one bounded probe observed. Tag
// names the case the probe induced the observation for, which is the
// only case the observation may ever fill.
type geminiSemanticObservation struct {
	Tag          qualification.Case
	Induced      bool
	Distinct     bool
	Structured   bool
	Failure      qualification.Outcome
	SessionID    string
	EvidencePath string
	Detail       string
}

// geminiSemanticRecordFor builds one semantic probe record from an
// observation. The classification follows the evidence contract: a
// failure or an uninduced case is not_observed with its specific
// verdict and never becomes gap; an observed, distinct, structured
// outcome is usable; an observed but conflated or unstructured outcome
// is gap. An observation tagged for another case is refused, so one
// record can never satisfy more than one tuple.
func geminiSemanticRecordFor(surface qualification.Surface, capability qualification.Capability, caseID qualification.Case, obs geminiSemanticObservation) qualification.Record {
	rec := qualification.Record{
		SchemaVersion: 1,
		ObservedAt:    qualification.FixtureTime,
		Scenario:      qualification.ScenarioSemanticProbe,
		Surface:       surface,
		Capability:    capability,
		SemanticCase:  new(caseID),
		InputID:       qualification.CaseInputs[caseID],
		Detail:        obs.Detail,
	}
	if rec.Detail == "" {
		rec.Detail = fmt.Sprintf("%s %s case observation on %s", capability, caseID, surface)
	}

	switch {
	case obs.Tag != caseID:
		// An observation collected for a different case never fills
		// this tuple: permission denial, invalid configuration, and
		// synthetic fixtures cannot stand in for the live class.
		rec.Outcome = qualification.OutcomeNotObserved
		rec.Grade = qualification.GradeNotObserved
	case obs.Failure != "":
		rec.Outcome = obs.Failure
		rec.Grade = qualification.GradeNotObserved
	case !obs.Induced:
		rec.Outcome = qualification.OutcomeNotObserved
		rec.Grade = qualification.GradeNotObserved
	case obs.Distinct && obs.Structured:
		rec.Outcome = qualification.OutcomePass
		rec.Grade = qualification.GradeUsable
		rec.SessionID = new(obs.SessionID)
		rec.EvidencePath = new(obs.EvidencePath)
	default:
		rec.Outcome = qualification.OutcomePass
		rec.Grade = qualification.GradeGap
		rec.SessionID = new(obs.SessionID)
		rec.EvidencePath = new(obs.EvidencePath)
	}

	switch surface {
	case qualification.SurfaceProtocol:
		rec.Source = qualification.SourceProtocolStable
		rec.AgentName = new(qualification.FixtureAgentName)
		rec.AgentVersion = new(qualification.FixtureAgentVer)
		rec.ProtocolVersion = new(1)
	default:
		rec.Source = qualification.SourceNativeStructured
		rec.AgentVersion = new(qualification.FixtureAgentVer)
	}
	return rec
}

// geminiInputSpec is one fixed input's catalog entry: the exact prompt
// the collector sends, whether the probe launches at all, and, when a
// substitution exists, the probe whose absolute path is the only
// substituted value.
type geminiInputSpec struct {
	Prompt string
	Launch bool
	Probe  *string
}

// geminiSemanticInputCatalog builds the fixed input catalog for one
// run. Fixed prompts are exact; every substitution input varies only by
// the single substituted value its contract names.
func geminiSemanticInputCatalog(probes geminiQualificationProbes, nonce string) map[qualification.InputID]geminiInputSpec {
	probeInput := func(format string, probePath string) geminiInputSpec {
		return geminiInputSpec{
			Prompt: fmt.Sprintf(format, probePath),
			Launch: true,
			Probe:  new(probePath),
		}
	}

	return map[qualification.InputID]geminiInputSpec{
		qualification.InputDispositionSuccess: {
			Prompt: "Reply with exactly SORTIE_BASELINE_OK and do not call any tool.",
			Launch: true,
		},
		qualification.InputDispositionRuntimeFailure: probeInput(
			"Invoke the test-owned probe at %s exactly once.", probes.Failing),
		qualification.InputDispositionRuntimeRefusal: {
			Prompt: "Decline to continue this turn and report your runtime refusal outcome without calling a tool.",
			Launch: true,
		},
		qualification.InputDispositionCancellation: probeInput(
			"Invoke the test-owned probe at %s exactly once.", probes.Cancellation),
		qualification.InputDispositionLimitReached: {
			// No documented deterministic inducer: no separate launch.
			Launch: false,
		},
		qualification.InputRetryableTransport: probeInput(
			"Invoke the test-owned probe at %s exactly once.", probes.Transport),
		qualification.InputRetryNonRetryableRefusal: {
			// The refusal retry references the refusal disposition's
			// own physical run and carries no new prompt.
			Launch: false,
		},
		qualification.InputRetryHumanInput: probeInput(
			"Call run_shell_command exactly once with the absolute path %s, with no arguments and no other tool.", probes.Permission),
		qualification.InputRetryUnknownOutcome: {
			Prompt: "End this turn with an outcome outside your documented terminal outcome set and emit no normal answer.",
			Launch: true,
		},
		qualification.InputPolicyControl: probeInput(
			"Call run_shell_command exactly once with the absolute path %s, with no arguments and no other tool.", probes.PolicyLoad),
		qualification.InputPermissionProbe: probeInput(
			"Call run_shell_command exactly once with the absolute path %s, with no arguments and no other tool.", probes.Permission),
		qualification.InputMCPProbe: {
			Prompt: fmt.Sprintf("Call the test server's %s tool exactly once with the nonce %s and then reply with the nonce the tool returned.", geminiMCPToolName, nonce),
			Launch: true,
		},
		qualification.InputContinuationSeed: {
			Prompt: fmt.Sprintf("Remember the nonce %s for the rest of this conversation and reply exactly STORED.", nonce),
			Launch: true,
		},
		qualification.InputContinuationRecall: {
			// The recall prompt does not repeat the nonce; the prior
			// conversation supplies it.
			Prompt: "Reply with the nonce supplied by the prior conversation and no other text.",
			Launch: true,
		},
		qualification.InputE2E: {
			Prompt: "Reply with exactly SORTIE_E2E_OK and do not call any tool.",
			Launch: true,
		},
	}
}

// TestGeminiSemanticProbeCatalog confirms the fixed input catalog: the
// exact prompts match the input table verbatim, every substitution
// input varies by exactly its one substituted value, limit_reached
// launches no process, and each semantic case resolves to its own
// input.
func TestGeminiSemanticProbeCatalog(t *testing.T) {
	t.Parallel()

	fakeProbes := geminiQualificationProbes{
		PolicyLoad:   "/controlled/policy-load-probe",
		Permission:   "/controlled/permission-probe",
		Failing:      "/controlled/failing-probe",
		Cancellation: "/controlled/cancellation-probe",
		Transport:    "/controlled/transport-probe",
	}
	otherProbes := geminiQualificationProbes{
		PolicyLoad:   "/other/policy-load-probe",
		Permission:   "/other/permission-probe",
		Failing:      "/other/failing-probe",
		Cancellation: "/other/cancellation-probe",
		Transport:    "/other/transport-probe",
	}

	catalog := geminiSemanticInputCatalog(fakeProbes, "sortie-nonce-fixture")
	other := geminiSemanticInputCatalog(otherProbes, "sortie-nonce-other")

	t.Run("exact fixed prompts", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			id     qualification.InputID
			prompt string
		}{
			{qualification.InputDispositionSuccess, "Reply with exactly SORTIE_BASELINE_OK and do not call any tool."},
			{qualification.InputDispositionRuntimeRefusal, "Decline to continue this turn and report your runtime refusal outcome without calling a tool."},
			{qualification.InputRetryUnknownOutcome, "End this turn with an outcome outside your documented terminal outcome set and emit no normal answer."},
			{qualification.InputE2E, "Reply with exactly SORTIE_E2E_OK and do not call any tool."},
			{qualification.InputContinuationRecall, "Reply with the nonce supplied by the prior conversation and no other text."},
		}
		for _, tt := range tests {
			if catalog[tt.id].Prompt != tt.prompt {
				t.Errorf("catalog prompt for %s = %q, want the exact fixed prompt %q", tt.id, catalog[tt.id].Prompt, tt.prompt)
			}
			if !catalog[tt.id].Launch {
				t.Errorf("catalog entry %s does not launch, want one bounded attempt", tt.id)
			}
		}
	})

	t.Run("substitution inputs vary only by their single substituted value", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			id    qualification.InputID
			probe string
		}{
			{qualification.InputDispositionRuntimeFailure, "failing-probe"},
			{qualification.InputDispositionCancellation, "cancellation-probe"},
			{qualification.InputRetryableTransport, "transport-probe"},
			{qualification.InputRetryHumanInput, "permission-probe"},
			{qualification.InputPolicyControl, "policy-load-probe"},
		}
		for _, tt := range tests {
			spec := catalog[tt.id]
			otherSpec := other[tt.id]
			if !spec.Launch {
				t.Errorf("catalog entry %s does not launch", tt.id)
			}
			if spec.Probe == nil || filepath.Base(*spec.Probe) != tt.probe {
				t.Errorf("catalog entry %s names %v, want only %s", tt.id, spec.Probe, tt.probe)
			}
			if !strings.Contains(spec.Prompt, tt.probe) {
				t.Errorf("prompt %q does not name %s", spec.Prompt, tt.probe)
			}
			// The only difference between two runs' prompts is the
			// substituted path: rewriting one probe path into the other
			// makes the two prompts equal.
			if normalized := strings.ReplaceAll(spec.Prompt, "/controlled/", "/other/"); normalized != otherSpec.Prompt {
				t.Errorf("catalog entry %s varies by more than its substituted path", tt.id)
			}
		}
	})

	t.Run("limit reached launches no process", func(t *testing.T) {
		t.Parallel()

		if catalog[qualification.InputDispositionLimitReached].Launch {
			t.Error("limit_reached catalog entry launches, want no separate launch")
		}
	})

	t.Run("the nonce substitution appears in the seed and MCP prompts only", func(t *testing.T) {
		t.Parallel()

		if !strings.Contains(catalog[qualification.InputContinuationSeed].Prompt, "sortie-nonce-") {
			t.Errorf("seed prompt %q carries no nonce", catalog[qualification.InputContinuationSeed].Prompt)
		}
		if strings.Contains(catalog[qualification.InputContinuationRecall].Prompt, "sortie-nonce") {
			t.Errorf("recall prompt %q repeats the nonce, want no nonce substitution", catalog[qualification.InputContinuationRecall].Prompt)
		}
		if !strings.Contains(catalog[qualification.InputMCPProbe].Prompt, "sortie-nonce") {
			t.Errorf("MCP prompt %q carries no nonce", catalog[qualification.InputMCPProbe].Prompt)
		}
		if normalized := strings.ReplaceAll(catalog[qualification.InputContinuationSeed].Prompt, "sortie-nonce-fixture", "sortie-nonce-x"); normalized == catalog[qualification.InputContinuationSeed].Prompt {
			t.Error("seed prompt does not vary with the generated nonce")
		}
	})

	t.Run("every semantic case maps to exactly one catalog input", func(t *testing.T) {
		t.Parallel()

		for _, capability := range qualification.ComparisonCapabilities[:2] {
			for _, caseID := range qualification.CapabilityCases[capability] {
				inputID, known := qualification.CaseInputs[caseID]
				if !known {
					t.Fatalf("semantic case %s carries no input mapping", caseID)
				}
				if _, inCatalog := catalog[inputID]; !inCatalog {
					t.Errorf("semantic case %s maps to input %s, which the catalog omits", caseID, inputID)
				}
			}
		}
	})
}

// TestGeminiSemanticProbeClassificationControls drives the semantic
// collectors with fake local processes only: the cancellation and
// transport-loss controls run against the fixture probes, the protocol
// outcome mapping runs against scripted in-memory sessions, and the
// forbidden-substitution control proves one record can never satisfy
// another case's tuple.
func TestGeminiSemanticProbeClassificationControls(t *testing.T) {
	t.Parallel()

	workspace := geminiNewQualificationWorkspace(t)
	probes := geminiWriteProbeExecutables(t, workspace.Checkout)

	t.Run("cancellation control with a fake local probe", func(t *testing.T) {
		t.Parallel()

		control := geminiRunCancellationControl(t, probes.Cancellation, workspace.Checkout)
		if !control.MarkerPresent {
			t.Fatal("cancellation control observed no started marker, want fixture induction")
		}
		if !control.GroupDrained {
			t.Fatal("cancellation control left its process group alive after graceful cancellation")
		}

		obs := geminiSemanticObservation{
			Tag:          qualification.CaseCancellation,
			Induced:      control.MarkerPresent,
			Distinct:     true,
			Structured:   true,
			SessionID:    "sess-cancellation-fixture",
			EvidencePath: "/turn/stop_reason",
			Detail:       "bounded probe cancellation completed with a distinct outcome",
		}
		rec := geminiSemanticRecordFor(qualification.SurfaceNativeJSON, qualification.CapabilityTurnDisposition, qualification.CaseCancellation, obs)
		if rec.Outcome != qualification.OutcomePass || rec.Grade != qualification.GradeUsable {
			t.Errorf("cancellation record = %s/%s, want pass/usable", rec.Outcome, rec.Grade)
		}
		if err := checkGeminiVerdictClassification(&rec); err != nil {
			t.Errorf("checkGeminiVerdictClassification() error = %v", err)
		}
	})

	t.Run("transport loss control with a fake local probe", func(t *testing.T) {
		t.Parallel()

		control := geminiRunTransportLossControl(t, probes.Transport, workspace.Checkout)
		if !control.MarkerPresent || !control.GroupDrained {
			t.Fatalf("transport control = %+v, want marker induction and a drained group", control)
		}

		classification := domain.ErrPortExit.RetryClassification()
		if !classification.Retryable || classification.Backoff != domain.BackoffExponential {
			t.Errorf("ErrPortExit retry classification = %+v, want retryable with exponential backoff", classification)
		}
		caseID, mapped := geminiProtocolCase(geminiProtocolTurnOutcome{ErrKind: domain.ErrPortExit})
		if !mapped || caseID != qualification.CaseRetryableTransport {
			t.Errorf("geminiProtocolCase(ErrPortExit) = %q, %v, want retryable_runtime_or_transport_failure", caseID, mapped)
		}
	})

	t.Run("protocol outcome mapping table", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			outcome     geminiProtocolTurnOutcome
			wantCase    qualification.Case
			wantMapped  bool
			wantRetry   qualification.Case
			wantRetryOK bool
		}{
			{
				name:       "end_turn is a distinct success",
				outcome:    geminiProtocolTurnOutcome{StopReason: stopReasonEndTurn},
				wantCase:   qualification.CaseSuccess,
				wantMapped: true,
			},
			{
				name:        "refusal stop reason is a distinct refusal and non-retryable",
				outcome:     geminiProtocolTurnOutcome{StopReason: stopReasonRefusal, ErrKind: domain.ErrTurnRefused},
				wantCase:    qualification.CaseRuntimeRefusal,
				wantMapped:  true,
				wantRetry:   qualification.CaseNonRetryableRefusal,
				wantRetryOK: true,
			},
			{
				name:        "stream end is the retryable transport loss",
				outcome:     geminiProtocolTurnOutcome{ErrKind: domain.ErrPortExit},
				wantCase:    qualification.CaseRetryableTransport,
				wantMapped:  true,
				wantRetry:   qualification.CaseRetryableTransport,
				wantRetryOK: true,
			},
			{
				name:       "cancellation is its own distinct case",
				outcome:    geminiProtocolTurnOutcome{ErrKind: domain.ErrTurnCancelled},
				wantCase:   qualification.CaseCancellation,
				wantMapped: true,
			},
			{
				name:       "input required maps to human input with no retry",
				outcome:    geminiProtocolTurnOutcome{ErrKind: domain.ErrTurnInputRequired},
				wantCase:   qualification.CaseHumanInput,
				wantMapped: true,
			},
			{
				name:       "unknown stop reason maps to unknown outcome",
				outcome:    geminiProtocolTurnOutcome{ErrKind: domain.ErrTurnOutcomeUnknown},
				wantCase:   qualification.CaseUnknownOutcome,
				wantMapped: true,
			},
			{
				name:       "max tokens is a naturally occurring limit",
				outcome:    geminiProtocolTurnOutcome{StopReason: stopReasonMaxTokens, ErrKind: domain.ErrTurnFailed},
				wantCase:   qualification.CaseLimitReached,
				wantMapped: true,
			},
			{
				name:       "an attributed prompt-level error is a runtime failure",
				outcome:    geminiProtocolTurnOutcome{ErrKind: domain.ErrResponseError, Attributed: true},
				wantCase:   qualification.CaseRuntimeFailure,
				wantMapped: true,
			},
			{
				name:       "an unattributed prompt error is no runtime-failure evidence",
				outcome:    geminiProtocolTurnOutcome{ErrKind: domain.ErrResponseError, Attributed: false},
				wantCase:   "",
				wantMapped: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				caseID, mapped := geminiProtocolCase(tt.outcome)
				if mapped != tt.wantMapped || caseID != tt.wantCase {
					t.Errorf("geminiProtocolCase(%+v) = %q, %v, want %q, %v", tt.outcome, caseID, mapped, tt.wantCase, tt.wantMapped)
				}
				retryCase, retryOK := geminiRetryCaseFor(tt.outcome)
				if retryOK != tt.wantRetryOK || retryCase != tt.wantRetry {
					t.Errorf("geminiRetryCaseFor(%+v) = %q, %v, want %q, %v", tt.outcome, retryCase, retryOK, tt.wantRetry, tt.wantRetryOK)
				}
			})
		}
	})

	t.Run("forbidden substitution never fills another tuple", func(t *testing.T) {
		t.Parallel()

		// A permission-denial observation is collected for the
		// permission probe; it must not fill the runtime-refusal
		// disposition tuple.
		permissionObservation := geminiSemanticObservation{
			Tag:          qualification.CaseHumanInput,
			Induced:      true,
			Distinct:     true,
			Structured:   true,
			SessionID:    "sess-permission",
			EvidencePath: "session/request_permission",
			Detail:       "permission request answered",
		}
		rec := geminiSemanticRecordFor(qualification.SurfaceProtocol, qualification.CapabilityTurnDisposition, qualification.CaseRuntimeRefusal, permissionObservation)
		if rec.Outcome != qualification.OutcomeNotObserved || rec.Grade != qualification.GradeNotObserved {
			t.Errorf("mismatched-tag record = %s/%s, want not_observed/not_observed", rec.Outcome, rec.Grade)
		}
		if rec.SessionID != nil || rec.EvidencePath != nil {
			t.Errorf("mismatched-tag record carries session %v or path %v, want both null", rec.SessionID, rec.EvidencePath)
		}

		// An uninduced case stays not_observed, never gap.
		uninduced := geminiSemanticRecordFor(qualification.SurfaceProtocol, qualification.CapabilityTurnDisposition, qualification.CaseUnknownOutcome, geminiSemanticObservation{Tag: qualification.CaseUnknownOutcome})
		if uninduced.Outcome != qualification.OutcomeNotObserved || uninduced.Grade != qualification.GradeNotObserved {
			t.Errorf("uninduced record = %s/%s, want not_observed/not_observed", uninduced.Outcome, uninduced.Grade)
		}

		// A harness failure keeps its specific verdict and never
		// becomes gap evidence about the runtime surface.
		failed := geminiSemanticRecordFor(qualification.SurfaceNativeJSON, qualification.CapabilityRetryClassification, qualification.CaseRetryableTransport, geminiSemanticObservation{
			Tag:     qualification.CaseRetryableTransport,
			Failure: qualification.OutcomePrerequisiteFailed,
		})
		if failed.Outcome != qualification.OutcomePrerequisiteFailed || failed.Grade != qualification.GradeNotObserved {
			t.Errorf("failure record = %s/%s, want prerequisite_failed/not_observed", failed.Outcome, failed.Grade)
		}
	})

	t.Run("a conflated unstructured outcome grades gap", func(t *testing.T) {
		t.Parallel()

		rec := geminiSemanticRecordFor(qualification.SurfaceNativeText, qualification.CapabilityTurnDisposition, qualification.CaseSuccess, geminiSemanticObservation{
			Tag:          qualification.CaseSuccess,
			Induced:      true,
			Distinct:     false,
			Structured:   false,
			SessionID:    "sess-native-text",
			EvidencePath: "/text/final",
			Detail:       "unstructured residue only",
		})
		if rec.Outcome != qualification.OutcomePass || rec.Grade != qualification.GradeGap {
			t.Errorf("unstructured record = %s/%s, want pass/gap", rec.Outcome, rec.Grade)
		}
		if rec.Source != qualification.SourceNativeStructured {
			t.Errorf("native record source = %s, want native_structured", rec.Source)
		}
	})

	t.Run("a protocol turn through the adapter settles into its mapped case", func(t *testing.T) {
		t.Parallel()

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		markSessionKnown(state)

		var events []domain.AgentEvent
		turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
		promptID := out.awaitMethod(t, methodSessionPrompt)
		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonRefusal})
		turnOutcome := awaitOutcome(t, turnCh)

		var agentErr *domain.AgentError
		if !errors.As(turnOutcome.err, &agentErr) {
			t.Fatalf("runTurn() error = %v, want a *domain.AgentError", turnOutcome.err)
		}
		if agentErr.Kind != domain.ErrTurnRefused {
			t.Errorf("runTurn() error kind = %s, want %s for the refusal stop reason", agentErr.Kind, domain.ErrTurnRefused)
		}
		protocolOutcome := geminiProtocolTurnOutcome{StopReason: stopReasonRefusal, ErrKind: agentErr.Kind}
		caseID, mapped := geminiProtocolCase(protocolOutcome)
		if !mapped || caseID != qualification.CaseRuntimeRefusal {
			t.Errorf("geminiProtocolCase() = %q, %v, want runtime_refusal", caseID, mapped)
		}
		retryCase, retryOK := geminiRetryCaseFor(protocolOutcome)
		if !retryOK || retryCase != qualification.CaseNonRetryableRefusal {
			t.Errorf("geminiRetryCaseFor() = %q, %v, want non_retryable_refusal", retryCase, retryOK)
		}
	})
}

// geminiCancellationControl is what a bounded cancellation control
// observed with fake local processes: the probe's marker appeared, the
// graceful group cancellation drained the group, and nothing else ran.
type geminiCancellationControl struct {
	MarkerPresent bool
	GroupDrained  bool
}

// geminiRunCancellationControl launches the cancellation probe in its
// own process group, waits for its public started marker, cancels the
// group gracefully once, and bounds the exit wait. Nothing here
// touches a real tracker, repository, or network.
func geminiRunCancellationControl(t *testing.T, probePath string, dir string) geminiCancellationControl {
	t.Helper()

	cmd := exec.Command(probePath) //nolint:gosec // the probe path is written by this test under its own temp workspace
	cmd.Dir = dir
	procutil.SetProcessGroup(cmd)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cancellation probe: %v", err)
	}
	t.Cleanup(func() {
		_ = procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	waitForFile(t, geminiProbeMarkerPath(probePath), geminiSemanticProbeTimeout)
	if err := procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("graceful cancellation signal: %v", err)
	}
	_, _ = cmd.Process.Wait()

	return geminiCancellationControl{
		MarkerPresent: true,
		GroupDrained:  geminiAwaitGroupDrain(t, cmd.Process.Pid, geminiSemanticProbeTimeout),
	}
}

// geminiTransportLossControl is what a bounded transport-loss control
// observed with fake local processes.
type geminiTransportLossControl struct {
	MarkerPresent bool
	GroupDrained  bool
}

// geminiRunTransportLossControl launches the transport probe, waits for
// its marker to prove the child is active, then sends SIGKILL to the
// captured process group through procutil.SignalProcessGroup and
// requires a bounded drain.
func geminiRunTransportLossControl(t *testing.T, probePath string, dir string) geminiTransportLossControl {
	t.Helper()

	cmd := exec.Command(probePath) //nolint:gosec // the probe path is written by this test under its own temp workspace
	cmd.Dir = dir
	procutil.SetProcessGroup(cmd)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start transport probe: %v", err)
	}
	t.Cleanup(func() {
		_ = procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	waitForFile(t, geminiProbeMarkerPath(probePath), geminiSemanticProbeTimeout)
	if err := procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("transport-loss signal: %v", err)
	}
	_, _ = cmd.Process.Wait()

	return geminiTransportLossControl{
		MarkerPresent: true,
		GroupDrained:  geminiAwaitGroupDrain(t, cmd.Process.Pid, geminiSemanticProbeTimeout),
	}
}

// geminiAwaitGroupDrain polls the group's liveness until it drains and
// reports whether that happened within the bound.
func geminiAwaitGroupDrain(t *testing.T, pgid int, within time.Duration) bool {
	t.Helper()

	deadline := time.Now().Add(within)
	for {
		present, err := geminiProcessGroupPresent(pgid)
		if err != nil {
			t.Fatalf("liveness query: %v", err)
		}
		if !present {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
