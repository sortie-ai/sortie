//go:build windows

package workspace

import "strings"

// normalizeEnvKey uppercases the key on Windows where environment
// variable names are case-insensitive. The allowedEnvKeys map uses
// uppercase keys, so normalizing before lookup ensures that system
// variables like "Path" match "PATH".
func normalizeEnvKey(key string) string { return strings.ToUpper(key) }

// allowedEnvKeys lists the parent-process environment variables that
// hook subprocesses are permitted to inherit on Windows. All other
// variables are stripped so that secrets are not present in the hook
// subprocess environment unless explicitly injected via SORTIE_*
// prefixed vars.
var allowedEnvKeys = map[string]bool{
	"PATH":         true,
	"SYSTEMROOT":   true,
	"COMSPEC":      true,
	"PATHEXT":      true,
	"USERPROFILE":  true,
	"TEMP":         true,
	"TMP":          true,
	"APPDATA":      true,
	"LOCALAPPDATA": true,
	"HOMEDRIVE":    true,
	"HOMEPATH":     true,
	"USERNAME":     true,
}
