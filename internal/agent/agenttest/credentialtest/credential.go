// Package credentialtest checks an agent adapter's credential
// verification against [agentcore.VerifyCredential].
//
// It is a child of agenttest because it imports agentcore, whose
// in-package tests import agenttest: agenttest must not import
// agentcore, directly or transitively, or those tests fail with an
// import cycle.
package credentialtest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// CredentialVerificationWant names the outcome a
// [CredentialVerificationCase] expects from [agentcore.VerifyCredential].
type CredentialVerificationWant uint8

const (
	// WantVerified expects a nil error: the runtime completed the
	// verification request with its credential.
	WantVerified CredentialVerificationWant = iota

	// WantUnverified expects a credential_unverified [*domain.AgentError].
	WantUnverified

	// WantSSHConnectionFailed expects a port_exit [*domain.AgentError]
	// wrapping [sshutil.ErrConnectionFailed].
	WantSSHConnectionFailed
)

// CredentialVerificationCase is one scripted [domain.AgentAdapter]
// driven through [agentcore.VerifyCredential], and the outcome it
// must produce.
type CredentialVerificationCase struct {
	Name    string
	Adapter domain.AgentAdapter
	Params  domain.StartSessionParams
	Want    CredentialVerificationWant
}

// AssertCredentialVerification fails t unless kind is registered in
// [registry.Agents], cases include at least one [WantVerified] case
// and one [WantUnverified] case, cases include a
// [WantSSHConnectionFailed] case whenever kind's registered
// [registry.AgentMeta.RequiresCommand] is true, and every case,
// driven through [agentcore.VerifyCredential], produces the outcome
// its Want names.
func AssertCredentialVerification(t *testing.T, kind string, cases []CredentialVerificationCase) {
	t.Helper()
	assertCredentialVerification(t, kind, cases)
}

// credentialVerificationReporter lets a package-internal test observe
// failures, which a *testing.T cannot do without failing itself.
type credentialVerificationReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func assertCredentialVerification(t credentialVerificationReporter, kind string, cases []CredentialVerificationCase) {
	t.Helper()

	meta, registered := registry.Agents.Meta(kind)
	if !registered {
		t.Errorf("kind %q is not registered", kind)
		return
	}

	var haveVerified, haveUnverified, haveSSHFailed bool
	for _, tc := range cases {
		switch tc.Want {
		case WantVerified:
			haveVerified = true
		case WantUnverified:
			haveUnverified = true
		case WantSSHConnectionFailed:
			haveSSHFailed = true
		}
	}
	if !haveVerified {
		t.Errorf("kind %q: cases include no WantVerified case", kind)
	}
	if !haveUnverified {
		t.Errorf("kind %q: cases include no WantUnverified case", kind)
	}
	if meta.RequiresCommand && !haveSSHFailed {
		t.Errorf("kind %q: RequiresCommand is true, but cases include no WantSSHConnectionFailed case", kind)
	}

	for _, tc := range cases {
		_, err := agentcore.VerifyCredential(context.Background(), tc.Adapter, agentcore.CredentialVerification{
			Session: tc.Params,
			Issue:   domain.Issue{},
		})
		assertCredentialVerificationOutcome(t, tc.Name, tc.Want, err)
	}
}

func assertCredentialVerificationOutcome(t credentialVerificationReporter, name string, want CredentialVerificationWant, err error) {
	t.Helper()

	switch want {
	case WantVerified:
		if err != nil {
			t.Errorf("case %q: verification returned %v, want nil", name, err)
		}
	case WantUnverified:
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrCredentialUnverified {
			t.Errorf("case %q: verification returned %v, want a credential_unverified *domain.AgentError", name, err)
		}
	case WantSSHConnectionFailed:
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit || !errors.Is(err, sshutil.ErrConnectionFailed) {
			t.Errorf("case %q: verification returned %v, want a port_exit *domain.AgentError wrapping sshutil.ErrConnectionFailed", name, err)
		}
	default:
		t.Errorf("case %q: Want %v is a value outside the declared set", name, want)
	}
}

// VerifyLive runs [agentcore.VerifyCredential] against a real runtime
// with the turn and stop bounds every gated integration suite uses.
func VerifyLive(adapter domain.AgentAdapter, params domain.StartSessionParams) (domain.TurnResult, error) {
	return agentcore.VerifyCredential(context.Background(), adapter, agentcore.CredentialVerification{
		Session:   params,
		TurnBound: 300 * time.Second,
		StopBound: 30 * time.Second,
	})
}

// SetRefusedCredential sets every name listed in the comma-separated
// environment variable envVar to an invalid credential, and points
// HOME, XDG_CONFIG_HOME, XDG_DATA_HOME, XDG_STATE_HOME, and
// XDG_CACHE_HOME at one empty directory, for the rest of t. When envVar
// is unset it skips t, logging why, so a gated suite skips only its
// refused-credential case.
func SetRefusedCredential(t *testing.T, envVar string) {
	t.Helper()
	raw := os.Getenv(envVar)
	if raw == "" {
		t.Skipf("skipping refused-credential case: set %s to a comma-separated list of names the suite sets to an invalid value", envVar)
	}
	for name := range strings.SplitSeq(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			t.Setenv(name, "sortie-invalid-credential")
		}
	}

	emptyRoot := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, emptyRoot)
	}
}

// RequireUnverified fails t unless err is a credential_unverified
// [*domain.AgentError].
func RequireUnverified(t *testing.T, err error) {
	t.Helper()
	assertCredentialVerificationOutcome(t, "refused credential", WantUnverified, err)
}

// RuntimeCases returns the cases a kind that launches a runtime runs:
// verifiedCommand accepting its credential, refusedCommand refusing
// it, and a remote launch of remoteCommand whose ssh client exits 255
// with no output. config supplies every AgentConfig field but Command.
// It puts that stand-in ssh client first on PATH for the rest of t, so
// t must not run in parallel.
func RuntimeCases(t *testing.T, adapter domain.AgentAdapter, config domain.AgentConfig, verifiedCommand, refusedCommand, remoteCommand string) []CredentialVerificationCase {
	t.Helper()
	sshDir := t.TempDir()
	agenttest.FakeRuntime(t, sshDir, "ssh", agenttest.OutputScenario, agenttest.Output{ExitCode: 255})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	params := func(command, sshHost string) domain.StartSessionParams {
		agentConfig := config
		agentConfig.Command = command
		return domain.StartSessionParams{WorkspacePath: t.TempDir(), AgentConfig: agentConfig, SSHHost: sshHost}
	}
	return []CredentialVerificationCase{
		{Name: "working credential", Adapter: adapter, Params: params(verifiedCommand, ""), Want: WantVerified},
		{Name: "refused credential", Adapter: adapter, Params: params(refusedCommand, ""), Want: WantUnverified},
		{Name: "ssh connection failure", Adapter: adapter, Params: params(remoteCommand, "user@stand-in-host"), Want: WantSSHConnectionFailed},
	}
}
