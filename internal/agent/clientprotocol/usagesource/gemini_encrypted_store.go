package usagesource

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"

	"golang.org/x/crypto/scrypt"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

// The runtime derives its file key with Node's crypto.scryptSync, whose
// default cost is N=16384, r=8, p=1.
const (
	geminiKeyPassword = "gemini-cli-oauth" //nolint:gosec // G101: the constant the runtime itself derives its file key from
	geminiKeyLen      = 32
	geminiScryptN     = 1 << 14
	geminiScryptR     = 8
	geminiScryptP     = 1

	geminiGCMTagLen = 16
)

var errGeminiEncryptedStore = errors.New("decode gemini encrypted sign-in file")

// geminiHostSalt is the runtime's key salt, built from the runtime host's own
// host and user names, which differ from Sortie's for a remote session.
func geminiHostSalt(ctx context.Context, rt PullRuntime) (string, error) {
	host, name, err := geminiHostNames(ctx, rt)
	if err != nil {
		return "", err
	}
	return host + "-" + name + "-gemini-cli", nil
}

func geminiHostNames(ctx context.Context, rt PullRuntime) (host, name string, err error) {
	if rt.RemoteHost() {
		out, err := rt.RunHostCommand(ctx, agentcore.HostCommand{Name: "sh", Args: []string{"-c", `printf '%s\0%s' "$(uname -n)" "$(id -un)"`}})
		if err != nil {
			return "", "", fmt.Errorf("read gemini host names: %w", err)
		}
		host, name, ok := strings.Cut(string(out), "\x00")
		if !ok {
			return "", "", errors.New("read gemini host names: unexpected output")
		}
		return host, name, nil
	}

	host, err = os.Hostname()
	if err != nil {
		return "", "", fmt.Errorf("read gemini host name: %w", err)
	}
	current, err := user.Current()
	if err != nil {
		return "", "", fmt.Errorf("read gemini user name: %w", err)
	}
	name = current.Username
	if runtime.GOOS == "windows" {
		// Node reports the account name alone, Go prefixes the domain.
		name = name[strings.LastIndex(name, `\`)+1:]
	}
	return host, name, nil
}

// geminiDecryptEntries decrypts the runtime's "iv:tag:ciphertext" hex file
// (AES-256-GCM) into a service-to-account-to-secret map. Every failure returns
// the same error: a wrapped one could quote bytes of the file.
func geminiDecryptEntries(data []byte, salt string) (map[string]map[string]string, error) {
	parts := strings.Split(strings.TrimSpace(string(data)), ":")
	if len(parts) != 3 {
		return nil, errGeminiEncryptedStore
	}
	iv, ivErr := hex.DecodeString(parts[0])
	tag, tagErr := hex.DecodeString(parts[1])
	body, bodyErr := hex.DecodeString(parts[2])
	if errors.Join(ivErr, tagErr, bodyErr) != nil || (len(iv) != 12 && len(iv) != 16) || len(tag) != geminiGCMTagLen {
		return nil, errGeminiEncryptedStore
	}

	key, err := scrypt.Key([]byte(geminiKeyPassword), []byte(salt), geminiScryptN, geminiScryptR, geminiScryptP, geminiKeyLen)
	if err != nil {
		return nil, errGeminiEncryptedStore
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errGeminiEncryptedStore
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return nil, errGeminiEncryptedStore
	}
	plain, err := gcm.Open(nil, iv, append(body, tag...), nil)
	if err != nil {
		return nil, errGeminiEncryptedStore
	}

	var entries map[string]map[string]string
	if err := json.Unmarshal(plain, &entries); err != nil {
		return nil, errGeminiEncryptedStore
	}
	return entries, nil
}
