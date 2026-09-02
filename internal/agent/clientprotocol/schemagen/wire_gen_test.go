package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const maxDiffLineLen = 200

// callerDir reports the directory holding this test file, resolved
// independently of the working directory a test runner chooses.
func callerDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report the test file's own path")
	}
	return filepath.Dir(file)
}

// firstDiffLine reports the 1-indexed line number of the first line at
// which got and want disagree, along with that line's content on each
// side. A side with fewer lines than the other reports an empty line
// past its own end.
func firstDiffLine(got, want []byte) (line int, gotLine, wantLine string) {
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")

	total := max(len(gotLines), len(wantLines))
	for i := range total {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			return i + 1, g, w
		}
	}
	return total + 1, "", ""
}

func truncateLine(s string) string {
	if len(s) <= maxDiffLineLen {
		return s
	}
	return s[:maxDiffLineLen] + "...(truncated)"
}

func TestGenerateMatchesCommittedWireGen(t *testing.T) {
	t.Parallel()

	dir := callerDir(t)
	assetsDir := filepath.Join(dir, "..", "testdata", "schema-v1.21.0")
	wireGenPath := filepath.Join(dir, "..", "wire_gen.go")

	got, err := Generate(assetsDir)
	if err != nil {
		t.Fatalf("Generate(%q) returned error: %v", assetsDir, err)
	}
	if gotCount := bytes.Count(got, []byte("\ntype ")); gotCount != 104 {
		t.Errorf("Generate(%q) type declaration count = %d, want 104", assetsDir, gotCount)
	}
	for _, continuation := range []string{
		"type loadSessionRequest ",
		"type loadSessionResponse ",
		"type resumeSessionRequest ",
		"type resumeSessionResponse ",
	} {
		if !bytes.Contains(got, []byte(continuation)) {
			t.Errorf("Generate(%q) does not contain permitted continuation type %q", assetsDir, continuation)
		}
	}

	want, err := os.ReadFile(wireGenPath) //nolint:gosec // G304: wireGenPath is derived from the test file's own location, not external input
	if err != nil {
		t.Fatalf("reading committed %s: %v", wireGenPath, err)
	}

	if bytes.Equal(got, want) {
		return
	}

	line, gotLine, wantLine := firstDiffLine(got, want)
	t.Fatalf("Generate(%q) line %d = %q, want %q (comparing against %s)",
		assetsDir, line, truncateLine(gotLine), truncateLine(wantLine), wireGenPath)
}
