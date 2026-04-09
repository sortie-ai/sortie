//go:build !unix && !windows

package workspace

// allowedEnvKeys is empty on unsupported platforms. Hooks are
// non-functional; only SORTIE_* variables would be inherited.
var allowedEnvKeys = map[string]bool{}
