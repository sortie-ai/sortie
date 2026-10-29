package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	geminiCredentialsHomeVar = "GEMINI_CLI_HOME"
	geminiCredentialsLimit   = 64 * 1024

	geminiForceEncryptedVar = "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE"
	geminiForceFileVar      = "GEMINI_FORCE_FILE_STORAGE"
	geminiUseGCAVar         = "GOOGLE_GENAI_USE_GCA"
	geminiAccessTokenVar    = "GOOGLE_CLOUD_ACCESS_TOKEN" //nolint:gosec // G101: a variable name, not a credential

	geminiKeychainService = "gemini-cli-oauth"
	geminiKeychainAccount = "main-account"
)

var (
	geminiCredentialsFile = agentcore.RuntimePath{
		HomeVar:  geminiCredentialsHomeVar,
		Segments: []string{".gemini", "oauth_creds.json"},
	}
	geminiEncryptedFile = agentcore.RuntimePath{
		HomeVar:  geminiCredentialsHomeVar,
		Segments: []string{".gemini", "gemini-credentials.json"},
	}
)

// geminiSignInVars are the variables that decide where the runtime takes its
// sign-in from. The WSL ones make it skip the OS keychain.
var geminiSignInVars = []string{
	geminiUseGCAVar, geminiAccessTokenVar,
	geminiForceEncryptedVar, geminiForceFileVar, "WSL_DISTRO_NAME", "WSLENV", "WSL_INTEROP",
}

// geminiKeychainScript tries libsecret, then the macOS Keychain, because the
// system of a remote host is not known.
var geminiKeychainScript = fmt.Sprintf(
	"secret-tool lookup service %[1]s account %[2]s || security find-generic-password -s %[1]s -a %[2]s -w",
	geminiKeychainService, geminiKeychainAccount)

type geminiToken struct {
	access   string
	expiryMS float64
}

type geminiSignIn struct {
	AccessToken string  `json:"access_token"`
	ExpiryDate  float64 `json:"expiry_date"`
}

// geminiStoredSignIn is the JSON in the runtime's keychain entry and encrypted
// file.
type geminiStoredSignIn struct {
	Token struct {
		AccessToken string  `json:"accessToken"`
		ExpiresAt   float64 `json:"expiresAt"`
	} `json:"token"`
}

// geminiStore looks one runtime store up; found is false when it holds no
// sign-in.
type geminiStore func(ctx context.Context, rt PullRuntime) (token geminiToken, found bool, err error)

// geminiAccessToken returns the access token the runtime uses, or "" when the
// stored one has expired or the sign-in holds none.
//
// The runtime prefers GOOGLE_CLOUD_ACCESS_TOKEN over every store when
// GOOGLE_GENAI_USE_GCA is set, and treats it as never expiring.
func geminiAccessToken(ctx context.Context, rt PullRuntime) (string, error) {
	env, err := geminiHostEnv(ctx, rt, geminiSignInVars)
	if err != nil {
		return "", err
	}
	if env[geminiUseGCAVar] != "" && env[geminiAccessTokenVar] != "" {
		return env[geminiAccessTokenVar], nil
	}

	token, err := geminiStoredToken(ctx, rt, env)
	if err != nil {
		return "", err
	}
	if token.access == "" || !time.UnixMilli(int64(token.expiryMS)).After(time.Now()) {
		return "", nil
	}
	return token.access, nil
}

// geminiStoredToken reads the sign-in from the first store, in the runtime's
// own order, that holds one.
func geminiStoredToken(ctx context.Context, rt PullRuntime, env map[string]string) (geminiToken, error) {
	for _, store := range geminiStoreOrder(env) {
		token, found, err := store(ctx, rt)
		if err != nil {
			return geminiToken{}, err
		}
		if found {
			return token, nil
		}
	}
	return geminiToken{}, fmt.Errorf("gemini sign-in: %w", usage.ErrPullUnsupported)
}

// geminiStoreOrder is the runtime's read order: the plain file alone without
// the force flag; with it, keychain (unless skipped), encrypted file, plain
// file.
//
// The runtime settles a working keychain by writing a probe entry, which the
// pull must not do, so an empty keychain falls through to the encrypted file
// where the runtime would stop.
func geminiStoreOrder(env map[string]string) []geminiStore {
	if env[geminiForceEncryptedVar] != "true" {
		return []geminiStore{geminiPlainFileToken}
	}
	skipsKeychain := env[geminiForceFileVar] == "true" ||
		env["WSL_DISTRO_NAME"] != "" || env["WSLENV"] != "" || env["WSL_INTEROP"] != ""
	if skipsKeychain {
		return []geminiStore{geminiEncryptedFileToken, geminiPlainFileToken}
	}
	return []geminiStore{geminiKeychainToken, geminiEncryptedFileToken, geminiPlainFileToken}
}

// geminiHostEnv returns each named variable as the runtime sees it. A remote
// launch forwards only some variables, so the rest are read from the remote
// shell, where the runtime inherits them, with one command.
func geminiHostEnv(ctx context.Context, rt PullRuntime, names []string) (map[string]string, error) {
	env := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		if value, ok := rt.LaunchEnv(name); ok {
			env[name] = value
		} else {
			missing = append(missing, name)
		}
	}
	if !rt.RemoteHost() || len(missing) == 0 {
		return env, nil
	}

	var script strings.Builder
	script.WriteString("printf '%s\\0'")
	for _, name := range missing {
		fmt.Fprintf(&script, ` "$%s"`, name)
	}
	out, err := rt.RunHostCommand(ctx, agentcore.HostCommand{Name: "sh", Args: []string{"-c", script.String()}})
	if err != nil {
		return nil, fmt.Errorf("read gemini storage settings: %w", err)
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(values) != len(missing) {
		return nil, errors.New("read gemini storage settings: unexpected output")
	}
	for i, name := range missing {
		env[name] = values[i]
	}
	return env, nil
}

func geminiPlainFileToken(ctx context.Context, rt PullRuntime) (geminiToken, bool, error) {
	data, err := rt.ReadHomeFile(ctx, geminiCredentialsFile, geminiCredentialsLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return geminiToken{}, false, nil
	}
	if err != nil {
		return geminiToken{}, false, fmt.Errorf("read gemini sign-in file: %w", err)
	}

	var creds geminiSignIn
	if err := json.Unmarshal(data, &creds); err != nil {
		// The decode error is dropped: it can quote bytes of the file.
		return geminiToken{}, false, errors.New("decode gemini sign-in file")
	}
	return geminiToken{access: creds.AccessToken, expiryMS: creds.ExpiryDate}, true, nil
}

func geminiEncryptedFileToken(ctx context.Context, rt PullRuntime) (geminiToken, bool, error) {
	data, err := rt.ReadHomeFile(ctx, geminiEncryptedFile, geminiCredentialsLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return geminiToken{}, false, nil
	}
	if err != nil {
		return geminiToken{}, false, fmt.Errorf("read gemini encrypted sign-in file: %w", err)
	}

	salt, err := geminiHostSalt(ctx, rt)
	if err != nil {
		return geminiToken{}, false, err
	}
	entries, err := geminiDecryptEntries(data, salt)
	if err != nil {
		return geminiToken{}, false, err
	}
	return geminiEntryToken(entries[geminiKeychainService][geminiKeychainAccount])
}

// geminiKeychainToken reads the runtime's keychain entry. A host with no
// keychain tool, a locked keychain or a missing entry all mean the keychain
// holds no sign-in, as they do for the runtime.
func geminiKeychainToken(ctx context.Context, rt PullRuntime) (geminiToken, bool, error) {
	secret, err := geminiKeychainSecret(ctx, rt)
	if err != nil {
		return geminiToken{}, false, err
	}
	return geminiEntryToken(secret)
}

func geminiKeychainSecret(ctx context.Context, rt PullRuntime) (string, error) {
	if !rt.RemoteHost() && runtime.GOOS == "windows" {
		return geminiCredentialManagerSecret(geminiKeychainService + "/" + geminiKeychainAccount), nil
	}
	out, err := rt.RunHostCommand(ctx, agentcore.HostCommand{Name: "sh", Args: []string{"-c", geminiKeychainScript}})
	if errors.Is(err, agentcore.ErrHostCommandFailed) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read gemini keychain: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// geminiEntryToken decodes one keychain or encrypted-file entry. An empty
// entry is no entry.
func geminiEntryToken(entry string) (geminiToken, bool, error) {
	if entry == "" {
		return geminiToken{}, false, nil
	}
	var stored geminiStoredSignIn
	if err := json.Unmarshal([]byte(entry), &stored); err != nil {
		// The decode error is dropped: it can quote bytes of the entry.
		return geminiToken{}, false, errors.New("decode gemini stored sign-in")
	}
	return geminiToken{access: stored.Token.AccessToken, expiryMS: stored.Token.ExpiresAt}, true, nil
}
