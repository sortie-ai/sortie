package redact

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// randomSecret returns a fresh value at least MinValueBytes long that no
// other test in this process has registered, so registrations made by one
// test can never be observed by another sharing the same process-wide
// registry.
func randomSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "secret-" + hex.EncodeToString(buf)
}

func TestIsSecretName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{"TEST_API_KEY", true},
		{"MY_APP_TOKEN", true},
		{"DB_PASSWORD", true},
		{"DB_PASSWD", true},
		{"GH_PAT", true},
		{"SERVICE_CREDENTIAL", true},
		{"SERVICE_CREDENTIALS", true},
		{"PROXY_AUTH", true},
		{"AUTHORIZATION", true},
		{"COOKIE", true},
		{"COOKIE2", true},
		{"CODEX_API_KEY", true},
		{"Authorization", true},
		{"Proxy-Authorization", true},
		{"Cookie", true},
		{"X-Api-Key", true},
		{"MY_PRIVATE_KEY", true},
		{"MY_SSH_KEY", true},
		{"MY_ACCESS_KEY", true},
		{"MY_SECRET_KEY", true},
		{"DB_CONNECTION_STRING", true},
		{"my-connection-string", true},
		{"Accept", false},
		{"SSH_PASS_ENV", false},
		{"SSH_DISALLOW_PASS_ENV", false},
		{"SSH_AUTH_SOCK", false},
		{"EXAMPLE_PROJECT", false},
		{"", false},
		{"_", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSecretName(tt.name); got != tt.want {
				t.Errorf("IsSecretName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestAdd_RegistersMultilineValueAndSchemeCredential(t *testing.T) {
	t.Parallel()

	token := randomSecret(t)
	value := "line one\nBearer " + token + "\nline three"
	Add("test.Add multiline", value)

	text := "before " + value + " after"
	got := Mask(text)
	if strings.Contains(got, token) {
		t.Fatalf("Mask(%q) = %q, still contains the bare token", text, got)
	}
	if !strings.Contains(got, Marker) {
		t.Errorf("Mask(%q) = %q, want it to contain %q", text, got, Marker)
	}

	bareTokenText := "Authorization header carried " + token + " as its credential"
	if got := Mask(bareTokenText); !strings.Contains(got, Marker) || strings.Contains(got, token) {
		t.Errorf("Mask(%q) = %q, want the bare token masked", bareTokenText, got)
	}
}

func TestAdd_DropsShortPieceAndNoByteReachesMask(t *testing.T) {
	t.Parallel()

	short := "abc1234" // 7 bytes, below MinValueBytes
	source := "test.Add short " + randomSecret(t)
	Add(source, short)

	text := "value=" + short
	if got := Mask(text); got != text {
		t.Errorf("Mask(%q) = %q, want unchanged (piece below MinValueBytes must never match)", text, got)
	}
}

func TestAddPieces_WarnsOnceAndNamesSource(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })

	var buf syncBuffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	source := "test.warn-source-" + randomSecretNoRegistration(t)
	const dropped = "qz42vx" // 6 bytes, below MinValueBytes, non-space content
	Add(source, dropped)
	Add(source, dropped) // repeated: still only one WARN for this source

	out := buf.String()
	count := strings.Count(out, "secret value too short to mask")
	if count != 1 {
		t.Fatalf("WARN count for source %q = %d, want 1; log:\n%s", source, count, out)
	}
	if !strings.Contains(out, "name="+source) && !strings.Contains(out, fmt.Sprintf("name=%q", source)) {
		t.Errorf("WARN does not name the source %q; log:\n%s", source, out)
	}
	if !strings.Contains(out, "min_bytes=8") {
		t.Errorf("WARN does not carry min_bytes=8; log:\n%s", out)
	}
	if strings.Contains(out, dropped) {
		t.Errorf("WARN leaked the dropped value itself; log:\n%s", out)
	}
}

func TestAdd_SpaceOnlyValueNeverWarns(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })

	var buf syncBuffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	source := "test.space-only-source-" + randomSecretNoRegistration(t)
	Add(source, "   ")

	if out := buf.String(); strings.Contains(out, "secret value too short to mask") {
		t.Errorf("Add(%q, %q) warned for a value with no non-space content; log:\n%s", source, "   ", out)
	}
}

// randomSecretNoRegistration returns a random suffix without registering
// anything, for building unique source names in tests that must control
// exactly what gets registered.
func randomSecretNoRegistration(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(buf)
}

func TestAddNamed_UsesNameConventionToChooseWholeOrURLCredentials(t *testing.T) {
	t.Parallel()

	t.Run("secret name registers the whole value", func(t *testing.T) {
		t.Parallel()
		value := randomSecret(t)
		AddNamed("test.AddNamed secret", "TEST_API_KEY", value)
		if got := Mask("token=" + value); got == "token="+value {
			t.Errorf("Mask() did not register %q under a secret name", value)
		}
	})

	t.Run("non-secret name registers URL credentials only", func(t *testing.T) {
		t.Parallel()
		user := randomSecret(t)
		pass := randomSecret(t)
		endpoint := "https://" + user + ":" + pass + "@example.com/path"
		AddNamed("test.AddNamed endpoint", "ENDPOINT_URL", endpoint)

		if got := Mask(user + ":" + pass); got == user+":"+pass {
			t.Errorf("Mask(%q) = %q, want the whole userinfo masked", user+":"+pass, got)
		}
		if got := Mask(pass); got == pass {
			t.Errorf("Mask(%q) = %q, want the userinfo password masked", pass, got)
		}
	})
}

func TestAddURLCredentials(t *testing.T) {
	t.Parallel()

	t.Run("scheme with user and password registers the whole userinfo and the password", func(t *testing.T) {
		t.Parallel()
		user := randomSecret(t)
		pass := randomSecret(t)
		value := "https://" + user + ":" + pass + "@proxy.example.com"
		AddURLCredentials("test.AddURLCredentials userpass", value)

		if got := Mask(user + ":" + pass); got == user+":"+pass {
			t.Errorf("Mask(%q) = %q, want the whole userinfo masked", user+":"+pass, got)
		}
		if got := Mask(pass); got == pass {
			t.Errorf("Mask(%q) = %q, want the password masked", pass, got)
		}
	})

	t.Run("scheme with user only registers the username", func(t *testing.T) {
		t.Parallel()
		user := randomSecret(t)
		value := "https://" + user + "@example.com"
		AddURLCredentials("test.AddURLCredentials useronly", value)

		if got := Mask(user); got == user {
			t.Errorf("Mask(%q) = %q, want the username masked", user, got)
		}
	})

	t.Run("no scheme contributes nothing", func(t *testing.T) {
		t.Parallel()
		user := randomSecret(t)
		pass := randomSecret(t)
		value := user + ":" + pass + "@example.com"
		AddURLCredentials("test.AddURLCredentials noscheme", value)

		if got := Mask(user); got != user {
			t.Errorf("Mask(%q) = %q, want unchanged: value carries no \"://\"", user, got)
		}
	})
}

func TestAddEnviron(t *testing.T) {
	t.Parallel()

	keyValue := randomSecret(t)
	plainValue := randomSecret(t)
	AddEnviron([]string{
		"TEST_API_KEY=" + keyValue,
		"EXAMPLE_PROJECT=" + plainValue,
		"NOT_AN_ENTRY_NO_EQUALS_SIGN",
	})

	if got := Mask(keyValue); got == keyValue {
		t.Errorf("AddEnviron did not register TEST_API_KEY's value")
	}
	if got := Mask(plainValue); got != plainValue {
		t.Errorf("AddEnviron registered a value under a name IsSecretName rejects: Mask(%q) = %q", plainValue, got)
	}
}

func TestMask_OverlappingRegisteredValuesMergeIntoOneMarker(t *testing.T) {
	t.Parallel()

	unique := randomSecret(t)
	shared := "shared" + unique
	left := "AAAAAAAA" + shared
	right := shared + "BBBBBBBB"
	Add("test.Mask overlap "+unique, left)
	Add("test.Mask overlap "+unique, right)

	text := "token=" + left + "BBBBBBBB"
	got := Mask(text)
	want := "token=" + Marker
	if got != want {
		t.Errorf("Mask(%q) = %q, want %q", text, got, want)
	}
}

func TestMask_Idempotent(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.Mask idempotent", value)

	text := "credential=" + value + " and again " + value
	once := Mask(text)
	twice := Mask(once)
	if once != twice {
		t.Errorf("Mask(Mask(s)) = %q, want %q (Mask(s))", twice, once)
	}
}

func TestMask_UnregisteredValueUnchanged(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	text := "SORTIE_WORKSPACE=/tmp/" + value
	if got := Mask(text); got != text {
		t.Errorf("Mask(%q) = %q, want unchanged for a value never registered", text, got)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		maxRunes int
		want     string
	}{
		{"empty string", "", 10, ""},
		{"below limit", "hello", 10, "hello"},
		{"exact limit not truncated", "hello", 5, "hello"},
		{"above limit gets ellipsis", "hello world", 5, "hello…"},
		{"multi-byte CJK runes counted correctly", "日本語テスト", 3, "日本語…"},
		{"emoji counted as single rune", "ab🎉cd", 3, "ab🎉…"},
		{"maxRunes zero returns ellipsis", "abc", 0, "…"},
		{"unicode two-byte runes", "héllo", 4, "héll…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Truncate(tt.input, tt.maxRunes)
			if got != tt.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.input, tt.maxRunes, got, tt.want)
			}
		})
	}
}

func TestTruncate_MasksBeforeCuttingNeverLeaksRegisteredValue(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.Truncate cut", value)

	text := "prefix-" + value + "-suffix"
	for _, maxRunes := range []int{5, 9, 15, 100} {
		got := Truncate(text, maxRunes)
		if strings.Contains(got, value) {
			t.Errorf("Truncate(%q, %d) = %q, leaked the registered value", text, maxRunes, got)
		}
	}

	// A cut generous enough to keep the whole marker still contains it,
	// proving masking ran before the cut rather than being skipped.
	got := Truncate(text, 100)
	if !strings.Contains(got, Marker) {
		t.Errorf("Truncate(%q, 100) = %q, want it to contain %q", text, got, Marker)
	}
}

// syncBuffer is a concurrency-safe io.Writer for capturing slog output, per
// this project's logging test convention: a shared handler's writes must be
// serialized in the writer, not assumed serial by the caller.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
