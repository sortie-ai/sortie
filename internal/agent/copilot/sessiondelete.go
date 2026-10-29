package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

const sessionDeleteRequestID = 1

const maxSessionDeleteResponseLength = 1 << 20

// deleteVerificationSession deletes sessionID through a one-shot copilot
// server. Failures are only logged: a leftover verification conversation
// does not fail the run.
func deleteVerificationSession(ctx context.Context, target agentcore.LaunchTarget, sessionID string, timeout time.Duration, stopGraceMS int, logger *slog.Logger) {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdinReader, stdin := io.Pipe()
	defer func() { _ = stdinReader.Close() }() //nolint:errcheck // best-effort cleanup once the command exits

	cmd, agentErr := target.AuxiliaryCommand(deadlineCtx, []string{"--server", "--stdio"}, stdinReader, nil)
	if agentErr != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", agentErr))
		return
	}
	procutil.SetGroupCancel(cmd, procutil.StopGrace(stopGraceMS))
	pipes, _, err := procutil.StartWithOwnedPipes(deadlineCtx, cmd, logger)
	if err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
		return
	}
	defer pipes.Close() //nolint:errcheck,gosec // best-effort cleanup; runs after the reap wait below
	procutil.NewStderrCollector(pipes.Stderr, logger)
	reaper := procutil.StartReaper(cmd, logger)
	defer func() { <-reaper.Done() }()

	payload, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      sessionDeleteRequestID,
		Method:  "session.delete",
		Params: struct {
			SessionID string `json:"sessionId"`
		}{SessionID: sessionID},
	})
	if err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
		return
	}

	if err := jsonrpc.WriteContentLengthFrame(stdin, payload); err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
		return
	}

	success, err := readSessionDeleteResponse(deadlineCtx, pipes.Stdout)
	_ = stdin.Close() //nolint:errcheck // deletion outcome already resolved or timed out
	if err == nil && !success {
		err = errSessionDeleteNotSuccessful
	}
	if err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
	}
}

var errSessionDeleteNotSuccessful = errors.New("session.delete reported success: false")

type sessionDeleteResponse struct {
	ID     *int `json:"id"`
	Result *struct {
		Success bool `json:"success"`
	} `json:"result"`
}

// readSessionDeleteResponse returns when the response to
// sessionDeleteRequestID arrives or ctx ends. Its reading goroutine
// outlives ctx until the peer is killed and r closes.
func readSessionDeleteResponse(ctx context.Context, r io.Reader) (bool, error) {
	type outcome struct {
		success bool
		err     error
	}
	done := make(chan outcome, 1)

	go func() {
		br := bufio.NewReader(r)
		for {
			body, err := jsonrpc.ReadContentLengthFrame(br, maxSessionDeleteResponseLength)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			var resp sessionDeleteResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				continue
			}
			if resp.ID == nil || *resp.ID != sessionDeleteRequestID {
				continue
			}
			done <- outcome{success: resp.Result != nil && resp.Result.Success}
			return
		}
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case res := <-done:
		return res.success, res.err
	}
}
