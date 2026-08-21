package httpkit

import (
	"strings"
	"testing"
)

// --- Test helpers ---

// assertFailureShape asserts the fail-safe zero-value contract of a rejected
// [Endpoint]: every field except Redacted is empty, so a caller that ignores
// the ok result cannot find a usable-looking Base.
func assertFailureShape(t *testing.T, raw string, got Endpoint) {
	t.Helper()

	if got.Base != "" {
		t.Errorf("ParseEndpoint(%q) Base = %q, want empty on failure", raw, got.Base)
	}
	if got.Host != "" {
		t.Errorf("ParseEndpoint(%q) Host = %q, want empty on failure", raw, got.Host)
	}
	if got.Scheme != "" {
		t.Errorf("ParseEndpoint(%q) Scheme = %q, want empty on failure", raw, got.Scheme)
	}
	if got.Path != "" {
		t.Errorf("ParseEndpoint(%q) Path = %q, want empty on failure", raw, got.Path)
	}
}

// --- Tests ---

func TestParseEndpoint_Rejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{"empty string", ""},
		{"whitespace-only", "   "},
		{"bare host with no scheme", "github.example.com"},
		{"non-http scheme", "ftp://github.example.com"},
		{"scheme with no host", "https://"},
		{"unbracketed IPv6 with port", "http://fd00::1:3000"},
		{"unbracketed IPv6 loopback", "http://::1/"},
		{"doubled port", "http://host:80:80/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseEndpoint(tt.raw)

			if ok {
				t.Fatalf("ParseEndpoint(%q) ok = true, want false", tt.raw)
			}
			assertFailureShape(t, tt.raw, got)
		})
	}
}

func TestParseEndpoint_Accepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantBase   string
		wantHost   string
		wantScheme string
		wantPath   string
	}{
		{
			name:       "plain https host",
			raw:        "https://api.github.com",
			wantBase:   "https://api.github.com",
			wantHost:   "api.github.com",
			wantScheme: "https",
			wantPath:   "",
		},
		{
			name:       "trailing slash trimmed",
			raw:        "https://api.github.com/",
			wantBase:   "https://api.github.com",
			wantHost:   "api.github.com",
			wantScheme: "https",
			wantPath:   "/",
		},
		{
			name:       "doubled trailing slash",
			raw:        "https://host/api/v3//",
			wantBase:   "https://host/api/v3",
			wantHost:   "host",
			wantScheme: "https",
			wantPath:   "/api/v3//",
		},
		{
			name:       "surrounding whitespace trimmed",
			raw:        "  https://api.github.com  ",
			wantBase:   "https://api.github.com",
			wantHost:   "api.github.com",
			wantScheme: "https",
			wantPath:   "",
		},
		{
			name:       "custom port",
			raw:        "https://api.github.com:8443",
			wantBase:   "https://api.github.com:8443",
			wantHost:   "api.github.com",
			wantScheme: "https",
			wantPath:   "",
		},
		{
			name:       "bracketed IPv6",
			raw:        "https://[fd00::1]:3000",
			wantBase:   "https://[fd00::1]:3000",
			wantHost:   "fd00::1",
			wantScheme: "https",
			wantPath:   "",
		},
		{
			name:       "uppercase scheme lowercased by url.Parse",
			raw:        "HTTPS://api.github.com",
			wantBase:   "HTTPS://api.github.com",
			wantHost:   "api.github.com",
			wantScheme: "https",
			wantPath:   "",
		},
		{
			name:       "http scheme preserved",
			raw:        "http://api.github.com",
			wantBase:   "http://api.github.com",
			wantHost:   "api.github.com",
			wantScheme: "http",
			wantPath:   "",
		},
		{
			name:       "subpath value carried through",
			raw:        "https://api.github.com/some/sub/path",
			wantBase:   "https://api.github.com/some/sub/path",
			wantHost:   "api.github.com",
			wantScheme: "https",
			wantPath:   "/some/sub/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseEndpoint(tt.raw)

			if !ok {
				t.Fatalf("ParseEndpoint(%q) ok = false, want true", tt.raw)
			}
			if got.Base != tt.wantBase {
				t.Errorf("ParseEndpoint(%q) Base = %q, want %q", tt.raw, got.Base, tt.wantBase)
			}
			if got.Host != tt.wantHost {
				t.Errorf("ParseEndpoint(%q) Host = %q, want %q", tt.raw, got.Host, tt.wantHost)
			}
			if got.Scheme != tt.wantScheme {
				t.Errorf("ParseEndpoint(%q) Scheme = %q, want %q", tt.raw, got.Scheme, tt.wantScheme)
			}
			if got.Path != tt.wantPath {
				t.Errorf("ParseEndpoint(%q) Path = %q, want %q", tt.raw, got.Path, tt.wantPath)
			}
		})
	}
}

func TestParseEndpoint_RedactsCredentialsOnFailure(t *testing.T) {
	t.Parallel()

	const raw = "https://operator:s3cr3t@fd00::1:3000/api/v3"

	got, ok := ParseEndpoint(raw)

	if ok {
		t.Fatalf("ParseEndpoint(%q) ok = true, want false", raw)
	}
	assertFailureShape(t, raw, got)
	if !strings.Contains(got.Redacted, "fd00::1:3000") {
		t.Errorf("ParseEndpoint(%q) Redacted = %q, want it to still name the host", raw, got.Redacted)
	}
	if strings.Contains(got.Redacted, "operator") {
		t.Errorf("ParseEndpoint(%q) Redacted = %q, leaks the username", raw, got.Redacted)
	}
	if strings.Contains(got.Redacted, "s3cr3t") {
		t.Errorf("ParseEndpoint(%q) Redacted = %q, leaks the password", raw, got.Redacted)
	}
}

func TestResolveEndpoint(t *testing.T) {
	t.Parallel()

	const defaultBase = "https://api.example.com/graphql"

	t.Run("empty raw uses defaultBase", func(t *testing.T) {
		t.Parallel()

		got, ok := ResolveEndpoint("", defaultBase)

		if !ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = false, want true", "", defaultBase)
		}
		if got.Base != defaultBase {
			t.Errorf("ResolveEndpoint(%q, %q) Base = %q, want %q", "", defaultBase, got.Base, defaultBase)
		}
		if got.Host != "api.example.com" {
			t.Errorf("ResolveEndpoint(%q, %q) Host = %q, want %q", "", defaultBase, got.Host, "api.example.com")
		}
		if got.Scheme != "https" {
			t.Errorf("ResolveEndpoint(%q, %q) Scheme = %q, want %q", "", defaultBase, got.Scheme, "https")
		}
		if got.Path != "/graphql" {
			t.Errorf("ResolveEndpoint(%q, %q) Path = %q, want %q", "", defaultBase, got.Path, "/graphql")
		}
	})

	t.Run("whitespace-only raw uses defaultBase", func(t *testing.T) {
		t.Parallel()

		got, ok := ResolveEndpoint("   ", defaultBase)

		if !ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = false, want true", "   ", defaultBase)
		}
		if got.Base != defaultBase {
			t.Errorf("ResolveEndpoint(%q, %q) Base = %q, want %q", "   ", defaultBase, got.Base, defaultBase)
		}
	})

	t.Run("non-empty raw overrides defaultBase", func(t *testing.T) {
		t.Parallel()

		const raw = "https://self-hosted.example.com/api"

		got, ok := ResolveEndpoint(raw, defaultBase)

		if !ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = false, want true", raw, defaultBase)
		}
		if got.Base != raw {
			t.Errorf("ResolveEndpoint(%q, %q) Base = %q, want %q", raw, defaultBase, got.Base, raw)
		}
	})

	t.Run("raw with surrounding whitespace is trimmed then parsed", func(t *testing.T) {
		t.Parallel()

		const raw = "  https://self-hosted.example.com/api  "
		const want = "https://self-hosted.example.com/api"

		got, ok := ResolveEndpoint(raw, defaultBase)

		if !ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = false, want true", raw, defaultBase)
		}
		if got.Base != want {
			t.Errorf("ResolveEndpoint(%q, %q) Base = %q, want %q", raw, defaultBase, got.Base, want)
		}
	})

	t.Run("malformed non-empty raw does not fall back to defaultBase", func(t *testing.T) {
		t.Parallel()

		const raw = "ftp://self-hosted.example.com/api"

		got, ok := ResolveEndpoint(raw, defaultBase)

		if ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = true, want false", raw, defaultBase)
		}
		if got.Base == defaultBase {
			t.Errorf("ResolveEndpoint(%q, %q) Base = %q, must not silently fall back to defaultBase", raw, defaultBase, got.Base)
		}
		assertFailureShape(t, raw, got)
	})

	t.Run("malformed raw carrying userinfo redacts credentials", func(t *testing.T) {
		t.Parallel()

		const raw = "https://operator:s3cr3t@fd00::1:3000/api"

		got, ok := ResolveEndpoint(raw, defaultBase)

		if ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = true, want false", raw, defaultBase)
		}
		if strings.Contains(got.Redacted, "operator") || strings.Contains(got.Redacted, "s3cr3t") {
			t.Errorf("ResolveEndpoint(%q, %q) Redacted = %q, leaks credentials", raw, defaultBase, got.Redacted)
		}
		if !strings.Contains(got.Redacted, "fd00::1:3000") {
			t.Errorf("ResolveEndpoint(%q, %q) Redacted = %q, want it to still name the host", raw, defaultBase, got.Redacted)
		}
	})

	t.Run("bad defaultBase with empty raw returns false", func(t *testing.T) {
		t.Parallel()

		const badDefault = "not a url at all"

		got, ok := ResolveEndpoint("", badDefault)

		if ok {
			t.Fatalf("ResolveEndpoint(%q, %q) ok = true, want false", "", badDefault)
		}
		assertFailureShape(t, badDefault, got)
	})
}
