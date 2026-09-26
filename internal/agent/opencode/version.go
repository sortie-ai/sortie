package opencode

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// opencodeVersionPattern matches a bare semantic version, with an
// optional leading "v" already stripped by the caller and an optional
// pre-release or build suffix kept but not decomposed.
var opencodeVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.+-]+)?$`)

// detectRuntimeMajor launches the configured command with --version,
// bounded by [agentcore.AuxiliaryTimeout], and resolves the outcome
// into the OpenCode major that command reports. It re-verifies the
// workspace through the same [agentcore.LaunchTarget.AuxiliaryCommand]
// path every other auxiliary launch uses, so a session start on a
// removed workspace fails here rather than at the first turn.
func detectRuntimeMajor(ctx context.Context, state *sessionState) (runtimeMajor, *domain.AgentError) {
	queryCtx, cancel := context.WithTimeout(ctx, agentcore.AuxiliaryTimeout(state.agentConfig))
	defer cancel()

	stdout := procutil.NewTailBuffer(agentcore.EarlyExitCaptureBytes)
	stderr := procutil.NewTailBuffer(agentcore.EarlyExitCaptureBytes)

	cmd, agentErr := state.target.AuxiliaryCommand(queryCtx, []string{"--version"}, nil, versionQueryEnv(os.Environ()),
		sshutil.EnvVar{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "true"})
	if agentErr != nil {
		return majorUnknown, agentErr
	}

	result, startErr := procutil.RunCapture(cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{
		Stdout: stdout,
		Stderr: stderr,
		Logger: state.logger(),
	})

	switch {
	case startErr != nil:
		return majorUnknown, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "could not start the agent runtime to read its version",
			Err:     startErr,
		}
	case ctx.Err() != nil:
		return majorUnknown, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "the session start ended while the agent runtime was reporting its version",
			Err:     ctx.Err(),
		}
	case queryCtx.Err() != nil:
		return majorUnknown, &domain.AgentError{
			Kind: domain.ErrResponseTimeout,
			Message: fmt.Sprintf("the agent runtime did not report its version within %d ms",
				agentcore.AuxiliaryTimeout(state.agentConfig).Milliseconds()),
		}
	case result.WaitErr != nil:
		if state.target.RemoteCommand != "" && sshutil.ConnectionFailed(procutil.ExtractExitCode(result.WaitErr)) {
			return majorUnknown, agentcore.ConnectionFailedError()
		}
		return majorUnknown, agentcore.ExitedEarly(state.target, result).Report(stderr.Collector(state.logger()))
	}

	version, major, ok := parseRuntimeVersion(stdout.Bytes(), stdout.Truncated())
	if !ok {
		message := "no output"
		if line := firstReadableLine(stdout.Bytes()); line != "" {
			message = redact.Truncate(line, 200)
		}
		return majorUnknown, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "the configured OpenCode command reported no version Sortie can read: " + message,
		}
	}
	if major != 1 && major != 2 {
		return majorUnknown, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("OpenCode %s is not supported; install a 1.x or 2.x release", version),
		}
	}

	return runtimeMajor(major), nil
}

// versionQueryEnv scrubs base of every managed variable a turn's own
// environment carries and appends the one setting the version query
// itself needs, so the query never carries a tool policy or a sharing
// setting computed for a major it has not detected yet.
func versionQueryEnv(base []string) []string {
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if shouldDropManagedEnv(entry) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "OPENCODE_DISABLE_AUTOUPDATE=true")
}

// parseRuntimeVersion extracts the version and major number from a
// version query's standard output. ok is false when out is truncated
// or carries no field, among the first non-empty line's whitespace-
// separated fields, that (after dropping one leading "v") matches a
// bare semantic version.
func parseRuntimeVersion(out []byte, truncated bool) (version string, major int, ok bool) {
	if truncated {
		return "", 0, false
	}

	line := firstReadableLine(out)
	if line == "" {
		return "", 0, false
	}

	for field := range strings.FieldsSeq(line) {
		candidate := strings.TrimPrefix(field, "v")
		match := opencodeVersionPattern.FindStringSubmatch(candidate)
		if match == nil {
			continue
		}
		majorInt, err := strconv.Atoi(match[1])
		if err != nil {
			return "", 0, false
		}
		return candidate, majorInt, true
	}
	return "", 0, false
}

// firstReadableLine returns the first line of out that is non-empty
// after [agentcore.SanitizeLine] and trimming, or "" when every line is
// empty.
func firstReadableLine(out []byte) string {
	for raw := range strings.SplitSeq(string(out), "\n") {
		line := strings.TrimSpace(agentcore.SanitizeLine(raw))
		if line != "" {
			return line
		}
	}
	return ""
}

// checkMajorSettings refuses a session whose passthrough configuration
// names a setting OpenCode 2.x cannot carry. It always returns nil on
// major1.
func checkMajorSettings(pt passthroughConfig, major runtimeMajor) *domain.AgentError {
	if major != major2 {
		return nil
	}

	switch {
	case pt.Pure:
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "opencode.pure is not supported by OpenCode 2.x; remove it or install a 1.x release",
		}
	case pt.Variant != "" && pt.Model == "":
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "opencode.variant needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.variant",
		}
	case pt.Variant != "" && strings.Contains(pt.Model, "#"):
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "opencode.model already names a variant after #; remove that suffix or remove opencode.variant",
		}
	default:
		return nil
	}
}
