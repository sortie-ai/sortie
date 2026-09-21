package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

const verificationUUID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"

func TestFindVerificationConversation(t *testing.T) {
	t.Parallel()

	before := []kiroSessionListing{{SessionID: "existing-1", Source: "classic", Title: "prior work"}}
	withNew := func(entries ...kiroSessionListing) []kiroSessionListing {
		return append(append([]kiroSessionListing{}, before...), entries...)
	}

	tests := []struct {
		name        string
		after       []kiroSessionListing
		wantOutcome verificationListingOutcome
		wantID      string
	}{
		{name: "no new entry", after: before, wantOutcome: verificationNoNewEntry},
		{
			name:        "one new classic entry with a UUID identifier",
			after:       withNew(kiroSessionListing{SessionID: verificationUUID, Source: "classic"}),
			wantOutcome: verificationConversationFound,
			wantID:      verificationUUID,
		},
		{
			name: "two new entries",
			after: withNew(
				kiroSessionListing{SessionID: verificationUUID, Source: "classic"},
				kiroSessionListing{SessionID: "another-new-id", Source: "classic"}),
			wantOutcome: verificationAmbiguous,
		},
		{
			name:        "non-classic source",
			after:       withNew(kiroSessionListing{SessionID: verificationUUID, Source: "protocol"}),
			wantOutcome: verificationAmbiguous,
		},
		{
			name:        "non-UUID identifier",
			after:       withNew(kiroSessionListing{SessionID: "not-a-uuid", Source: "classic"}),
			wantOutcome: verificationAmbiguous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, outcome := findVerificationConversation(before, tt.after)

			if outcome != tt.wantOutcome {
				t.Errorf("findVerificationConversation() outcome = %v, want %v", outcome, tt.wantOutcome)
			}
			if tt.wantID != "" && got.SessionID != tt.wantID {
				t.Errorf("findVerificationConversation() SessionID = %q, want %q", got.SessionID, tt.wantID)
			}
		})
	}
}

const listingCallScenario = "kiro.listing-call"

type listingCallParams struct {
	FirstListing  string
	SecondListing string
	CallCountFile string
	DeleteMarker  string
}

func init() {
	fakeScenarios[listingCallScenario] = agenttest.Typed(runListingCallScenario)
}

func runListingCallScenario(args []string, params listingCallParams) int {
	if len(args) > 1 && args[0] == "chat" && args[1] == "--delete-session" {
		_ = os.WriteFile(params.DeleteMarker, []byte(strings.Join(args, " ")), 0o600)
		return 0
	}

	count := 0
	if data, err := os.ReadFile(params.CallCountFile); err == nil {
		count = len(data)
	}
	if err := os.WriteFile(params.CallCountFile, make([]byte, count+1), 0o600); err != nil {
		return 1
	}

	if count == 0 {
		_, _ = os.Stdout.WriteString(params.FirstListing)
		return 0
	}
	_, _ = os.Stdout.WriteString(params.SecondListing)
	return 0
}

func TestDeleteVerificationConversation(t *testing.T) {
	t.Parallel()

	marshalListing := func(t *testing.T, cwd string, sessions ...kiroSessionListing) string {
		t.Helper()
		data, err := json.Marshal([]kiroSessionListGroup{{Cwd: cwd, Sessions: sessions}})
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		return string(data)
	}
	priorSession := kiroSessionListing{SessionID: "existing-1", Source: "classic", Title: "prior"}
	baseline := func(cwd string) string {
		return marshalListing(t, cwd, priorSession)
	}
	oneNew := func(cwd string) string {
		return marshalListing(t, cwd, priorSession, kiroSessionListing{SessionID: verificationUUID, Source: "classic", Title: "a"})
	}
	twoNew := func(cwd string) string {
		return marshalListing(t, cwd,
			priorSession,
			kiroSessionListing{SessionID: verificationUUID, Source: "classic", Title: "a"},
			kiroSessionListing{SessionID: "bb778ea0-6eab-4ce9-b87e-11d6d33dab4f", Source: "classic", Title: "b"})
	}

	tests := []struct {
		name            string
		firstFailed     bool
		second          func(cwd string) string
		wantDeleteArgs  string
		wantLoggedError bool
	}{
		{name: "first listing failed skips deletion for lack of a trustworthy baseline", firstFailed: true, second: oneNew},
		{name: "no new entry", second: baseline},
		{name: "ambiguous listing", second: twoNew, wantLoggedError: true},
		{
			name:           "one new conversation",
			second:         oneNew,
			wantDeleteArgs: "chat --delete-session " + verificationUUID + " --session-source v1",
		},
		{
			name:            "second listing reports another workspace",
			second:          func(string) string { return oneNew("/elsewhere") },
			wantLoggedError: true,
		},
		{
			name:            "second listing carries two directory groups",
			second:          func(string) string { return `[{"cwd":"a","sessions":[]},{"cwd":"b","sessions":[]}]` },
			wantLoggedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			countFile := filepath.Join(dir, "call-count")
			deleteMarker := filepath.Join(dir, "delete-marker")
			bin := agenttest.FakeRuntime(t, dir, "kiro-cli", listingCallScenario, listingCallParams{
				FirstListing:  baseline(dir),
				SecondListing: tt.second(dir),
				CallCountFile: countFile,
				DeleteMarker:  deleteMarker,
			})
			var logs bytes.Buffer
			s := &sessionState{
				target:     agentcore.LaunchTarget{Command: bin, WorkspacePath: dir},
				baseLogger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			}
			if tt.firstFailed {
				s.verificationBefore = []kiroSessionListing{{SessionID: "existing-1", Source: "classic"}}
			} else {
				listing, err := listWorkspaceConversations(context.Background(), s.target, agentcore.AuxiliaryTimeout(domain.AgentConfig{}), 0)
				if err != nil {
					t.Fatalf("listWorkspaceConversations() (first) error = %v", err)
				}
				s.verificationBefore = listing
				s.verificationBeforeListed = true
			}

			s.deleteVerificationConversation(context.Background())

			deleted, _ := os.ReadFile(deleteMarker)
			if tt.wantDeleteArgs == "" && len(deleted) != 0 {
				t.Errorf("deleteVerificationConversation() ran %q, want no deletion", deleted)
			}
			if tt.wantDeleteArgs != "" && string(deleted) != tt.wantDeleteArgs {
				t.Errorf("deleteVerificationConversation() deletion args = %q, want %q", deleted, tt.wantDeleteArgs)
			}
			if _, err := os.Stat(countFile); tt.firstFailed && err == nil {
				t.Error("deleteVerificationConversation() listed conversations after a failed first listing, want no invocation")
			}
			if tt.wantLoggedError && !strings.Contains(logs.String(), "error=") {
				t.Errorf("deleteVerificationConversation() log = %q, want an error attribute", logs.String())
			}
		})
	}
}
