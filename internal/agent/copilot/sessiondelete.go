package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
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

	cmd := target.AuxiliaryCommand(deadlineCtx, []string{"--server", "--stdio"}, stdinReader, nil)
	procutil.SetGroupCancel(cmd, procutil.StopGrace(stopGraceMS))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
		return
	}
	if err := cmd.Start(); err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
		return
	}
	defer func() { _ = cmd.Wait() }() //nolint:errcheck // best-effort; the deletion outcome already logged

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

	if err := writeContentLengthFramed(stdin, payload); err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
		return
	}

	success, err := readSessionDeleteResponse(deadlineCtx, stdout)
	_ = stdin.Close() //nolint:errcheck // deletion outcome already resolved or timed out
	if err == nil && !success {
		err = errSessionDeleteNotSuccessful
	}
	if err != nil {
		logger.Warn("failed to delete credential verification session", slog.Any("error", err))
	}
}

var errSessionDeleteNotSuccessful = errors.New("session.delete reported success: false")

// writeContentLengthFramed uses the Content-Length framing the Copilot
// server expects, as its SDK does, rather than newline-delimited JSON.
func writeContentLengthFramed(w io.Writer, payload []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

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
			length, err := readContentLengthHeader(br)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(br, body); err != nil {
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

func readContentLengthHeader(br *bufio.Reader) (int, error) {
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(value))
		if convErr != nil {
			return 0, fmt.Errorf("invalid Content-Length header %q: %w", value, convErr)
		}
		if n > maxSessionDeleteResponseLength {
			return 0, fmt.Errorf("content-length %d exceeds %d byte limit", n, maxSessionDeleteResponseLength)
		}
		length = n
	}
	if length < 0 {
		return 0, fmt.Errorf("message carries no Content-Length header")
	}
	return length, nil
}
