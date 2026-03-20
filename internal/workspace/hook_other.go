//go:build !unix

package workspace

import (
	"context"
	"errors"
)

// RunHook is not supported on non-POSIX platforms. Hook scripts
// require "sh -c" which is inherently POSIX-scoped.
func RunHook(_ context.Context, params HookParams) (HookResult, error) {
	return HookResult{}, &HookError{
		Op:       "start",
		Script:   truncateScript(params.Script),
		ExitCode: -1,
		Err:      errors.New("hooks require a POSIX system"),
	}
}
