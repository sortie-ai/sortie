package opencode

import "testing"

func TestParseRuntimeVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		out         string
		truncated   bool
		wantVersion string
		wantMajor   int
		wantOK      bool
	}{
		{name: "1.x bare version", out: "1.18.32\n", wantVersion: "1.18.32", wantMajor: 1, wantOK: true},
		{name: "2.x opencode bin, v-prefixed", out: "opencode v2.0.18\n", wantVersion: "2.0.18", wantMajor: 2, wantOK: true},
		{name: "2.x opencode2 bin, v-prefixed", out: "opencode2 v2.0.18\n", wantVersion: "2.0.18", wantMajor: 2, wantOK: true},
		{name: "2.x prerelease suffix", out: "opencode v2.1.0-beta.3\n", wantVersion: "2.1.0-beta.3", wantMajor: 2, wantOK: true},
		{name: "2.x trailing build metadata token is ignored", out: "opencode 2.0.18 (abc123)\n", wantVersion: "2.0.18", wantMajor: 2, wantOK: true},
		{name: "leading blank lines are skipped", out: "\n\n1.19.0\n", wantVersion: "1.19.0", wantMajor: 1, wantOK: true},
		{name: "an unsupported major still parses", out: "0.0.0-dev-202609250101\n", wantVersion: "0.0.0-dev-202609250101", wantMajor: 0, wantOK: true},
		{name: "major 3 still parses", out: "3.0.0\n", wantVersion: "3.0.0", wantMajor: 3, wantOK: true},
		{name: "empty output is not ok", out: "", wantOK: false},
		{name: "usage text carries no version", out: "Usage: opencode [options]\n", wantOK: false},
		{name: "a two-component number is not a bare semantic version", out: "1.18\n", wantOK: false},
		{name: "truncated output is never ok", out: "1.18.32\n", truncated: true, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, major, ok := parseRuntimeVersion([]byte(tt.out), tt.truncated)

			if ok != tt.wantOK {
				t.Fatalf("parseRuntimeVersion(%q, truncated=%v) ok = %v, want %v", tt.out, tt.truncated, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if version != tt.wantVersion {
				t.Errorf("parseRuntimeVersion(%q) version = %q, want %q", tt.out, version, tt.wantVersion)
			}
			if major != tt.wantMajor {
				t.Errorf("parseRuntimeVersion(%q) major = %d, want %d", tt.out, major, tt.wantMajor)
			}
		})
	}
}

func TestCheckMajorSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pt      passthroughConfig
		major   runtimeMajor
		wantMsg string
	}{
		{name: "major1 never refuses pure", pt: passthroughConfig{Pure: true}, major: major1},
		{name: "major1 never refuses a variant without a model", pt: passthroughConfig{Variant: "thinking"}, major: major1},
		{name: "major2 pass-through with none of the three conditions set", pt: passthroughConfig{Model: "anthropic/claude-3-5-sonnet"}, major: major2},
		{
			name:    "major2 refuses pure",
			pt:      passthroughConfig{Pure: true},
			major:   major2,
			wantMsg: "opencode.pure is not supported by OpenCode 2.x; remove it or install a 1.x release",
		},
		{
			name:    "major2 refuses a variant without a model",
			pt:      passthroughConfig{Variant: "thinking"},
			major:   major2,
			wantMsg: "opencode.variant needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.variant",
		},
		{
			name:    "major2 refuses a variant when the model already names one",
			pt:      passthroughConfig{Model: "anthropic/claude-3-5-sonnet#thinking", Variant: "thinking"},
			major:   major2,
			wantMsg: "opencode.model already names a variant after #; remove that suffix or remove opencode.variant",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkMajorSettings(tt.pt, tt.major)

			if tt.wantMsg == "" {
				if got != nil {
					t.Fatalf("checkMajorSettings() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("checkMajorSettings() = nil, want message %q", tt.wantMsg)
			}
			if got.Message != tt.wantMsg {
				t.Errorf("checkMajorSettings().Message = %q, want %q", got.Message, tt.wantMsg)
			}
		})
	}
}
