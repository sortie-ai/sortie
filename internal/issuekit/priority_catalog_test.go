package issuekit

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestWarnUnresolvedPriorityLabels(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	WarnUnresolvedPriorityLabels(log, map[string]int{"p1": 1, "missing-priority": 2}, []string{"P1"})
	output := buf.String()
	if got := strings.Count(output, "priority_labels label does not resolve against repository labels"); got != 1 || !strings.Contains(output, "label=missing-priority") || strings.Contains(output, "label=p1") {
		t.Errorf("WarnUnresolvedPriorityLabels() log = %q, want one warning for missing-priority only", output)
	}
}
