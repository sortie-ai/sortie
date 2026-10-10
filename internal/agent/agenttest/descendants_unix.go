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
	return processChildren()[ppid]
}

// LiveDescendants returns every live process descended from root by parent id,
// root excluded; nil where the platform has no scan or the scan fails.
func LiveDescendants(root int) []int {
	children := processChildren()
	var descendants []int
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		descendants = append(descendants, children[queue[0]]...)
		queue = append(queue, children[queue[0]]...)
	}
	return descendants
}

// processChildren maps each parent id to its live children, in table order.
// The table never lists the reading ps: it is a child of the caller that has
// exited by the time its output is parsed, so listing it would report a
// process that is already gone.
func processChildren() map[int][]int {
	ctx, cancel := context.WithTimeout(context.Background(), liveChildrenBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-Ao", "pid=,ppid=") //nolint:gosec // fixed command and fixed arguments
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	reader := cmd.Process.Pid
	children := make(map[int][]int)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil || pid == parent || pid == reader {
			continue
		}
		children[parent] = append(children[parent], pid)
	}
	return children
}
