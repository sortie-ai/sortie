//go:build unix

package fakemodel

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// livenessChecked reports that remainingRuntime probes the process table.
const livenessChecked = true

// remainingRuntime names what of pid and its process group still answers
// signal 0, and is empty when neither does. A zombie answers, and so does a
// group the caller may not signal, so both read as remaining. The caller
// guarantees pid is above 1: signal 0 to group 0 or to every process would
// answer for processes that are not the runtime's.
func remainingRuntime(pid int) string {
	var remaining []string
	if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		remaining = append(remaining, fmt.Sprintf("process %d", pid))
	}
	if !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
		remaining = append(remaining, fmt.Sprintf("process group %d", pid))
	}
	return strings.Join(remaining, " and ")
}
