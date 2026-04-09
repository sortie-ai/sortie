//go:build !unix && !windows

package workspace

import (
	"context"
	"errors"
)

// RunHook returns an error on unsupported platforms. Hook scripts
// require a platform-specific shell invocation.
func RunHook(_ context.Context, params HookParams) (HookResult, error) {
	return HookResult{}, &HookError{
		Op:       "start",
		Script:   truncateScript(params.Script),
		ExitCode: -1,
		Err:      errors.New("hooks are not supported on this platform"),
	}
}
