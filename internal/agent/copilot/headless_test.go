package copilot

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const headlessServerScenario = "copilot.headless-server"

type headlessServerParams struct {
	CapturePath string
	Replies     [][]string
}

func init() {
	fakeScenarios[headlessServerScenario] = agenttest.Typed(runHeadlessServerScenario)
}

func runHeadlessServerScenario(_ []string, params headlessServerParams) int {
	signal.Ignore(syscall.SIGTERM)
	record := func(event string) {
		f, err := os.OpenFile(params.CapturePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		_, _ = f.WriteString(event + " ")
		_ = f.Close()
	}

	br := bufio.NewReader(os.Stdin)
	for _, reply := range params.Replies {
		length, err := readContentLengthHeader(br)
		if err != nil {
			return 1
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			return 1
		}
		var req struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &req) != nil {
			return 1
		}
		record(req.Method)
		for _, message := range reply {
			if writeContentLengthFramed(os.Stdout, []byte(message)) != nil {
				return 1
			}
		}
	}
	if _, err := br.ReadByte(); err == io.EOF {
		record("stdin-closed")
	}
	return 0
}

func headlessReplies(t *testing.T) [][]string {
	t.Helper()

	data, err := os.ReadFile("testdata/headless_real.txt")
	if err != nil {
		t.Fatalf("reading fixture headless_real.txt: %v", err)
	}
	var replies [][]string
	for line := range strings.Lines(string(data)) {
		if message, ok := strings.CutPrefix(line, "<< "); ok {
			replies = append(replies, []string{strings.TrimSpace(message)})
		}
	}
	return replies
}

func runHeadlessQuota(t *testing.T, replies [][]string) ([]usage.WindowReading, []string, error) {
	t.Helper()

	dir := t.TempDir()
	capturePath := dir + "/captured.json"
	bin := agenttest.FakeRuntime(t, dir, "copilot", headlessServerScenario, headlessServerParams{CapturePath: capturePath, Replies: replies})

	readings, err := headlessQuota(waittest.Context(t), agentcore.LaunchTarget{Command: bin, WorkspacePath: dir}, nil)

	return readings, strings.Fields(string(waittest.File(t, capturePath))), err
}

func TestHeadlessQuota(t *testing.T) {
	t.Parallel()

	t.Run("connect precedes the quota request and stdin closes after the answer", func(t *testing.T) {
		t.Parallel()

		_, captured, err := runHeadlessQuota(t, headlessReplies(t))

		want := []string{"connect", "account.getQuota"}
		wantClosed := append(slices.Clone(want), "stdin-closed")
		// Windows ends the server without a stop grace, so whether it reads the closed stdin first is a race.
		ok := slices.Equal(captured, wantClosed) || runtime.GOOS == "windows" && slices.Equal(captured, want)
		if err != nil || !ok {
			t.Errorf("headlessQuota() events = %q, error %v, want %q and nil", captured, err, wantClosed)
		}
	})

	t.Run("exhausted window serves 100 percent used with no reset at the resetDate instant", func(t *testing.T) {
		t.Parallel()

		readings, _, err := runHeadlessQuota(t, headlessReplies(t))
		if err != nil {
			t.Fatalf("headlessQuota() error = %v, want nil", err)
		}

		got := usage.Windows(quotaSource, readings, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		if len(got) != 1 {
			t.Fatalf("usage.Windows() = %d windows, want 1 (the unlimited chat entry is skipped)", len(got))
		}
		w := got[0]
		if w.Key != "premium_interactions" || w.UsedPercent == nil || *w.UsedPercent != 100 || !w.ResetsAt.IsZero() {
			t.Errorf("usage.Windows() = %+v, want premium_interactions at 100 percent with no reset", w)
		}
		if want := time.Date(2026, 10, 28, 0, 15, 43, 0, time.UTC); !w.ReportedAt.Equal(want) {
			t.Errorf("usage.Windows() ReportedAt = %v, want %v", w.ReportedAt, want)
		}
	})

	t.Run("notifications ahead of an answer are skipped", func(t *testing.T) {
		t.Parallel()

		replies := headlessReplies(t)
		replies[1] = slices.Insert(replies[1], 0, `{"jsonrpc":"2.0","method":"log","params":{}}`, `{"jsonrpc":"2.0","id":9,"result":{}}`)

		readings, _, err := runHeadlessQuota(t, replies)

		if err != nil || len(readings) != 1 {
			t.Errorf("headlessQuota() = %d readings, error %v, want 1 and nil", len(readings), err)
		}
	})

	for name, failing := range map[string]int{"connect": 0, "account.getQuota": 1} {
		t.Run("an error answer to "+name+" ends the pulls", func(t *testing.T) {
			t.Parallel()

			replies := headlessReplies(t)
			replies[failing] = []string{fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Method not found"}}`, failing+1)}

			_, _, err := runHeadlessQuota(t, replies)

			if !errors.Is(err, usage.ErrPullUnsupported) {
				t.Errorf("headlessQuota() error = %v, want it to wrap usage.ErrPullUnsupported", err)
			}
		})
	}
}
