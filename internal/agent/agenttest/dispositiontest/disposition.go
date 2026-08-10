// Package dispositiontest provides the conformance assertion that checks an
// agent adapter's observed turn disposition against the shared decision in
// [agentcore.DecideTurn].
//
// The helper lives in this child package, rather than in agenttest itself,
// because it imports agentcore: agentcore's own in-package tests declare
// package agentcore and import agenttest for WriteScript and LogSpy, so an
// agenttest import of agentcore would close an import cycle and fail
// go test ./internal/agent/agentcore with "import cycle not allowed in
// test". agenttest MUST NOT import agentcore, directly or transitively, for
// as long as any agentcore test file declares package agentcore and
// imports agenttest. A future in-package agentcore test that needs
// [AssertDispositionContract] MUST be written as package agentcore_test
// instead of adding this package's import to agenttest.
package dispositiontest

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

// AssertDispositionContract fails t when the disposition an adapter
// produced for a named evidence case does not match the disposition
// [agentcore.DecideTurn] assigns to the same case.
//
// AssertDispositionContract pins the decision table, not the evidence
// mapping: it checks that the adapter decided consistently with the ev the
// caller believes it produced, so it catches an adapter that disagrees with
// the shared table but cannot catch an adapter that built the wrong
// evidence from its runtime's wire format. err is typed as the plain error
// interface, not *domain.AgentError, so a typed-nil *domain.AgentError
// returned as a non-nil interface fails this assertion rather than passing
// it.
func AssertDispositionContract(
	t *testing.T,
	ev agentcore.TurnEvidence,
	result domain.TurnResult,
	err error,
) {
	t.Helper()

	want := agentcore.DecideTurn(ev)

	if result.ExitReason != want.ExitReason {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, want.ExitReason)
	}

	wantErr := want.ErrorKind != ""
	if (err != nil) != wantErr {
		t.Errorf("err = %v, want non-nil: %v", err, wantErr)
		return
	}
	if err == nil {
		return
	}

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Errorf("err = %v, want *domain.AgentError", err)
		return
	}
	if agentErr.Kind != want.ErrorKind {
		t.Errorf("err.Kind = %q, want %q", agentErr.Kind, want.ErrorKind)
	}
	if agentErr.Message != want.ErrorMessage {
		t.Errorf("err.Message = %q, want %q", agentErr.Message, want.ErrorMessage)
	}
}
