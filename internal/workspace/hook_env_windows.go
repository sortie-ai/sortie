//go:build windows

package workspace

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
