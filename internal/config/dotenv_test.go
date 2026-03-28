package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDotEnvFile creates a temp .env file with the given content and
// returns its absolute path. Shared by dotenv_test.go, envoverride_test.go
// and config_test.go (all in package config).
func writeDotEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeDotEnvFile: %v", err)
	}
	return path
}

func TestParseDotEnv(t *testing.T) {
	t.Parallel()

	t.Run("empty path returns nil nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseDotEnv("")
		if err != nil {
			t.Fatalf("parseDotEnv(\"\") unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("parseDotEnv(\"\") = %v, want nil", got)
		}
	})

	t.Run("nonexistent file returns nil nil", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "does_not_exist.env")
		got, err := parseDotEnv(path)
		if err != nil {
			t.Fatalf("parseDotEnv(nonexistent) unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("parseDotEnv(nonexistent) = %v, want nil", got)
		}
	})

	tests := []struct {
		name      string
		content   string
		want      map[string]string
		wantErr   bool
		errSubstr string // substring expected in error message
	}{
		{
			name:    "empty file",
			content: "",
			want:    map[string]string{},
		},
		{
			name:    "comment only lines",
			content: "# this is a comment\n  # indented comment\n",
			want:    map[string]string{},
		},
		{
			name:    "blank lines only",
			content: "\n\n   \n",
			want:    map[string]string{},
		},
		{
			name:    "simple key value pair",
			content: "SORTIE_FOO=bar\n",
			want:    map[string]string{"SORTIE_FOO": "bar"},
		},
		{
			name:    "multiple pairs",
			content: "SORTIE_FOO=bar\nSORTIE_BAR=baz\n",
			want:    map[string]string{"SORTIE_FOO": "bar", "SORTIE_BAR": "baz"},
		},
		{
			name:    "non-sortie keys silently ignored",
			content: "OTHER=ignored\nSORTIE_KEEP=yes\nANOTHER_KEY=also_ignored\n",
			want:    map[string]string{"SORTIE_KEEP": "yes"},
		},
		{
			name:    "double-quoted value strips quotes",
			content: "SORTIE_KEY=\"my-value\"\n",
			want:    map[string]string{"SORTIE_KEY": "my-value"},
		},
		{
			name:    "single-quoted value strips quotes",
			content: "SORTIE_KEY='my-value'\n",
			want:    map[string]string{"SORTIE_KEY": "my-value"},
		},
		{
			name:    "whitespace trimmed around key and value",
			content: "  SORTIE_KEY  =  value  \n",
			want:    map[string]string{"SORTIE_KEY": "value"},
		},
		{
			name:    "dollar sign in value not interpolated",
			content: "SORTIE_KEY=$FOO_BAR\n",
			want:    map[string]string{"SORTIE_KEY": "$FOO_BAR"},
		},
		{
			name:    "blank lines between entries",
			content: "\n\nSORTIE_X=1\n\nSORTIE_Y=2\n\n",
			want:    map[string]string{"SORTIE_X": "1", "SORTIE_Y": "2"},
		},
		{
			name:    "embedded equals sign uses first separator",
			content: "SORTIE_KEY=val=ue\n",
			want:    map[string]string{"SORTIE_KEY": "val=ue"},
		},
		{
			name:    "empty value",
			content: "SORTIE_KEY=\n",
			want:    map[string]string{"SORTIE_KEY": ""},
		},
		{
			name:    "mixed sortie and non-sortie keys",
			content: "HOME=/root\nSORTIE_A=1\nPATH=/usr/bin\nSORTIE_B=2\n",
			want:    map[string]string{"SORTIE_A": "1", "SORTIE_B": "2"},
		},
		{
			name:    "comment following entries",
			content: "SORTIE_A=1\n# trailing comment\n",
			want:    map[string]string{"SORTIE_A": "1"},
		},
		{
			name:    "quoted value with spaces inside",
			content: "SORTIE_KEY=\"hello world\"\n",
			want:    map[string]string{"SORTIE_KEY": "hello world"},
		},
		{
			name:    "unmatched quotes not stripped",
			content: "SORTIE_KEY=\"unmatched\n",
			// No closing quote on one line → quote is not stripped, so value is `"unmatched`
			want: map[string]string{"SORTIE_KEY": "\"unmatched"},
		},
		{
			name:      "missing equals at line 1",
			content:   "SORTIE_KEY_NO_EQUALS\n",
			wantErr:   true,
			errSubstr: ":1:",
		},
		{
			name:      "missing equals line number counts blank and comment lines",
			content:   "# comment\n\nSORTIE_BAD_LINE\n",
			wantErr:   true,
			errSubstr: ":3:",
		},
		{
			name:      "invalid key character dash",
			content:   "SORTIE_KEY-INVALID=val\n",
			wantErr:   true,
			errSubstr: "invalid key",
		},
		{
			name:      "invalid key character space",
			content:   "SORTIE KEY=val\n",
			wantErr:   true,
			errSubstr: "invalid key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeDotEnvFile(t, tt.content)

			got, err := parseDotEnv(path)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDotEnv(%q) = nil error, want error", path)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("parseDotEnv(%q) error = %q, want it to contain %q", path, err.Error(), tt.errSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("parseDotEnv(%q) unexpected error: %v", path, err)
			}

			if got == nil {
				t.Fatalf("parseDotEnv(%q) = nil, want non-nil map", path)
			}

			for k, wantV := range tt.want {
				if gotV, ok := got[k]; !ok {
					t.Errorf("parseDotEnv(%q): missing key %q", path, k)
				} else if gotV != wantV {
					t.Errorf("parseDotEnv(%q)[%q] = %q, want %q", path, k, gotV, wantV)
				}
			}
			for k := range got {
				if _, ok := tt.want[k]; !ok {
					t.Errorf("parseDotEnv(%q): unexpected key %q = %q", path, k, got[k])
				}
			}
		})
	}
}
