//go:build unix

package agentcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/waittest"
)

type echoThenHangParams struct{ PIDFile string }

func init() {
	scenarios["echoThenHang"] = agenttest.Typed(func(_ []string, p echoThenHangParams) int {
		_ = os.WriteFile(p.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
		_, _ = io.Copy(os.Stdout, os.Stdin)
		agenttest.Hang()
		return 0
	})
}

func echo(stdin io.Writer, stdout io.Reader, payload string) error {
	if _, err := io.WriteString(stdin, payload); err != nil {
		return err
	}
	got := make([]byte, len(payload))
	_, err := io.ReadFull(stdout, got)
	if err == nil && string(got) != payload {
		err = fmt.Errorf("echo = %q, want %q", got, payload)
	}
	return err
}

func TestRunHostExchange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		limit    int64
		exchange func(cancel context.CancelFunc, stdin io.Writer, stdout io.Reader) error
		wantErr  bool
		wantIs   error
	}{
		{name: "bytes cross both ways and the command is gone on return", exchange: func(_ context.CancelFunc, w io.Writer, r io.Reader) error { return echo(w, r, "ping") }},
		{name: "read past the limit fails", limit: 4, wantErr: true, exchange: func(_ context.CancelFunc, w io.Writer, r io.Reader) error { return echo(w, r, "abcde") }},
		{name: "done context during a blocked read", wantErr: true, wantIs: context.Canceled, exchange: func(cancel context.CancelFunc, _ io.Writer, stdout io.Reader) error {
			cancel()
			_, err := io.ReadFull(stdout, make([]byte, 1))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pidFile := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithCancel(waittest.Context(t))
			defer cancel()

			var pid int
			err := LaunchTarget{}.RunHostExchange(ctx, nil, HostCommand{Name: agenttest.FakeRuntime(t, t.TempDir(), "tool", "echoThenHang", echoThenHangParams{PIDFile: pidFile}), Limit: tt.limit}, func(stdin io.Writer, stdout io.Reader) error {
				pid = waittest.PIDFile(t, pidFile)
				return tt.exchange(cancel, stdin, stdout)
			})

			if (err != nil) != tt.wantErr || (tt.wantIs != nil && !errors.Is(err, tt.wantIs)) {
				t.Errorf("RunHostExchange(limit %d) = %v, want error %t wrapping %v", tt.limit, err, tt.wantErr, tt.wantIs)
			}
			waittest.ProcessGone(t, pid)
		})
	}
}
