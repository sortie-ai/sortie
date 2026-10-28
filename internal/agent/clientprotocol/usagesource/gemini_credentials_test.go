package usagesource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

const (
	geminiFixtureToken = "ya29.fixture-fake-token"
	geminiFixtureHost  = "fixture-host"
	geminiFixtureUser  = "fixture-user"
)

// geminiStoreRuntime is a remote runtime so the host and user names the
// encrypted file is salted with come from the fake, not from the test machine.
type geminiStoreRuntime struct {
	geminiRuntime
	env      map[string]string
	files    map[string]string
	keychain string
	host     string
	user     string
}

func (r geminiStoreRuntime) LaunchEnv(name string) (string, bool) { return r.env[name], true }

func (geminiStoreRuntime) RemoteHost() bool { return true }

func (r geminiStoreRuntime) ReadHomeFile(_ context.Context, _, rel string, _ int64) ([]byte, error) {
	content, ok := r.files[rel]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func (r geminiStoreRuntime) ScanHomeFile(_ context.Context, _, rel string, _, _ int64, fn func(line []byte) error) error {
	content, ok := r.files[rel]
	if !ok {
		return fs.ErrNotExist
	}
	for line := range strings.Lines(content) {
		if err := fn([]byte(strings.TrimRight(line, "\r\n"))); err != nil {
			return err
		}
	}
	return nil
}

func (r geminiStoreRuntime) RunHostCommand(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch script := args[len(args)-1]; {
	case script == geminiKeychainScript && r.keychain == "":
		return nil, agentcore.ErrHostCommandFailed
	case script == geminiKeychainScript:
		return []byte(r.keychain + "\n"), nil
	case strings.Contains(script, "uname -n"):
		return []byte(r.host + "\x00" + r.user), nil
	}
	return nil, errors.New("unexpected host command")
}

func readGeminiFixture(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(data)
}

func geminiStoredEntry(token string, expiry time.Time) string {
	return fmt.Sprintf(`{"token":{"accessToken":%q,"expiresAt":%d}}`, token, expiry.UnixMilli())
}

func flipGeminiHex(data string, part int) string {
	parts := strings.Split(data, ":")
	digit := []byte(parts[part])
	digit[0] = '0' + (digit[0]-'0'+1)%2
	parts[part] = string(digit)
	return strings.Join(parts, ":")
}

func TestGeminiAccessTokenFollowsTheRuntimesStoreOrder(t *testing.T) {
	t.Parallel()

	valid := time.Now().Add(time.Hour)
	encrypted := readGeminiFixture(t, "gemini_credentials_encrypted.txt")
	tests := []struct {
		name        string
		env         map[string]string
		noKeychain  bool
		noEncrypted bool
		want        string
	}{
		{"plain file when encrypted storage is not forced", nil, false, false, geminiTestToken},
		{"keychain first when encrypted storage is forced", map[string]string{geminiForceEncryptedVar: "true"}, false, false, "keychain-token"},
		{"encrypted file when the keychain has no entry", map[string]string{geminiForceEncryptedVar: "true"}, true, false, geminiFixtureToken},
		{"encrypted file when file storage is forced", map[string]string{geminiForceEncryptedVar: "true", geminiForceFileVar: "true"}, false, false, geminiFixtureToken},
		{"encrypted file under WSL", map[string]string{geminiForceEncryptedVar: "true", "WSL_DISTRO_NAME": "Ubuntu"}, false, false, geminiFixtureToken},
		{"plain file when keychain and encrypted file are empty", map[string]string{geminiForceEncryptedVar: "true"}, true, true, geminiTestToken},
		{"environment token before every store", map[string]string{geminiForceEncryptedVar: "true", geminiUseGCAVar: "true", geminiAccessTokenVar: "env-token"}, false, false, "env-token"},
		{"environment token ignored without the GCA switch", map[string]string{geminiAccessTokenVar: "env-token"}, false, false, geminiTestToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := geminiStoreRuntime{
				env:      tt.env,
				files:    map[string]string{geminiCredentialsFile: testSignInFile(valid)},
				keychain: geminiStoredEntry("keychain-token", valid),
				host:     geminiFixtureHost,
				user:     geminiFixtureUser,
			}
			if tt.noKeychain {
				rt.keychain = ""
			}
			if !tt.noEncrypted {
				rt.files[geminiEncryptedFile] = encrypted
			}

			got, err := geminiAccessToken(t.Context(), rt)

			if err != nil {
				t.Fatalf("geminiAccessToken(%v) error = %v, want nil", tt.env, err)
			}
			if got != tt.want {
				t.Errorf("geminiAccessToken(%v) = %q, want %q", tt.env, got, tt.want)
			}
		})
	}
}

func TestGeminiEncryptedFileYieldsTheTokenOnlyForItsOwnHost(t *testing.T) {
	t.Parallel()

	encrypted := readGeminiFixture(t, "gemini_credentials_encrypted.txt")
	tests := []struct {
		name    string
		host    string
		user    string
		file    string
		want    string
		wantErr bool
	}{
		{"fixture host and user", geminiFixtureHost, geminiFixtureUser, encrypted, geminiFixtureToken, false},
		{"other host name", "other-host", geminiFixtureUser, encrypted, "", true},
		{"flipped tag", geminiFixtureHost, geminiFixtureUser, flipGeminiHex(encrypted, 1), "", true},
		{"flipped ciphertext", geminiFixtureHost, geminiFixtureUser, flipGeminiHex(encrypted, 2), "", true},
		{"not an encrypted file", geminiFixtureHost, geminiFixtureUser, "oauth credentials in the clear", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := geminiStoreRuntime{
				env:   map[string]string{geminiForceEncryptedVar: "true", geminiForceFileVar: "true"},
				files: map[string]string{geminiEncryptedFile: tt.file},
				host:  tt.host,
				user:  tt.user,
			}

			got, err := geminiAccessToken(t.Context(), rt)

			if gotErr := errors.Is(err, errGeminiEncryptedStore); gotErr != tt.wantErr {
				t.Errorf("geminiAccessToken(%s) error = %v, want errGeminiEncryptedStore = %v", tt.name, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("geminiAccessToken(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestGeminiPullSendsNothingWithAnExpiredStoredToken(t *testing.T) {
	t.Parallel()

	expired := time.Now().Add(-time.Minute)
	valid := time.Now().Add(time.Hour)
	tests := []struct {
		name     string
		env      map[string]string
		keychain string
		file     string
		wantAuth string
	}{
		{"expired keychain entry", map[string]string{geminiForceEncryptedVar: "true"}, geminiStoredEntry("keychain-token", expired), "", ""},
		{"expired encrypted file", map[string]string{geminiForceEncryptedVar: "true", geminiForceFileVar: "true"}, "", "gemini_credentials_encrypted_expired.txt", ""},
		{"valid keychain entry", map[string]string{geminiForceEncryptedVar: "true"}, geminiStoredEntry("keychain-token", valid), "", "Bearer keychain-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var auth atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth.Store(r.Header.Get("Authorization"))
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			rt := geminiStoreRuntime{
				env:      map[string]string{"CODE_ASSIST_ENDPOINT": srv.URL, "GOOGLE_CLOUD_PROJECT": "p"},
				files:    map[string]string{},
				keychain: tt.keychain,
				host:     geminiFixtureHost,
				user:     geminiFixtureUser,
			}
			maps.Copy(rt.env, tt.env)
			if tt.file != "" {
				rt.files[geminiEncryptedFile] = readGeminiFixture(t, tt.file)
			}

			readings, err := geminiPullWindows(t.Context(), rt)

			gotAuth, _ := auth.Load().(string)
			if err != nil || readings != nil {
				t.Errorf("geminiPullWindows(%s) = %v, %v, want nil, nil", tt.name, readings, err)
			}
			if gotAuth != tt.wantAuth {
				t.Errorf("geminiPullWindows(%s) sent Authorization %q, want %q", tt.name, gotAuth, tt.wantAuth)
			}
		})
	}
}
