package main

import (
	"strings"
	"testing"
)

func TestShortCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"longer than 7 chars truncated", "abcdefgh", "abcdefg"},
		{"exactly 7 chars passthrough", "abcdefg", "abcdefg"},
		{"shorter than 7 chars passthrough", "abc", "abc"},
		{"empty string passthrough", "", ""},
		{"unknown passthrough", "unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shortCommit(tt.input)
			if got != tt.want {
				t.Errorf("shortCommit(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestVersionBannerFormat(t *testing.T) {
	t.Parallel()

	banner := versionBanner()

	for _, want := range []string{"sortie ", "commit:", "built:", "\n"} {
		if !strings.Contains(banner, want) {
			t.Errorf("versionBanner() = %q, want to contain %q", banner, want)
		}
	}
	if !strings.HasSuffix(banner, "\n") {
		t.Errorf("versionBanner() = %q, want trailing newline", banner)
	}
}
