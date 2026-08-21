package linear

import (
	"strings"
	"testing"
)

func TestResolveEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          string
		wantEndpoint string
		wantOK       bool
	}{
		{
			name:         "absent value defaults to the hosted graphql host",
			raw:          "",
			wantEndpoint: defaultEndpoint,
			wantOK:       true,
		},
		{
			name:         "whitespace-only value defaults to the hosted graphql host",
			raw:          "   ",
			wantEndpoint: defaultEndpoint,
			wantOK:       true,
		},
		{
			name:         "valid custom endpoint resolves to the trimmed base",
			raw:          "  https://self-hosted.example.com/graphql  ",
			wantEndpoint: "https://self-hosted.example.com/graphql",
			wantOK:       true,
		},
		{
			name:   "unsupported scheme is rejected",
			raw:    "ftp://example.com/graphql",
			wantOK: false,
		},
		{
			name:   "non-url string is rejected",
			raw:    "not a url at all",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotEndpoint, _, gotOK := resolveEndpoint(tt.raw)

			if gotOK != tt.wantOK {
				t.Fatalf("resolveEndpoint(%q) ok = %v, want %v", tt.raw, gotOK, tt.wantOK)
			}
			if !tt.wantOK {
				if gotEndpoint != "" {
					t.Errorf("resolveEndpoint(%q) endpoint = %q, want empty on failure", tt.raw, gotEndpoint)
				}
				return
			}
			if gotEndpoint != tt.wantEndpoint {
				t.Errorf("resolveEndpoint(%q) endpoint = %q, want %q", tt.raw, gotEndpoint, tt.wantEndpoint)
			}
		})
	}
}

func TestResolveEndpoint_RedactsCredentialsOnFailure(t *testing.T) {
	t.Parallel()

	const raw = "https://operator:s3cr3t@fd00::1:3000/graphql"

	endpoint, redacted, ok := resolveEndpoint(raw)

	if ok {
		t.Fatalf("resolveEndpoint(%q) ok = true, want false", raw)
	}
	if endpoint != "" {
		t.Errorf("resolveEndpoint(%q) endpoint = %q, want empty on failure", raw, endpoint)
	}
	if strings.Contains(redacted, "operator") || strings.Contains(redacted, "s3cr3t") {
		t.Errorf("resolveEndpoint(%q) redacted = %q, leaks credentials", raw, redacted)
	}
	if !strings.Contains(redacted, "fd00::1:3000") {
		t.Errorf("resolveEndpoint(%q) redacted = %q, want it to still name the host", raw, redacted)
	}
}
