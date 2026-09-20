//go:build unix

package agenttest

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// liveChildrenBound bounds the process-table query so a query that never
// returns cannot hold a program open past its own exit.
const liveChildrenBound = 10 * time.Second

// recordDescendants appends this process's live children's pids to exe's
// descendant receipt. The process writes its own record because a child that
// left its parent's group is traceable only through the parent while alive, and
// an outside watcher would miss a child born between samples.
func recordDescendants(exe string) {
	children := liveChildren(os.Getpid())
	if len(children) == 0 {
		return
	}
	var record strings.Builder
	for _, pid := range children {
		record.WriteString(strconv.Itoa(pid))
		record.WriteByte('\n')
	}
	receipt, err := os.OpenFile(DescendantReceipt(exe), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	// One append under the pipe buffer is atomic, so two programs sharing a
	// receipt cannot interleave.
	_, _ = receipt.WriteString(record.String())
	_ = receipt.Close()
}

func liveChildren(ppid int) []int {
	ctx, cancel := context.WithTimeout(context.Background(), liveChildrenBound)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-Ao", "pid=,ppid=").Output() //nolint:gosec // fixed command and fixed arguments
	if err != nil {
		return nil
	}
	var children []int
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil || parent != ppid || pid == ppid {
			continue
		}
		children = append(children, pid)
	}
	return children
}
