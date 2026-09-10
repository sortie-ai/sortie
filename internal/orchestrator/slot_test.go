package orchestrator

import (
	"fmt"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func makeRunningFromStates(states []string) map[string]*RunningEntry {
	running := make(map[string]*RunningEntry, len(states))
	for i, state := range states {
		id := fmt.Sprintf("ISSUE-%d", i+1)
		running[id] = &RunningEntry{Issue: domain.Issue{State: state}}
	}
	return running
}

func TestGlobalAvailableSlots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		maxConcurrentAgents int
		runningCount        int
		want                int
	}{
		{name: "all slots available", maxConcurrentAgents: 10, runningCount: 0, want: 10},
		{name: "partial usage", maxConcurrentAgents: 10, runningCount: 5, want: 5},
		{name: "at capacity", maxConcurrentAgents: 10, runningCount: 10, want: 0},
		{name: "over capacity never negative", maxConcurrentAgents: 10, runningCount: 15, want: 0},
		{name: "zero configured slots", maxConcurrentAgents: 0, runningCount: 0, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := GlobalAvailableSlots(tt.maxConcurrentAgents, tt.runningCount)
			if got != tt.want {
				t.Errorf("GlobalAvailableSlots(%d, %d) = %d, want %d", tt.maxConcurrentAgents, tt.runningCount, got, tt.want)
			}
		})
	}
}

func TestStateAvailableSlots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		state                string
		maxConcurrentByState map[string]int
		stateRunningCount    int
		globalAvailable      int
		want                 int
	}{
		{
			name:                 "per-state limit under cap",
			state:                "to do",
			maxConcurrentByState: map[string]int{"to do": 3},
			stateRunningCount:    1,
			globalAvailable:      8,
			want:                 2,
		},
		{
			name:                 "per-state limit at cap",
			state:                "to do",
			maxConcurrentByState: map[string]int{"to do": 3},
			stateRunningCount:    3,
			globalAvailable:      8,
			want:                 0,
		},
		{
			name:                 "per-state limit over cap",
			state:                "to do",
			maxConcurrentByState: map[string]int{"to do": 3},
			stateRunningCount:    5,
			globalAvailable:      8,
			want:                 0,
		},
		{
			name:                 "no per-state limit falls back to global",
			state:                "to do",
			maxConcurrentByState: map[string]int{},
			stateRunningCount:    1,
			globalAvailable:      8,
			want:                 8,
		},
		{
			name:                 "case-normalized state lookup",
			state:                "In Progress",
			maxConcurrentByState: map[string]int{"in progress": 2},
			stateRunningCount:    1,
			globalAvailable:      5,
			want:                 1,
		},
		{
			name:                 "nil per-state map falls back to global",
			state:                "To Do",
			maxConcurrentByState: nil,
			stateRunningCount:    1,
			globalAvailable:      4,
			want:                 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := StateAvailableSlots(tt.state, tt.maxConcurrentByState, tt.stateRunningCount, tt.globalAvailable)
			if got != tt.want {
				t.Errorf("StateAvailableSlots(%q, ..., %d, %d) = %d, want %d", tt.state, tt.stateRunningCount, tt.globalAvailable, got, tt.want)
			}
		})
	}
}

func TestHasAvailableSlots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		globalMax            int
		maxConcurrentByState map[string]int
		runningStates        []string
		queryState           string
		want                 bool
	}{
		{name: "empty no limits", globalMax: 10, maxConcurrentByState: map[string]int{}, runningStates: nil, queryState: "To Do", want: true},
		{name: "global full", globalMax: 10, maxConcurrentByState: map[string]int{}, runningStates: []string{"To Do", "To Do", "To Do", "To Do", "To Do", "To Do", "To Do", "To Do", "To Do", "To Do"}, queryState: "To Do", want: false},
		{name: "per-state full", globalMax: 10, maxConcurrentByState: map[string]int{"to do": 2}, runningStates: []string{"To Do", "To Do", "In Progress", "In Progress", "In Progress"}, queryState: "To Do", want: false},
		{name: "per-state has room", globalMax: 10, maxConcurrentByState: map[string]int{"to do": 2}, runningStates: []string{"To Do", "In Progress", "In Progress", "In Progress"}, queryState: "To Do", want: true},
		{name: "uncapped state", globalMax: 10, maxConcurrentByState: map[string]int{"to do": 2}, runningStates: []string{"To Do", "In Progress", "In Progress", "In Progress"}, queryState: "In Progress", want: true},
		{name: "both states capped", globalMax: 10, maxConcurrentByState: map[string]int{"to do": 2, "in progress": 3}, runningStates: []string{"To Do", "In Progress", "In Progress", "In Progress"}, queryState: "In Progress", want: false},
		{name: "global blocks despite per-state room", globalMax: 2, maxConcurrentByState: map[string]int{"to do": 5}, runningStates: []string{"To Do", "To Do"}, queryState: "To Do", want: false},
		{name: "one global slot and per-state room", globalMax: 5, maxConcurrentByState: map[string]int{"to do": 1}, runningStates: []string{"In Progress", "In Progress", "In Progress", "In Progress"}, queryState: "To Do", want: true},
		{name: "global full per-state irrelevant", globalMax: 5, maxConcurrentByState: map[string]int{"to do": 1}, runningStates: []string{"To Do", "In Progress", "In Progress", "In Progress", "In Progress"}, queryState: "To Do", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewState(1000, tt.globalMax, 0, tt.maxConcurrentByState, AgentTotals{})
			s.Running = makeRunningFromStates(tt.runningStates)

			got := HasAvailableSlots(s, tt.queryState)
			if got != tt.want {
				t.Errorf("HasAvailableSlots(..., %q) = %t, want %t", tt.queryState, got, tt.want)
			}
		})
	}
}
