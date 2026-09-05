//go:build unix

package clientprotocol

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// geminiKillGroupAndReap terminates a tracked process's group and reaps
// its leader, so the oracle's poll observes a genuinely empty group
// rather than a zombie that still holds it.
func geminiKillGroupAndReap(cmd *exec.Cmd) {
	_ = procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	_, _ = cmd.Process.Wait()
}

// geminiProcessGroupTracker is the run-owned record of every process
// group a qualification run launched. The tracker holds captured PGIDs
// only; its evidence never names them.
type geminiProcessGroupTracker struct {
	groups []int
}

// register captures one launched runtime's process-group identifier.
// The PGID is the group leader's PID, which procutil.SetProcessGroup
// establishes at start.
func (tr *geminiProcessGroupTracker) register(groupPID int) {
	if !slices.Contains(tr.groups, groupPID) {
		tr.groups = append(tr.groups, groupPID)
	}
}

// registerCmd starts cmd in its own process group through the
// production launch contract and captures its group. It returns the
// started command so the test can reap its leader after killing the
// group, which the oracle needs: an unreaped zombie still holds the
// group.
func (tr *geminiProcessGroupTracker) registerCmd(t *testing.T, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tracked process: %v", err)
	}
	tr.register(cmd.Process.Pid)
	t.Cleanup(func() {
		_ = procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// count returns how many groups the run captured.
func (tr *geminiProcessGroupTracker) count() int {
	return len(tr.groups)
}

// geminiProcessMember is one process-group member observed at
// inventory time, recorded by process-group membership and executable
// basename only. ScriptBasenames carries the basenames of argument
// tokens that name files, so a shebang-interpreted probe script is
// attributable to its own basename rather than its interpreter's.
type geminiProcessMember struct {
	PID             int
	PPID            int
	PGID            int
	ScriptBasenames []string
}

// geminiListProcessGroupMembers inventories every visible process in
// the group led by pgid through one bounded ps call. It reports
// executable basenames and group membership only, never command lines
// or argument values.
func geminiListProcessGroupMembers(t *testing.T, pgid int) []geminiProcessMember {
	t.Helper()

	cmd := exec.Command("ps", "-axo", "pid=,pgid=,ppid=,args=") //nolint:gosec // fixed argument vector, no user input
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("inventory process group members: %v", err)
	}

	var members []geminiProcessMember
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		memberPGID, pgidErr := strconv.Atoi(fields[1])
		ppid, ppidErr := strconv.Atoi(fields[2])
		if pidErr != nil || pgidErr != nil || ppidErr != nil || memberPGID != pgid {
			continue
		}
		member := geminiProcessMember{PID: pid, PPID: ppid, PGID: memberPGID}
		member.ScriptBasenames = append(member.ScriptBasenames, filepath.Base(fields[3]))
		// When the process is a shebang script, the argument token
		// after the interpreter names the script itself; recording its
		// basename keeps the member attributable without recording any
		// argument value.
		if len(fields) > 4 && strings.ContainsRune(fields[4], '/') {
			member.ScriptBasenames = append(member.ScriptBasenames, filepath.Base(fields[4]))
		}
		members = append(members, member)
	}
	return members
}

// geminiUnexpectedHelpers returns the identity basenames among a
// group's members that the qualification allowlist does not admit. A
// member is admitted when its own identity matches the allowlist or
// when its direct parent inside the group is an admitted member, which
// attributes a probe's or the runtime's own child processes to their
// declared parent. Anything else is an unexpected helper: a runtime
// failure until it is explained and contained, and the check never
// expands the allowlist to make it pass.
func geminiUnexpectedHelpers(t *testing.T, pgid int, allowedBasenames []string) []string {
	t.Helper()

	members := geminiListProcessGroupMembers(t, pgid)
	admitted := map[int]bool{}
	for _, member := range members {
		if slices.ContainsFunc(member.ScriptBasenames, func(name string) bool {
			return slices.Contains(allowedBasenames, name)
		}) {
			admitted[member.PID] = true
		}
	}

	var unexpected []string
	for _, member := range members {
		if admitted[member.PID] || admitted[member.PPID] {
			continue
		}
		unexpected = append(unexpected, member.ScriptBasenames[0])
	}
	return unexpected
}

// geminiWorkspaceSecurityFacts carries the six fixture facts and
// observations the workspace_security scenario records. It holds
// names, classes, and basenames only: no absolute path, environment
// value, credential, or prompt is ever serialized.
type geminiWorkspaceSecurityFacts struct {
	// PresentProjectConfig lists the project-scoped Gemini
	// configuration paths found in the controlled checkout before
	// launch; a compliant fixture records none.
	PresentProjectConfig []string

	// HomeClass and CLIHomeClass record the configuration-home
	// classes, run_scoped_temp for this fixture.
	HomeClass    string
	CLIHomeClass string

	// EnvNames are the allowlisted environment variable names, sorted
	// lexicographically, with every value omitted.
	EnvNames []string

	// SkipTrustUsed records that the qualification launch carried
	// --skip-trust; TrustPromptObserved records whether a trust prompt
	// or safe-mode refusal was nevertheless observed.
	SkipTrustUsed       bool
	TrustPromptObserved bool

	// MemberBasenames are the process-group member basenames observed
	// at the pre-turn baseline and at cleanup.
	MemberBasenames []string

	// UnexpectedHelpers lists helper basenames outside the allowlist;
	// a non-empty list is a runtime failure the evidence reports
	// without expanding the allowlist.
	UnexpectedHelpers []string
}

// geminiWorkspaceSecurityDetailBudget is the 256-Unicode-code-point
// bound the evidence contract places on every detail string.
const geminiWorkspaceSecurityDetailBudget = 256

// geminiWorkspaceSecurityDetail renders the bounded detail the single
// workspace_security record carries: a config-name list, the two home
// classes, the sorted allowlisted names, the trust facts, and member
// basenames. The allowlisted environment names are recorded while they
// fit the detail bound and elided behind their count otherwise; no
// value is ever included.
func geminiWorkspaceSecurityDetail(facts geminiWorkspaceSecurityFacts) string {
	var b strings.Builder
	if len(facts.PresentProjectConfig) == 0 {
		b.WriteString("no project-scoped gemini configuration in the controlled checkout")
	} else {
		fmt.Fprintf(&b, "present project configuration: %s", strings.Join(facts.PresentProjectConfig, ", "))
	}
	fmt.Fprintf(&b, "; homes %s and %s", facts.HomeClass, facts.CLIHomeClass)

	names := strings.Join(facts.EnvNames, ",")
	if !geminiFitsDetail(&b, names) {
		names = fmt.Sprintf("%d allowlisted names", len(facts.EnvNames))
	}
	fmt.Fprintf(&b, "; allowlisted env names %s", names)

	if facts.SkipTrustUsed {
		b.WriteString("; --skip-trust used")
	} else {
		b.WriteString("; --skip-trust missing")
	}
	if facts.TrustPromptObserved {
		b.WriteString("; trust prompt observed")
	} else {
		b.WriteString("; no trust prompt observed")
	}

	members := strings.Join(facts.MemberBasenames, ",")
	if !geminiFitsDetail(&b, members) {
		members = fmt.Sprintf("%d members", len(facts.MemberBasenames))
	}
	fmt.Fprintf(&b, "; member basenames %s", members)

	if len(facts.UnexpectedHelpers) > 0 {
		fmt.Fprintf(&b, "; unexpected helper %s", strings.Join(facts.UnexpectedHelpers, ","))
	} else {
		b.WriteString("; no unexpected helper")
	}

	detail := b.String()
	if points := utf8.RuneCountInString(detail); points > geminiWorkspaceSecurityDetailBudget {
		detail = string([]rune(detail)[:geminiWorkspaceSecurityDetailBudget])
	}
	return detail
}

// geminiFitsDetail reports whether appending separator plus value keeps
// the builder within the detail bound.
func geminiFitsDetail(b *strings.Builder, value string) bool {
	return utf8.RuneCountInString(b.String())+1+utf8.RuneCountInString(value) <= geminiWorkspaceSecurityDetailBudget
}

// geminiWorkspaceSecurityRecord builds the single workspace_security
// scenario record from the fixture facts. Its verdict is pass only
// when no project configuration was present, no trust prompt was
// observed, and no unexpected helper appeared; every other outcome
// reports runtime_failed with not_observed rather than a gap claim
// about the runtime.
func geminiWorkspaceSecurityRecord(facts geminiWorkspaceSecurityFacts) qualification.Record {
	rec := qualification.Record{
		SchemaVersion: 1,
		ObservedAt:    qualification.FixtureTime,
		Scenario:      qualification.ScenarioWorkspaceSecurity,
		Surface:       qualification.SurfaceAggregate,
		Capability:    qualification.CapabilityWorkspaceSecurity,
		Source:        qualification.SourceProcessObservation,
		InputID:       qualification.InputSecurity,
		EvidencePath:  new("/workspace/settings"),
		Detail:        geminiWorkspaceSecurityDetail(facts),
	}
	switch {
	case len(facts.PresentProjectConfig) == 0 && !facts.TrustPromptObserved && len(facts.UnexpectedHelpers) == 0:
		rec.Grade = qualification.GradeUsable
		rec.Outcome = qualification.OutcomePass
	default:
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeRuntimeFailed
	}
	return rec
}

// geminiProcessCleanupRecord builds the single process_cleanup record
// after bounded liveness checks for every captured Unix process group.
// Its detail reports only checked_groups=<count>, never a PGID, and
// its verdict is pass only when every group satisfied the absence
// oracle within the shared shutdown deadline.
func geminiProcessCleanupRecord(groups int, survivors bool) qualification.Record {
	rec := qualification.Record{
		SchemaVersion: 1,
		ObservedAt:    qualification.FixtureTime,
		Scenario:      qualification.ScenarioProcessCleanup,
		Surface:       qualification.SurfaceAggregate,
		Capability:    qualification.CapabilityProcessCleanup,
		Source:        qualification.SourceProcessObservation,
		InputID:       qualification.InputCleanup,
		EvidencePath:  new("process_group.liveness"),
		Detail:        fmt.Sprintf("checked_groups=%d", groups),
	}
	if survivors {
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeRuntimeFailed
	} else {
		rec.Grade = qualification.GradeUsable
		rec.Outcome = qualification.OutcomePass
	}
	return rec
}

// TestGeminiQualificationProcessCleanupBookkeeping confirms the
// cleanup-side bookkeeping that stays behind after the process-group
// absence oracle moved into the qualification package: the tracker
// counts one registered group per registerCmd call, and the cleanup
// record reports only the group count.
func TestGeminiQualificationProcessCleanupBookkeeping(t *testing.T) {
	t.Parallel()

	t.Run("the tracker counts one registered group", func(t *testing.T) {
		t.Parallel()

		tracker := &geminiProcessGroupTracker{}
		tracker.registerCmd(t, exec.Command("sleep", "5")) //nolint:gosec // bounded fake local process
		if got := tracker.count(); got != 1 {
			t.Fatalf("tracked group count = %d, want 1", got)
		}
	})

	t.Run("the cleanup record reports only the group count", func(t *testing.T) {
		t.Parallel()

		rec := geminiProcessCleanupRecord(9, false)
		if rec.Detail != "checked_groups=9" {
			t.Errorf("cleanup detail = %q, want only checked_groups=9", rec.Detail)
		}
		if rec.Outcome != qualification.OutcomePass || rec.Grade != qualification.GradeUsable {
			t.Errorf("cleanup record = %s/%s, want pass/usable after clean cleanup", rec.Outcome, rec.Grade)
		}
		survivor := geminiProcessCleanupRecord(9, true)
		if survivor.Outcome != qualification.OutcomeRuntimeFailed || survivor.Grade != qualification.GradeNotObserved {
			t.Errorf("survivor cleanup record = %s/%s, want runtime_failed/not_observed", survivor.Outcome, survivor.Grade)
		}
		if !strings.Contains(survivor.Detail, "checked_groups=9") || strings.Contains(survivor.Detail, "pgid") {
			t.Errorf("survivor cleanup detail = %q, want the count and no group identifier", survivor.Detail)
		}
	})
}

// TestGeminiQualificationUnexpectedHelperFails confirms the helper
// allowlist check: a declared local probe running in its own group is
// admitted, and a helper outside the runtime, the five probes, and the
// MCP server fails rather than expanding the allowlist.
func TestGeminiQualificationUnexpectedHelperFails(t *testing.T) {
	t.Parallel()

	workspace := geminiNewQualificationWorkspace(t)
	probes := geminiWriteProbeExecutables(t, workspace.Checkout)
	allowed := append(probes.names(), "gemini", "mcp-fixture-server")

	t.Run("a declared probe is admitted", func(t *testing.T) {
		t.Parallel()

		tracker := &geminiProcessGroupTracker{}
		// The cancellation probe writes its marker and then waits, so
		// the group is alive and inventoriable at check time.
		cmd := tracker.registerCmd(t, exec.Command(probes.Cancellation)) //nolint:gosec // test-owned probe under the test's temp workspace
		pgid := tracker.groups[0]
		waitForFile(t, geminiProbeMarkerPath(probes.Cancellation), geminiProbeMarkerTimeout)

		if unexpected := geminiUnexpectedHelpers(t, pgid, allowed); len(unexpected) != 0 {
			t.Errorf("geminiUnexpectedHelpers() = %v, want none for a declared probe and its own wait child", unexpected)
		}
		geminiKillGroupAndReap(cmd)
		qualification.AwaitProcessGroupAbsence(t, pgid)
	})

	t.Run("an undeclared helper is reported", func(t *testing.T) {
		t.Parallel()

		tracker := &geminiProcessGroupTracker{}
		cmd := tracker.registerCmd(t, exec.Command("sleep", "60")) //nolint:gosec // bounded fake local process killed below
		pgid := tracker.groups[0]

		unexpected := geminiUnexpectedHelpers(t, pgid, allowed)
		if len(unexpected) == 0 {
			t.Fatal("geminiUnexpectedHelpers() = none, want the undeclared helper reported")
		}
		if !slices.Contains(unexpected, "sleep") {
			t.Errorf("unexpected helpers = %v, want the undeclared helper's basename", unexpected)
		}

		// The unexpected helper is contained with the same bounded
		// cleanup, and the absence oracle still holds afterward.
		geminiKillGroupAndReap(cmd)
		qualification.AwaitProcessGroupAbsence(t, pgid)
	})
}

// TestGeminiQualificationWorkspaceSecurityObservation confirms the
// workspace-security observation builder: a compliant fixture reports
// usable with the six bounded facts, a planted project configuration
// or an unexpected helper reports runtime_failed, and neither the
// record nor its detail ever carries an absolute path, an environment
// value, or a credential value.
func TestGeminiQualificationWorkspaceSecurityObservation(t *testing.T) {
	t.Parallel()

	compliant := geminiWorkspaceSecurityFacts{
		PresentProjectConfig: nil,
		HomeClass:            "run_scoped_temp",
		CLIHomeClass:         "run_scoped_temp",
		EnvNames:             []string{"HOME", "PATH"},
		SkipTrustUsed:        true,
		TrustPromptObserved:  false,
		MemberBasenames:      []string{"gemini"},
		UnexpectedHelpers:    nil,
	}

	t.Run("a compliant fixture records usable and decodes cleanly", func(t *testing.T) {
		t.Parallel()

		rec := geminiWorkspaceSecurityRecord(compliant)
		if rec.Outcome != qualification.OutcomePass || rec.Grade != qualification.GradeUsable {
			t.Errorf("compliant workspace record = %s/%s, want pass/usable", rec.Outcome, rec.Grade)
		}
		if err := checkGeminiVerdictClassification(&rec); err != nil {
			t.Errorf("checkGeminiVerdictClassification() error = %v, want nil for the compliant record", err)
		}
		line, err := qualification.MarshalRecord(rec)
		if err != nil {
			t.Fatalf("qualification.MarshalRecord() error = %v", err)
		}
		if _, err := qualification.DecodeRecord(line); err != nil {
			t.Errorf("qualification.DecodeRecord() error = %v, want the workspace record to decode cleanly", err)
		}
		for _, banned := range []string{"/tmp/", "/home/", "value-a", "token-value"} {
			if strings.Contains(rec.Detail, banned) {
				t.Errorf("workspace detail %q carries the banned value shape %q", rec.Detail, banned)
			}
		}
	})

	t.Run("a planted project configuration reports runtime_failed", func(t *testing.T) {
		t.Parallel()

		planted := compliant
		planted.PresentProjectConfig = []string{".gemini/settings.json"}
		rec := geminiWorkspaceSecurityRecord(planted)
		if rec.Outcome != qualification.OutcomeRuntimeFailed || rec.Grade != qualification.GradeNotObserved {
			t.Errorf("planted-config workspace record = %s/%s, want runtime_failed/not_observed", rec.Outcome, rec.Grade)
		}
		if !strings.Contains(rec.Detail, ".gemini/settings.json") {
			t.Errorf("workspace detail = %q, want it to name the planted configuration class", rec.Detail)
		}
	})

	t.Run("an unexpected helper reports runtime_failed", func(t *testing.T) {
		t.Parallel()

		infected := compliant
		infected.UnexpectedHelpers = []string{"stray-helper"}
		rec := geminiWorkspaceSecurityRecord(infected)
		if rec.Outcome != qualification.OutcomeRuntimeFailed {
			t.Errorf("unexpected-helper workspace record = %s, want runtime_failed", rec.Outcome)
		}
	})

	t.Run("environment names are recorded without values", func(t *testing.T) {
		t.Parallel()

		detail := geminiWorkspaceSecurityDetail(compliant)
		for _, name := range compliant.EnvNames {
			if !strings.Contains(detail, name) {
				t.Errorf("workspace detail = %q, want it to name the allowlisted entry %s", detail, name)
			}
		}
		for _, valueShape := range []string{"value-a", "token-value", "="} {
			if strings.Contains(detail, valueShape) {
				t.Errorf("workspace detail = %q, want no environment value shape %q", detail, valueShape)
			}
		}
	})
}
