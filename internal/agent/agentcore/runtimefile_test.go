package agentcore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanRuntimeFileRejects(t *testing.T) {
	t.Parallel()

	const homeVar = "SORTIE_TEST_RUNTIME_HOME"
	errStop := errors.New("stop")

	tests := []struct {
		name      string
		content   string
		skipFile  bool
		limit     int64
		lineLimit int64
		stopAt    string
		wantErr   bool
		wantIs    error
		wantLines []string
	}{
		{
			name:      "line above the line cap",
			content:   "ok\n" + strings.Repeat("x", 11) + "\ntail\n",
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "unterminated last line above the line cap",
			content:   "ok\n" + strings.Repeat("x", 11),
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "file above the byte cap",
			content:   strings.Repeat("ab\n", 10),
			limit:     20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "missing file",
			skipFile:  true,
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
			wantIs:    fs.ErrNotExist,
		},
		{
			name:      "error from the line callback",
			content:   "a\nb\nc\n",
			limit:     1 << 20,
			lineLimit: 10,
			stopAt:    "b",
			wantErr:   true,
			wantIs:    errStop,
			wantLines: []string{"a", "b"},
		},
		{
			name:      "line of exactly the line cap passes",
			content:   strings.Repeat("x", 10) + "\r\nnext",
			limit:     1 << 20,
			lineLimit: 10,
			wantLines: []string{strings.Repeat("x", 10), "next"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			if !tt.skipFile {
				if err := os.WriteFile(filepath.Join(home, "events.jsonl"), []byte(tt.content), 0o600); err != nil {
					t.Fatalf("os.WriteFile: %v", err)
				}
			}
			var seen []string

			err := LaunchTarget{}.ScanRuntimeFile(t.Context(), []string{homeVar + "=" + home}, homeVar, "events.jsonl", tt.limit, tt.lineLimit, func(line []byte) error {
				seen = append(seen, strings.TrimSuffix(string(line), "\r"))
				if tt.stopAt != "" && string(line) == tt.stopAt {
					return errStop
				}
				return nil
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("ScanRuntimeFile() error = %v, want error %t", err, tt.wantErr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("ScanRuntimeFile() error = %v, want wrapping %v", err, tt.wantIs)
			}
			for _, line := range seen {
				if int64(len(line)) > tt.lineLimit {
					t.Errorf("ScanRuntimeFile() delivered a line of %d bytes, want none above %d", len(line), tt.lineLimit)
				}
			}
			if tt.wantLines != nil && strings.Join(seen, "|") != strings.Join(tt.wantLines, "|") {
				t.Errorf("ScanRuntimeFile() lines = %q, want %q", seen, tt.wantLines)
			}
		})
	}
}
