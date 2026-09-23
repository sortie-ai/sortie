package clientprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const malformedToolCallNameTestRun = "TestParseSessionUpdateMalformedToolCallName|TestToolCallLifecycleSurvivesMalformedName|TestPermissionRequestMalformedToolCallNameSelectsRefusingOption"

func repoRootFromTestFile(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report the test file's own path")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func mustReadProductionFile(t *testing.T, repoRoot, relPath string) (absPath string, content []byte) {
	t.Helper()
	absPath = filepath.Join(repoRoot, relPath)
	data, err := os.ReadFile(absPath) //nolint:gosec // G304: absPath is built from the test's own repository root
	if err != nil {
		t.Fatalf("read %s: %v", absPath, err)
	}
	return absPath, data
}

func mustReplaceExactly(t *testing.T, path string, content []byte, old string, wantCount int, replacement string) []byte {
	t.Helper()
	s := string(content)
	if got := strings.Count(s, old); got != wantCount {
		t.Fatalf("%s: %q occurs %d time(s), want %d: the overlay mutation no longer matches the production source", path, old, got, wantCount)
	}
	return []byte(strings.ReplaceAll(s, old, replacement))
}

func writeScratchFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func writeOverlay(t *testing.T, dir string, replace map[string]string) string {
	t.Helper()
	type overlayFile struct {
		Replace map[string]string
	}
	encoded, err := json.Marshal(overlayFile{Replace: replace})
	if err != nil {
		t.Fatalf("encode overlay: %v", err)
	}
	return writeScratchFile(t, dir, "overlay.json", encoded)
}

func runPropertyThirteenTests(t *testing.T, repoRoot, overlayPath string) (exitCode int, output string) {
	t.Helper()

	args := []string{"test"}
	if overlayPath != "" {
		args = append(args, "-overlay="+overlayPath)
	}
	args = append(args, "-run", malformedToolCallNameTestRun, "-count=1", "./internal/agent/clientprotocol")

	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = repoRoot
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return 0, buf.String()
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), buf.String()
	}
	t.Fatalf("running go test: %v\n%s", err, buf.String())
	return 0, ""
}

func TestMalformedToolCallNameOverlayNegativeControl(t *testing.T) {
	repoRoot := repoRootFromTestFile(t)
	scratch := t.TempDir()

	parsePath, parseSrc := mustReadProductionFile(t, repoRoot, filepath.Join("internal", "agent", "clientprotocol", "parse.go"))
	pumpPath, pumpSrc := mustReadProductionFile(t, repoRoot, filepath.Join("internal", "agent", "clientprotocol", "pump.go"))
	wireGenPath, wireGenSrc := mustReadProductionFile(t, repoRoot, filepath.Join("internal", "agent", "clientprotocol", "wire_gen.go"))

	unadaptedParse := mustReplaceExactly(t, parsePath, parseSrc, "dropMalformedToolCallName(raw)", 2, "raw")
	unadaptedPump := mustReplaceExactly(t, pumpPath, pumpSrc, "dropMalformedToolCallNameFromParams(msg.Params)", 1, "msg.Params")
	preMoveWireGen := mustReplaceExactly(t, wireGenPath, wireGenSrc, "\tName       *string             `json:\"name,omitempty\"`\n", 2, "")

	unadaptedParsePath := writeScratchFile(t, scratch, "parse.go", unadaptedParse)
	unadaptedPumpPath := writeScratchFile(t, scratch, "pump.go", unadaptedPump)
	preMoveWireGenPath := writeScratchFile(t, scratch, "wire_gen.go", preMoveWireGen)

	t.Run("adaptation removed, pin moved: property 13 fails", func(t *testing.T) {
		overlay := writeOverlay(t, t.TempDir(), map[string]string{
			parsePath: unadaptedParsePath,
			pumpPath:  unadaptedPumpPath,
		})
		code, output := runPropertyThirteenTests(t, repoRoot, overlay)
		if code == 0 {
			t.Errorf("go test with the adaptation removed exited 0, want a nonzero exit proving the adaptation guards the property:\n%s", output)
		}
	})

	t.Run("adaptation removed, pre-move wire_gen: property 13 passes as it did before the move", func(t *testing.T) {
		overlay := writeOverlay(t, t.TempDir(), map[string]string{
			parsePath:   unadaptedParsePath,
			pumpPath:    unadaptedPumpPath,
			wireGenPath: preMoveWireGenPath,
		})
		code, output := runPropertyThirteenTests(t, repoRoot, overlay)
		if code != 0 {
			t.Errorf("go test with the adaptation removed and the pre-move wire_gen.go exited %d, want 0:\n%s", code, output)
		}
	})

	t.Run("adaptation and pin move together: property 13 passes", func(t *testing.T) {
		code, output := runPropertyThirteenTests(t, repoRoot, "")
		if code != 0 {
			t.Errorf("go test against the committed tree exited %d, want 0:\n%s", code, output)
		}
	})
}
