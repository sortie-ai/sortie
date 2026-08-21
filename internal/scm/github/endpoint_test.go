package github

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- helpers ---

// credentialEndpoint is a malformed endpoint carrying userinfo. url.Parse
// rejects the unbracketed IPv6 host, and its error text quotes the whole
// value, credential included, which is the disclosure the constructors
// must not reproduce.
const (
	credentialEndpoint = "https://operator:s3cr3t@fd00::1:3000/api/v3"
	credentialUser     = "operator"
	credentialSecret   = "s3cr3t"
	credentialHost     = "fd00::1:3000"
)

// assertUserinfoRedacted asserts that message names the endpoint host but
// neither half of the userinfo pair, so a credential embedded in a rejected
// endpoint cannot reach an operator through an adapter error.
func assertUserinfoRedacted(t *testing.T, message string) {
	t.Helper()
	if !strings.Contains(message, credentialHost) {
		t.Errorf("message = %q, want to contain %q", message, credentialHost)
	}
	for _, secret := range []string{credentialUser, credentialSecret} {
		if strings.Contains(message, secret) {
			t.Errorf("message = %q, must not contain %q", message, secret)
		}
	}
}

// --- Tests ---

// TestResolveEndpoint pins the shapes the GitHub adapters accept as a base
// URL. The unbracketed IPv6 cases are the ones url.Parse rejects for having
// more than one colon in an unbracketed host: an operator who copies an
// address out of "ip addr" without brackets must be told so at
// configuration time, not at the first request.
func TestResolveEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          string
		wantEndpoint string
		wantOK       bool
	}{
		{name: "empty applies the public API default", raw: "", wantEndpoint: defaultEndpoint, wantOK: true},
		{name: "whitespace only applies the public API default", raw: "   ", wantEndpoint: defaultEndpoint, wantOK: true},
		{name: "https host", raw: "https://api.github.com", wantEndpoint: "https://api.github.com", wantOK: true},
		{name: "trailing slash trimmed", raw: "https://api.github.com/", wantEndpoint: "https://api.github.com", wantOK: true},
		{name: "repeated trailing slashes trimmed", raw: "https://github.example.com/api/v3//", wantEndpoint: "https://github.example.com/api/v3", wantOK: true},
		{name: "surrounding whitespace trimmed", raw: "  https://api.github.com  ", wantEndpoint: "https://api.github.com", wantOK: true},
		{name: "ghes subpath", raw: "https://github.example.com/api/v3", wantEndpoint: "https://github.example.com/api/v3", wantOK: true},
		{name: "custom port", raw: "https://github.example.com:8443", wantEndpoint: "https://github.example.com:8443", wantOK: true},
		{name: "bracketed IPv6 literal", raw: "https://[fd00::1]:3000", wantEndpoint: "https://[fd00::1]:3000", wantOK: true},
		{name: "http scheme", raw: "http://github.example.com", wantEndpoint: "http://github.example.com", wantOK: true},
		{name: "unbracketed IPv6 literal with port", raw: "http://fd00::1:3000"},
		{name: "unbracketed IPv6 loopback", raw: "http://::1/"},
		{name: "doubled port", raw: "http://github.example.com:80:80/"},
		{name: "no scheme", raw: "github.example.com"},
		{name: "unsupported scheme", raw: "ftp://github.example.com"},
		{name: "scheme without host", raw: "https://"},
		{name: "missing scheme before authority", raw: "://github.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			endpoint, _, ok := resolveEndpoint(tt.raw)

			if ok != tt.wantOK {
				t.Fatalf("resolveEndpoint(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if endpoint != tt.wantEndpoint {
				t.Errorf("resolveEndpoint(%q) endpoint = %q, want %q", tt.raw, endpoint, tt.wantEndpoint)
			}
		})
	}
}

// TestResolveEndpointRedactsUserinfo pins that the redacted form drops both
// halves of a userinfo pair. url.Parse quotes the whole raw URL in its error
// text, so the redacted value is the only form any caller may publish.
func TestResolveEndpointRedactsUserinfo(t *testing.T) {
	t.Parallel()

	_, redacted, ok := resolveEndpoint(credentialEndpoint)

	if ok {
		t.Fatal("resolveEndpoint(unbracketed IPv6 with userinfo) ok = true, want false")
	}
	assertUserinfoRedacted(t, redacted)
}

// TestNewGitHubAdapter_MalformedEndpoint asserts the tracker constructor
// rejects a malformed endpoint as a payload fault instead of handing it to
// the HTTP client, where the same value would surface as a transport error.
func TestNewGitHubAdapter_MalformedEndpoint(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"http://fd00::1:3000", "github.example.com", "ftp://github.example.com", "https://"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()

			a, err := NewGitHubAdapter(validConfig(endpoint))

			assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			if a != nil {
				t.Error("adapter should be nil on error")
			}
		})
	}
}

// TestNewGitHubAdapter_MalformedEndpointRedactsUserinfo asserts the
// credential embedded in a rejected endpoint never reaches the operator
// through the tracker error.
func TestNewGitHubAdapter_MalformedEndpointRedactsUserinfo(t *testing.T) {
	t.Parallel()

	_, err := NewGitHubAdapter(validConfig(credentialEndpoint))

	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	assertUserinfoRedacted(t, err.Error())
}

// TestNewGitHubCIProvider_MalformedEndpoint asserts the CI provider applies
// the same guard, with a payload fault of its own error family.
func TestNewGitHubCIProvider_MalformedEndpoint(t *testing.T) {
	t.Parallel()

	p, err := NewGitHubCIProvider(0, map[string]any{
		"api_key":  "test-token",
		"project":  "owner/repo",
		"endpoint": credentialEndpoint,
	})

	assertCIErrorKind(t, err, domain.ErrCIPayload)
	if p != nil {
		t.Error("provider should be nil on error")
	}
	assertUserinfoRedacted(t, err.Error())
}

// TestNewGitHubSCMAdapter_MalformedEndpoint asserts the SCM adapter applies
// the same guard. It reads pull request reviews through the same endpoint
// value, so leaving it unguarded would keep the disclosure reachable.
func TestNewGitHubSCMAdapter_MalformedEndpoint(t *testing.T) {
	t.Parallel()

	a, err := NewGitHubSCMAdapter(map[string]any{
		"api_key":  "test-token",
		"endpoint": credentialEndpoint,
	})

	assertSCMErrorKind(t, err, domain.ErrSCMPayload)
	if a != nil {
		t.Error("adapter should be nil on error")
	}
	assertUserinfoRedacted(t, err.Error())
}
