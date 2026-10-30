package codex

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

// TestRunTurn_EndsOnCtxDeadlineAgainstAStalledWriter drives RunTurn
// with its connection wired to an io.Pipe whose read end nobody ever
// reads, so the turn/start line it writes can never complete at the OS
// level, and a second io.Pipe whose write end nobody ever writes to,
// so the reader stays open rather than reporting a stream end. Only
// the turn's own ctx deadline can end this turn; the adapter's write
// itself never returns.
func TestRunTurn_EndsOnCtxDeadlineAgainstAStalledWriter(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const bound = 200 * time.Millisecond
		outR, outW := io.Pipe()
		inR, inW := io.Pipe()
		t.Cleanup(func() {
			_ = outR.Close()
			_ = outW.Close()
			_ = inR.Close()
			_ = inW.Close()
		})

		inbox := jsonrpc.NewInbox[jsonrpc.Message]()
		state := &sessionState{
			threadID:   "thread-001",
			target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
			waitCh:     make(chan struct{}),
			inbox:      inbox,
			readerDone: make(chan struct{}),
			acc:        agentcore.NewRunUsage(),
			ledger:     adaptNewLedger(t),
		}
		state.conn = jsonrpc.NewConn(outW, inR, jsonrpc.Deliver(inbox, identity))
		t.Cleanup(state.conn.Close)
		go watchTermination(state)

		adapter, _ := NewCodexAdapter()
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()

		start := time.Now()
		done := make(chan struct {
			result domain.TurnResult
			err    error
		}, 1)
		go func() {
			result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			done <- struct {
				result domain.TurnResult
				err    error
			}{result, err}
		}()

		got := waittest.Receive(t, done, "RunTurn outcome")
		requireAgentError(t, got.err, domain.ErrPortExit)
		if got.result.UsageMeasured {
			t.Error("RunTurn().UsageMeasured = true, want false")
		}
		if elapsed := time.Since(start); elapsed > bound {
			t.Errorf("RunTurn() took %v, want within the %v ctx deadline (the stalled write must never be what it waits on)", elapsed, bound)
		}
	})
}
