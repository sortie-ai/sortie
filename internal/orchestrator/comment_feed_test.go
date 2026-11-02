package orchestrator

import (
	"context"
	"testing"

	"github.com/sortie-ai/sortie/internal/storage"
)

func TestApplyQueuedAheadOfExit_RecordsGivenCommentsBeforeTheExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dispatchID string
		wantRows   int
	}{
		{name: "matching dispatch", dispatchID: "ISSUE-FEED-dispatch", wantRows: 2},
		{name: "mismatched dispatch", dispatchID: "an-earlier-dispatch", wantRows: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "ISSUE-FEED"
			state := NewState(60000, 4, 0, nil, AgentTotals{})
			state.Running[issueID] = queuedOrderingIssueEntry(issueID)

			store := &stubStore{}
			var recordedAtExit int
			store.onAppendRunHistory = func(storage.RunHistory) {
				store.mu.Lock()
				recordedAtExit = len(store.givenRows[issueID])
				store.mu.Unlock()
			}
			tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
			o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)

			o.commentsGivenCh <- commentsGivenMsg{
				IssueID:    issueID,
				DispatchID: tt.dispatchID,
				Rows: []storage.GivenComment{
					{ConversationID: "conv-1", Source: "tracker", CommentID: "c1"},
					{ConversationID: "conv-1", Source: "tracker", CommentID: "c2"},
				},
			}

			o.handleWorkerExit(context.Background(), WorkerResult{
				IssueID:    issueID,
				Identifier: issueID + "-ident",
				ExitKind:   WorkerExitNormal,
			})

			if recordedAtExit != tt.wantRows {
				t.Errorf("given comments recorded when the exit was handled = %d, want %d", recordedAtExit, tt.wantRows)
			}
			store.mu.Lock()
			got := len(store.givenRows[issueID])
			store.mu.Unlock()
			if got != tt.wantRows {
				t.Errorf("given comments recorded for %q = %d, want %d", issueID, got, tt.wantRows)
			}
		})
	}
}
