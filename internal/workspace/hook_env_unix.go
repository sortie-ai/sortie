//go:build unix

package workspace

// allowedEnvKeys lists the parent-process environment variables that
// hook subprocesses are permitted to inherit. All other variables are
// stripped so that secrets are not present in the hook subprocess
// environment unless explicitly injected via SORTIE_* prefixed vars.
var allowedEnvKeys = map[string]bool{
	"PATH":          true,
	"HOME":          true,
	"SHELL":         true,
	"TMPDIR":        true,
	"USER":          true,
	"LOGNAME":       true,
	"TERM":          true,
	"LANG":          true,
	"LC_ALL":        true,
	"SSH_AUTH_SOCK": true,
}
