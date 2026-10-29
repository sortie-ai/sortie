//go:build unix

package clientprotocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const scenarioPullStandIn = "clientprotocol.pull-stand-in"

func init() { fakeRuntimeScenarios[scenarioPullStandIn] = agenttest.Typed(runPullStandIn) }

type pullStandInParams struct {
	Args    []string
	Stdout  string
	Replies []string
	Record  string
}

func runPullStandIn(args []string, p pullStandInParams) int {
	if p.Record != "" && os.WriteFile(p.Record, nil, 0o600) != nil {
		return 2
	}
	if !slices.Equal(args, p.Args) {
		return 3
	}
	fmt.Print(p.Stdout)
	in := bufio.NewReader(os.Stdin)
	for _, reply := range p.Replies {
		if _, err := jsonrpc.ReadContentLengthFrame(in, 1<<16); err != nil || jsonrpc.WriteContentLengthFrame(os.Stdout, []byte(reply)) != nil {
			return 4
		}
	}
	if len(p.Replies) > 0 {
		agenttest.Hang()
	}
	return 0
}

// The lab image has no Go test binary, so Node.js, which it ships, replays the exchange.
const pullStandInScript = `#!/usr/bin/env node
const cfg = %s;
if (process.argv.slice(2).join("\0") !== cfg.Args.join("\0")) process.exit(3);
process.stdout.write(cfg.Stdout);
const replies = cfg.Replies || [];
if (!replies.length) process.exit(0);
process.stdin.on("data", (chunk) => {
  for (const _ of chunk.toString().matchAll(/Content-Length/gi)) {
    const reply = replies.shift() ?? "";
    process.stdout.write("Content-Length: " + Buffer.byteLength(reply) + "\r\n\r\n" + reply);
  }
});
`

type pulledRow struct {
	runtimeIdentity
	name        string
	params      pullStandInParams
	observedNow bool
	want        []domain.UsageWindow
	wantErr     error
}

type runtimeIdentity struct{ Executable, Handshake, ACPArg, ProviderEnv, Fixture string }

func pulledRows(t *testing.T) []pulledRow {
	t.Helper()
	var runtimes map[string]runtimeIdentity
	if err := json.Unmarshal(waittest.File(t, "testdata/pulled_windows/runtimes.json"), &runtimes); err != nil {
		t.Fatalf("runtime identities: %v", err)
	}
	var replies []string
	for line := range strings.Lines(string(waittest.File(t, runtimes["exchange"].Fixture))) {
		if reply, ok := strings.CutPrefix(line, "<< "); ok {
			replies = append(replies, strings.TrimSpace(reply))
		}
	}
	return []pulledRow{
		{
			runtimeIdentity: runtimes["exchange"],
			name:            "headless exchange",
			params:          pullStandInParams{Args: []string{"--headless", "--no-auto-update", "--stdio"}, Replies: replies},
			want:            []domain.UsageWindow{{Key: "premium_interactions", Label: "Monthly premium request limit", UsedPercent: new(100.0), ReportedAt: time.Date(2026, 10, 28, 0, 15, 43, 0, time.UTC)}},
		},
		{
			runtimeIdentity: runtimes["limits"],
			name:            "limits command",
			params:          pullStandInParams{Args: []string{"api", "--standalone", "experimental.usage.limits"}, Stdout: string(waittest.File(t, runtimes["limits"].Fixture))},
			observedNow:     true,
			want: []domain.UsageWindow{
				{Key: "rolling", Label: "Rolling limit", UsedPercent: new(37.5), ResetsAt: time.Unix(1793199600, 0).UTC()},
				{Key: "weekly", Label: "Weekly limit", Minutes: 10080, UsedPercent: new(12.0), ResetsAt: time.Unix(1793577600, 0).UTC()},
				{Key: "monthly", Label: "Monthly limit", UsedPercent: new(100.0), Status: domain.UsageWindowExhausted, ResetsAt: time.Unix(1794700800, 0).UTC()},
			},
		},
	}
}

// launchEnv is passed non-nil: a nil one makes the session read the developer's
// own environment, token variables included.
func assertPulledWindows(t *testing.T, row pulledRow, target agentcore.LaunchTarget, launchEnv []string) {
	t.Helper()
	decl := chooseDeclaration(usagesource.Declarations, &implementation{Name: row.Handshake}, target).declaration
	state := &sessionState{target: target, launchEnv: launchEnv, sessionID: "session-1"}
	if merge := decl.LaunchAddition.Merge; merge != nil {
		value, _ := merge("", false)
		state.launchAddition = carriedValue{name: decl.LaunchAddition.Name, value: value, set: true}
	}

	before := time.Now()
	got, err := newSessionPuller(decl, state, false).Pull(waittest.Context(t), usage.EndingAnswered)
	after := time.Now()

	if row.observedNow {
		for i := range got {
			if got[i].ReportedAt.Before(before) || got[i].ReportedAt.After(after) {
				t.Errorf("Pull(%s) window %q ReportedAt = %v, want within [%v, %v]", row.name, got[i].Key, got[i].ReportedAt, before, after)
			}
			got[i].ReportedAt = time.Time{}
		}
	}
	if !errors.Is(err, row.wantErr) || !reflect.DeepEqual(got, row.want) {
		t.Errorf("Pull(%s) = %+v, %v, want %+v, %v", row.name, got, err, row.want, row.wantErr)
	}
}

func localTarget(t *testing.T, row pulledRow) (target agentcore.LaunchTarget, record string) {
	t.Helper()
	dir := t.TempDir()
	record = dir + "/record"
	row.params.Record = record
	standIn := agenttest.FakeRuntime(t, dir, row.Executable, scenarioPullStandIn, row.params)
	return agentcore.LaunchTarget{Command: standIn, Args: []string{row.ACPArg}, WorkspacePath: t.TempDir()}, record
}

func TestPulledWindowsLocal(t *testing.T) {
	t.Parallel()

	rows := pulledRows(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			target, _ := localTarget(t, row)

			assertPulledWindows(t, row, target, []string{"HOME=" + t.TempDir()})
		})
	}

	t.Run("custom provider reports nothing and never starts the stand-in", func(t *testing.T) {
		t.Parallel()
		row := rows[0]
		row.want, row.wantErr = nil, usage.ErrPullUnsupported
		target, record := localTarget(t, row)

		assertPulledWindows(t, row, target, []string{row.ProviderEnv})

		if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Stat(stand-in record) = %v, want it to wrap %v: the stand-in started", err, os.ErrNotExist)
		}
	})
}

func TestPulledWindowsSSHLab(t *testing.T) {
	if reason := agenttest.SSHLabSkipReason(); reason != "" {
		t.Skip(reason)
	}
	rows := pulledRows(t)
	workspace := t.TempDir()
	var install strings.Builder
	for _, row := range rows {
		config, _ := json.Marshal(row.params)
		fmt.Fprintf(&install, "cat > /usr/local/bin/%[1]s <<'STANDIN_EOF'\n%[2]sSTANDIN_EOF\nchmod 755 /usr/local/bin/%[1]s\n", row.Executable, fmt.Sprintf(pullStandInScript, config))
	}
	lab := agenttest.StartSSHLab(t, agenttest.SSHLabParams{Install: install.String(), Dirs: []string{workspace}})

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			target := agentcore.LaunchTarget{Command: "ssh", SSHHost: lab.Host, WorkspacePath: workspace, RemoteCommand: row.Executable + " " + row.ACPArg}

			assertPulledWindows(t, row, target, nil)
		})
	}

	waittest.Until(t, func() error {
		processes, err := lab.Processes(waittest.Context(t))
		if err != nil {
			return err
		}
		if slices.ContainsFunc(processes, func(process string) bool { return strings.Contains(process, rows[0].Executable) }) {
			return fmt.Errorf("the %s stand-in is still running among %q", rows[0].Executable, processes)
		}
		return nil
	})
}
