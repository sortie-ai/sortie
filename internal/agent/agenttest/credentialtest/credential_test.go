package credentialtest

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Prefixed so the fixture kinds cannot collide with a real adapter kind.
const (
	credentialTestKindWithCommand    = "agenttest-credential-with-command"
	credentialTestKindWithoutCommand = "agenttest-credential-without-command"
	credentialTestKindUsage          = "agenttest-credential-usage"
)

func credentialTestConstructor() (domain.AgentAdapter, error) {
	return nil, errors.New("credentialtest: fixture kind has no real adapter")
}

func init() {
	registry.Agents.RegisterWithMeta(credentialTestKindWithCommand, credentialTestConstructor, registry.AgentMeta{
		RequiresCommand: true,
	})
	registry.Agents.RegisterWithMeta(credentialTestKindWithoutCommand, credentialTestConstructor, registry.AgentMeta{
		RequiresCommand: false,
	})
	registry.Agents.RegisterWithMeta(credentialTestKindUsage, credentialTestConstructor, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionPerModel,
		UsageSessionRules: []registry.UsageSessionRule{
			{
				When:        func(_ map[string]any, remote bool) bool { return remote },
				Arrival:     registry.UsageArrivalNone,
				Attribution: registry.UsageAttributionNone,
			},
			{
				When:        func(passthrough map[string]any, _ bool) bool { return passthrough["usage"] == "off" },
				Arrival:     registry.UsageArrivalNone,
				Attribution: registry.UsageAttributionNone,
			},
		},
	})
}

type fakeReporter struct {
	errors []string
}

func (f *fakeReporter) Helper() {}

func (f *fakeReporter) Errorf(format string, args ...any) {
	f.errors = append(f.errors, format)
}

type scriptedAdapter struct {
	startErr error
	runErr   error
}

var _ domain.AgentAdapter = (*scriptedAdapter)(nil)

func (a *scriptedAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	if a.startErr != nil {
		return domain.Session{}, a.startErr
	}
	return domain.Session{ID: "sess-verify"}, nil
}

func (a *scriptedAdapter) RunTurn(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	if a.runErr != nil {
		return domain.TurnResult{}, a.runErr
	}
	return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
}

func (a *scriptedAdapter) StopSession(context.Context, domain.Session) error { return nil }

func verifiedAdapter() domain.AgentAdapter {
	return &scriptedAdapter{}
}

func unverifiedAdapter() domain.AgentAdapter {
	return &scriptedAdapter{runErr: &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "refused"}}
}

func sshFailedAdapter() domain.AgentAdapter {
	return &scriptedAdapter{runErr: &domain.AgentError{Kind: domain.ErrPortExit, Message: "ssh connection failed", Err: sshutil.ErrConnectionFailed}}
}

type callTrackingAdapter struct {
	startErr error
	runErr   error
	stopErr  error
	calls    []string
}

var _ domain.AgentAdapter = (*callTrackingAdapter)(nil)

func (a *callTrackingAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	a.calls = append(a.calls, "start")
	if a.startErr != nil {
		return domain.Session{}, a.startErr
	}
	return domain.Session{ID: "sess-working"}, nil
}

func (a *callTrackingAdapter) RunTurn(context.Context, domain.Session, domain.RunTurnParams) (domain.TurnResult, error) {
	a.calls = append(a.calls, "run")
	if a.runErr != nil {
		return domain.TurnResult{}, a.runErr
	}
	return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
}

func (a *callTrackingAdapter) StopSession(context.Context, domain.Session) error {
	a.calls = append(a.calls, "stop")
	return a.stopErr
}

type usageAdapter struct {
	events []domain.AgentEvent
	result domain.TurnResult
	runErr error
}

var _ domain.AgentAdapter = (*usageAdapter)(nil)

func (a *usageAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{ID: "sess-usage"}, nil
}

func (a *usageAdapter) RunTurn(_ context.Context, _ domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	for _, event := range a.events {
		params.OnEvent(event)
	}
	return a.result, a.runErr
}

func (a *usageAdapter) StopSession(context.Context, domain.Session) error { return nil }

func measuredUsageAdapter(model string) *usageAdapter {
	usage := domain.TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	return &usageAdapter{
		events: []domain.AgentEvent{{Type: domain.EventTokenUsage, Usage: usage, Model: model}},
		result: domain.TurnResult{SessionID: "sess-usage", ExitReason: domain.EventTurnCompleted, Usage: usage, UsageMeasured: true},
	}
}

// runOnStandIn drives fn against a *testing.T whose failures stay
// invisible to the enclosing test. It runs fn on its own goroutine
// because Fatalf and Skipf end the goroutine they run on.
func runOnStandIn(fn func(stand *testing.T)) (stand *testing.T, returned bool) {
	stand = new(testing.T)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(stand)
		returned = true
	}()
	<-done
	return stand, returned
}

func TestRunWorkingLive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		adapter   *callTrackingAdapter
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "success runs start, run, and stop",
			adapter:   &callTrackingAdapter{},
			wantCalls: []string{"start", "run", "stop"},
		},
		{
			name:      "StartSession error skips RunTurn and StopSession",
			adapter:   &callTrackingAdapter{startErr: errors.New("start failed")},
			wantErr:   errors.New("start failed"),
			wantCalls: []string{"start"},
		},
		{
			name:      "RunTurn error is returned and the session is still stopped",
			adapter:   &callTrackingAdapter{runErr: errors.New("run failed")},
			wantErr:   errors.New("run failed"),
			wantCalls: []string{"start", "run", "stop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := RunWorkingLive(tt.adapter, domain.StartSessionParams{})

			if (err == nil) != (tt.wantErr == nil) {
				t.Fatalf("RunWorkingLive() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && err.Error() != tt.wantErr.Error() {
				t.Errorf("RunWorkingLive() error = %q, want %q", err.Error(), tt.wantErr.Error())
			}
			if !slices.Equal(tt.adapter.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", tt.adapter.calls, tt.wantCalls)
			}
		})
	}
}

func TestRequireRefused(t *testing.T) {
	t.Parallel()

	conforming := newEarlyExitConformingAdapter(t)
	_, earlyExitErr := conforming.StartSession(context.Background(), domain.StartSessionParams{WorkspacePath: t.TempDir()})

	tests := []struct {
		name     string
		err      error
		wantFail bool
	}{
		{name: "credential_unverified is accepted", err: &domain.AgentError{Kind: domain.ErrCredentialUnverified, Message: "refused"}},
		{name: "an early-exit report is accepted", err: earlyExitErr},
		{name: "an unrelated error is rejected", err: &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "unrelated"}, wantFail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stand := new(testing.T)
			RequireRefused(stand, tt.err)

			if stand.Failed() != tt.wantFail {
				t.Errorf("RequireRefused(%v) failed = %v, want %v", tt.err, stand.Failed(), tt.wantFail)
			}
		})
	}
}

func TestAssertCredentialVerification(t *testing.T) {
	t.Parallel()

	verified := CredentialVerificationCase{Name: "verified", Adapter: verifiedAdapter(), Want: WantVerified}
	unverified := CredentialVerificationCase{Name: "unverified", Adapter: unverifiedAdapter(), Want: WantUnverified}
	sshFailed := CredentialVerificationCase{Name: "ssh failed", Adapter: sshFailedAdapter(), Want: WantSSHConnectionFailed}
	mismatched := CredentialVerificationCase{Name: "mismatched", Adapter: unverifiedAdapter(), Want: WantVerified}

	tests := []struct {
		name     string
		kind     string
		cases    []CredentialVerificationCase
		wantFail bool
	}{
		{name: "all three outcomes match", kind: credentialTestKindWithCommand, cases: []CredentialVerificationCase{verified, unverified, sshFailed}},
		{name: "no ssh case needed without a command", kind: credentialTestKindWithoutCommand, cases: []CredentialVerificationCase{verified, unverified}},
		{name: "unregistered kind", kind: "agenttest-credential-does-not-exist", cases: []CredentialVerificationCase{verified, unverified}, wantFail: true},
		{name: "missing ssh case with a command", kind: credentialTestKindWithCommand, cases: []CredentialVerificationCase{verified, unverified}, wantFail: true},
		{name: "outcome mismatch", kind: credentialTestKindWithCommand, cases: []CredentialVerificationCase{mismatched, unverified, sshFailed}, wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertCredentialVerification(reporter, tt.kind, tt.cases)

			if failed := len(reporter.errors) > 0; failed != tt.wantFail {
				t.Errorf("assertCredentialVerification() failures = %v, want failure %v", reporter.errors, tt.wantFail)
			}
		})
	}
}

func TestVerifyLiveUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		adapter     *usageAdapter
		params      domain.StartSessionParams
		passthrough map[string]any
		wantFatal   bool
		wantFailed  bool
	}{
		{
			name:    "measured turn naming a model passes",
			adapter: measuredUsageAdapter("gpt-6-astra"),
		},
		{
			name: "verification error is fatal",
			adapter: &usageAdapter{
				runErr: &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "refused"},
			},
			wantFatal:  true,
			wantFailed: true,
		},
		{
			name:       "measured turn whose events name no model fails under per_model",
			adapter:    measuredUsageAdapter(""),
			wantFailed: true,
		},
		{
			name:       "ssh session resolves the remote rule",
			adapter:    measuredUsageAdapter("gpt-6-astra"),
			params:     domain.StartSessionParams{SSHHost: "user@stand-in-host"},
			wantFailed: true,
		},
		{
			name:        "passthrough reaches the session rule",
			adapter:     measuredUsageAdapter("gpt-6-astra"),
			passthrough: map[string]any{"usage": "off"},
			wantFailed:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got domain.TurnResult
			stand, returned := runOnStandIn(func(stand *testing.T) {
				got = VerifyLiveUsage(stand, credentialTestKindUsage, tt.adapter, tt.params, tt.passthrough)
			})

			if stand.Failed() != tt.wantFailed {
				t.Errorf("VerifyLiveUsage() failed = %v, want %v", stand.Failed(), tt.wantFailed)
			}
			if returned == tt.wantFatal {
				t.Errorf("VerifyLiveUsage() returned = %v, want %v", returned, !tt.wantFatal)
			}
			if returned && got != tt.adapter.result {
				t.Errorf("VerifyLiveUsage() = %+v, want %+v", got, tt.adapter.result)
			}
		})
	}
}

func TestCredentialNames(t *testing.T) {
	const envVar = "SORTIE_TEST_CREDENTIAL_NAMES"

	tests := []struct {
		name  string
		value string
		unset bool
		want  []string
	}{
		{name: "unset variable", unset: true},
		{name: "empty value"},
		{name: "separators and blanks only", value: " , ,, "},
		{name: "single name", value: "API_KEY", want: []string{"API_KEY"}},
		{name: "order is kept", value: "ZED,ALPHA,MIDDLE", want: []string{"ZED", "ALPHA", "MIDDLE"}},
		{name: "whitespace trimmed and empty elements dropped", value: " ONE , ,TWO,, THREE ,", want: []string{"ONE", "TWO", "THREE"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envVar, tt.value)
			if tt.unset {
				if err := os.Unsetenv(envVar); err != nil {
					t.Fatalf("Unsetenv(%q) = %v, want nil", envVar, err)
				}
			}

			got := CredentialNames(envVar)

			if !slices.Equal(got, tt.want) {
				t.Errorf("CredentialNames(%q) = %q, want %q", tt.value, got, tt.want)
			}
			if (got == nil) != (tt.want == nil) {
				t.Errorf("CredentialNames(%q) nil = %v, want %v", tt.value, got == nil, tt.want == nil)
			}
		})
	}
}

func TestSetRefusedCredential(t *testing.T) {
	const (
		envVar = "SORTIE_TEST_REFUSED_CREDENTIAL_ENV"
		first  = "SORTIE_TEST_REFUSED_FIRST"
		second = "SORTIE_TEST_REFUSED_SECOND"
	)
	xdgRoots := []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}
	originalHome := os.Getenv("HOME")

	t.Run("unset variable skips the case and sets nothing", func(t *testing.T) {
		t.Setenv(envVar, "")
		if err := os.Unsetenv(envVar); err != nil {
			t.Fatalf("Unsetenv(%q) = %v, want nil", envVar, err)
		}

		stand, returned := runOnStandIn(func(stand *testing.T) { SetRefusedCredential(stand, envVar) })

		if !stand.Skipped() || returned {
			t.Errorf("SetRefusedCredential(unset) skipped = %v, returned = %v, want skipped and not returned", stand.Skipped(), returned)
		}
		if os.Getenv("HOME") != originalHome {
			t.Errorf("HOME = %q after a skipped call, want %q", os.Getenv("HOME"), originalHome)
		}
	})

	t.Run("listed names are invalidated and every root points at one empty directory", func(t *testing.T) {
		t.Setenv(envVar, " "+first+" , ,"+second)
		t.Run("call", func(t *testing.T) {
			SetRefusedCredential(t, envVar)

			for _, name := range []string{first, second} {
				if got := os.Getenv(name); got != "sortie-invalid-credential" {
					t.Errorf("%s = %q, want %q", name, got, "sortie-invalid-credential")
				}
			}
			emptyRoot := os.Getenv("HOME")
			if emptyRoot == originalHome {
				t.Errorf("HOME = %q, want a directory other than the original", emptyRoot)
			}
			for _, name := range xdgRoots {
				if got := os.Getenv(name); got != emptyRoot {
					t.Errorf("%s = %q, want %q", name, got, emptyRoot)
				}
			}
			entries, err := os.ReadDir(emptyRoot)
			if err != nil || len(entries) != 0 {
				t.Errorf("ReadDir(%q) = %d entries, %v, want an empty directory", emptyRoot, len(entries), err)
			}
		})

		for _, name := range []string{first, second} {
			if _, present := os.LookupEnv(name); present {
				t.Errorf("%s is still set after the call's test ended, want it restored", name)
			}
		}
		if os.Getenv("HOME") != originalHome {
			t.Errorf("HOME = %q after the call's test ended, want %q", os.Getenv("HOME"), originalHome)
		}
	})
}
