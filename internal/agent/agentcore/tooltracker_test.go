package agentcore

import (
	"testing"
	"time"
)

func TestToolTracker_BeginEnd_Roundtrip(t *testing.T) {
	t.Parallel()

	tracker := NewToolTracker()
	tracker.Begin("id-1", "Read")

	name, durationMS, ok := tracker.End("id-1")
	if !ok {
		t.Fatal("End(\"id-1\") ok = false, want true")
	}
	if name != "Read" {
		t.Errorf("End(\"id-1\") name = %q, want %q", name, "Read")
	}
	if durationMS < 0 {
		t.Errorf("End(\"id-1\") durationMS = %d, want >= 0", durationMS)
	}
}

func TestToolTracker_End_UnknownID(t *testing.T) {
	t.Parallel()

	tracker := NewToolTracker()

	name, durationMS, ok := tracker.End("nonexistent")
	if ok {
		t.Fatal("End(\"nonexistent\") ok = true, want false")
	}
	if name != "" {
		t.Errorf("End(\"nonexistent\") name = %q, want \"\"", name)
	}
	if durationMS != 0 {
		t.Errorf("End(\"nonexistent\") durationMS = %d, want 0", durationMS)
	}
}

func TestToolTracker_Begin_Overwrite(t *testing.T) {
	t.Parallel()

	tracker := NewToolTracker()
	tracker.Begin("id-x", "OldTool")
	tracker.Begin("id-x", "NewTool")

	name, _, ok := tracker.End("id-x")
	if !ok {
		t.Fatal("End(\"id-x\") ok = false, want true")
	}
	if name != "NewTool" {
		t.Errorf("End(\"id-x\") name = %q, want %q", name, "NewTool")
	}
}

func TestToolTracker_End_Idempotency(t *testing.T) {
	t.Parallel()

	tracker := NewToolTracker()
	tracker.Begin("id-once", "Bash")

	_, _, first := tracker.End("id-once")
	if !first {
		t.Fatal("first End(\"id-once\") ok = false, want true")
	}

	_, durationMS, second := tracker.End("id-once")
	if second {
		t.Error("second End(\"id-once\") ok = true, want false (idempotent)")
	}
	if durationMS != 0 {
		t.Errorf("second End durationMS = %d, want 0", durationMS)
	}
}

func TestToolTracker_MultipleEntries(t *testing.T) {
	t.Parallel()

	tracker := NewToolTracker()
	tracker.Begin("tool-a", "ToolA")
	tracker.Begin("tool-b", "ToolB")
	tracker.Begin("tool-c", "ToolC")

	tests := []struct {
		id       string
		wantName string
	}{
		{"tool-a", "ToolA"},
		{"tool-b", "ToolB"},
		{"tool-c", "ToolC"},
	}

	// Subtests are NOT parallel: ToolTracker is not goroutine-safe and all
	// subtests share the same tracker instance.
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			name, _, ok := tracker.End(tt.id)
			if !ok {
				t.Fatalf("End(%q) ok = false, want true", tt.id)
			}
			if name != tt.wantName {
				t.Errorf("End(%q) name = %q, want %q", tt.id, name, tt.wantName)
			}
		})
	}
}

func TestToolTracker_DurationClamp(t *testing.T) {
	t.Parallel()

	// Set ts to a future time so time.Since returns a negative duration,
	// exercising the clamp-to-zero branch in End.
	tracker := NewToolTracker()
	tracker.entries["id-future"] = toolEntry{
		name: "Bash",
		ts:   time.Now().Add(1 * time.Hour),
	}

	name, durationMS, ok := tracker.End("id-future")
	if !ok {
		t.Fatal("End(\"id-future\") ok = false, want true")
	}
	if name != "Bash" {
		t.Errorf("End(\"id-future\") name = %q, want %q", name, "Bash")
	}
	if durationMS != 0 {
		t.Errorf("End(\"id-future\") durationMS = %d, want 0 (clamp for non-positive duration)", durationMS)
	}
}
