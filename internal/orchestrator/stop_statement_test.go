package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/workspace"
)

func registeredSecret(t *testing.T) string {
	t.Helper()

	// The registry is add-only, so a random value keeps one test's
	// registration invisible to every other test.
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	secret := "stopsecret-" + hex.EncodeToString(buf)
	redact.Add("stop statement test", secret)
	return secret
}

func TestStopStatement_PublicStatement(t *testing.T) {
	t.Parallel()

	secret := registeredSecret(t)
	tests := []struct {
		name      string
		statement workspace.StatusStatement
		want      string
	}{
		{name: "empty text is absent", statement: workspace.StatusStatement{}, want: ""},
		{name: "whitespace-only text is absent", statement: workspace.StatusStatement{Text: " \t\n\r\n "}, want: ""},
		{name: "plain reason is kept", statement: workspace.StatusStatement{Text: "Which one should the API expose?"}, want: "Which one should the API expose?"},
		{name: "surrounding whitespace is trimmed and inner lines kept", statement: workspace.StatusStatement{Text: "\n  first\n\tsecond  \n\n"}, want: "first\n\tsecond"},
		{name: "crlf and lone cr become line feeds", statement: workspace.StatusStatement{Text: "a\r\nb\rc"}, want: "a\nb\nc"},
		{name: "control characters other than tab and line feed are deleted", statement: workspace.StatusStatement{Text: "a\x00b\x07c\x1bd\x7fe\tf\ng"}, want: "abcde\tf\ng"},
		{name: "invalid utf-8 becomes the replacement character", statement: workspace.StatusStatement{Text: "a\xffb"}, want: "a�b"},
		{name: "registered secret is masked", statement: workspace.StatusStatement{Text: "token=" + secret + " done"}, want: "token=" + redact.Marker + " done"},
		{
			name:      "control character inside a registered secret cannot hide it from the mask",
			statement: workspace.StatusStatement{Text: "token=" + secret[:6] + "\x00" + secret[6:] + " done"},
			want:      "token=" + redact.Marker + " done",
		},
		{
			name:      "complete secret at the cut is masked without dropping anything else",
			statement: workspace.StatusStatement{Text: "token=" + secret, Truncated: true},
			want:      "token=" + redact.Marker + "…",
		},
		{name: "truncated statement ends in an ellipsis", statement: workspace.StatusStatement{Text: "cut here~", Truncated: true}, want: "cut here~…"},
		{name: "truncated whitespace-only statement is absent", statement: workspace.StatusStatement{Text: " \n", Truncated: true}, want: ""},
		{
			name:      "cut inside a multi-byte rune drops the partial rune",
			statement: workspace.StatusStatement{Text: "reason 日" + "本"[:2], Truncated: true},
			want:      "reason 日…",
		},
		{
			name:      "cut inside a registered secret leaves no prefix of it",
			statement: workspace.StatusStatement{Text: "see " + secret[:len(secret)/2], Truncated: true},
			want:      "see…",
		},
		{
			name:      "control character inside a cut secret fragment cannot hide the fragment from the cut",
			statement: workspace.StatusStatement{Text: "see " + secret[:4] + "\x00" + secret[4:10], Truncated: true},
			want:      "see…",
		},
		{
			name:      "cut after one byte of a registered secret leaves no prefix of it",
			statement: workspace.StatusStatement{Text: "see " + secret[:1], Truncated: true},
			want:      "see…",
		},
		{
			name:      "statement that is only a cut secret is absent",
			statement: workspace.StatusStatement{Text: secret[:5], Truncated: true},
			want:      "",
		},
		{
			name:      "text past the bound is cut to it with an ellipsis",
			statement: workspace.StatusStatement{Text: strings.Repeat("a", 1030)},
			want:      strings.Repeat("a", 1024) + "…",
		},
		{
			name:      "truncated statement at the bound keeps the bound",
			statement: workspace.StatusStatement{Text: strings.Repeat("a", 1024), Truncated: true},
			want:      strings.Repeat("a", 1024) + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := publicStatement(tt.statement)

			if got != tt.want {
				t.Errorf("publicStatement(%+v) = %q, want %q", tt.statement, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("publicStatement(%+v) = %q, want valid UTF-8", tt.statement, got)
			}
			if strings.Contains(got, secret) {
				t.Errorf("publicStatement(%+v) = %q, holds the registered secret", tt.statement, got)
			}
		})
	}
}

func TestStopStatement_PublicStatementMasksSecretHoldingARewrittenByte(t *testing.T) {
	t.Parallel()

	for _, b := range []string{"\r", "\x01"} {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		withByte := "stopsecret-" + hex.EncodeToString(buf[:8]) + b + hex.EncodeToString(buf[8:])
		redact.Add("stop statement test", withByte)
		statement := workspace.StatusStatement{Text: "token=" + withByte + " done"}

		got := publicStatement(statement)

		if want := "token=" + redact.Marker + " done"; got != want {
			t.Errorf("publicStatement(secret holding %q) = %q, want %q", b, got, want)
		}
	}
}

func TestStopStatement_PublicStatementLeavesNoSecretPrefixAtAnyCut(t *testing.T) {
	t.Parallel()

	secret := registeredSecret(t)
	for n := 1; n < len(secret); n++ {
		statement := workspace.StatusStatement{Text: "see: " + secret[:n], Truncated: true}

		got := publicStatement(statement)

		if want := "see:…"; got != want {
			t.Errorf("publicStatement(cut after %d secret bytes) = %q, want %q", n, got, want)
		}
	}
}

type stopExit struct {
	tracker *mockTrackerAdapter
	slack   *notifierSpy
}

func exitWithStatement(t *testing.T, result WorkerResult, withheld bool) stopExit {
	t.Helper()

	const issueID = "STOP-1"
	tracker := &mockTrackerAdapter{}
	slack := &notifierSpy{}
	router := mustRouter(t, tracker, spyLookup(slack), route.Inputs{Entries: []config.NotificationBackend{
		subscribe(domain.TrackerCommentKind, domain.EventSessionStopped, domain.EventSessionFailed),
		subscribe("slack", domain.EventSessionStopped, domain.EventSessionFailed),
	}})
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, &mockExitStore{}, tracker, &domain.NoopMetrics{})
	params.Router = router
	result.IssueID = issueID
	result.Identifier = issueID + "-ident"
	result.AgentAdapter = "mock"
	if withheld {
		dir, baseline := handoffEvidenceGitWorkspace(t)
		result.WorkspacePath = dir
		result.HandoffEvidencePolicy = config.HandoffEvidenceObserved
		result.HandoffEvidenceBaseline = baseline
	}

	HandleWorkerExit(state, result, params)
	state.TrackerOpsWg.Wait()
	t.Cleanup(func() { CancelRetry(state, issueID) })

	return stopExit{tracker: tracker, slack: slack}
}

func (e stopExit) comments(t *testing.T) []commentIssueCall {
	t.Helper()

	e.tracker.commentMu.Lock()
	defer e.tracker.commentMu.Unlock()
	return append([]commentIssueCall(nil), e.tracker.commentCalls...)
}

func (e stopExit) singleComment(t *testing.T) commentIssueCall {
	t.Helper()

	comments := e.comments(t)
	if len(comments) != 1 {
		t.Fatalf("tracker comments = %+v, want exactly one", comments)
	}
	return comments[0]
}

func (e stopExit) singleNotification(t *testing.T) domain.Notification {
	t.Helper()

	sent := e.slack.notifications()
	if len(sent) != 1 {
		t.Fatalf("slack notifications = %+v, want exactly one", sent)
	}
	return sent[0]
}

func TestStopStatement_ExitCommentCarriesTheReasonAsLiteral(t *testing.T) {
	t.Parallel()

	secret := registeredSecret(t)
	tests := []struct {
		name        string
		reason      string
		statement   workspace.StatusStatement
		wantBody    string
		wantLiteral string
	}{
		{
			name:        "blocked run shows its reason",
			reason:      "blocked",
			statement:   workspace.StatusStatement{Text: "The ticket asks for both soft and hard delete.\nWhich one should the API expose?\n"},
			wantBody:    "Sortie session completed (agent signaled: blocked).\nDuration: 1m0s\nTurns: 2",
			wantLiteral: "The ticket asks for both soft and hard delete.\nWhich one should the API expose?",
		},
		{
			name:        "no-change-needed run shows its reason",
			reason:      "no-change-needed",
			statement:   workspace.StatusStatement{Text: "The endpoint already returns 404 for unknown ids."},
			wantBody:    "Sortie session completed (agent signaled: no-change-needed).\nDuration: 1m0s\nTurns: 2",
			wantLiteral: "The endpoint already returns 404 for unknown ids.",
		},
		{
			name:        "needs-human-review run shows its reason",
			reason:      "needs-human-review",
			statement:   workspace.StatusStatement{Text: "Check the retry path in worker.go."},
			wantBody:    "Sortie session completed (agent signaled: needs-human-review).\nDuration: 1m0s\nTurns: 2",
			wantLiteral: "Check the retry path in worker.go.",
		},
		{
			name:        "registered secret in the reason is masked",
			reason:      "blocked",
			statement:   workspace.StatusStatement{Text: "Use /close and @user with " + secret + " please"},
			wantBody:    "Sortie session completed (agent signaled: blocked).\nDuration: 1m0s\nTurns: 2",
			wantLiteral: "Use /close and @user with " + redact.Marker + " please",
		},
		{
			name:        "statement cut at the read bound ends in an ellipsis",
			reason:      "blocked",
			statement:   workspace.StatusStatement{Text: "needs a decision~", Truncated: true},
			wantBody:    "Sortie session completed (agent signaled: blocked).\nDuration: 1m0s\nTurns: 2",
			wantLiteral: "needs a decision~…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exit := exitWithStatement(t, WorkerResult{
				ExitKind:          WorkerExitNormal,
				TurnsCompleted:    2,
				SoftStop:          true,
				SoftStopReason:    tt.reason,
				SoftStopStatement: tt.statement,
			}, false)

			comment := exit.singleComment(t)
			if comment.Text != tt.wantBody || comment.Literal != tt.wantLiteral {
				t.Errorf("tracker comment = {Text:%q Literal:%q}, want {Text:%q Literal:%q}", comment.Text, comment.Literal, tt.wantBody, tt.wantLiteral)
			}
			notification := exit.singleNotification(t)
			if notification.Message.AgentText != tt.wantLiteral {
				t.Errorf("slack AgentText = %q, want %q", notification.Message.AgentText, tt.wantLiteral)
			}
			if notification.Message.Body != tt.wantBody {
				t.Errorf("slack Body = %q, want %q (the statement travels apart from the body)", notification.Message.Body, tt.wantBody)
			}
			if notification.Envelope.EventType != domain.EventSessionStopped {
				t.Errorf("slack EventType = %q, want %q", notification.Envelope.EventType, domain.EventSessionStopped)
			}
		})
	}
}

func TestStopStatement_ExitWithoutAReasonCommentsAsBefore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		statement workspace.StatusStatement
	}{
		{"status token alone", workspace.StatusStatement{}},
		{"whitespace-only reason", workspace.StatusStatement{Text: " \n\t\n"}},
		{"truncated whitespace-only reason", workspace.StatusStatement{Text: "\n", Truncated: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			exit := exitWithStatement(t, WorkerResult{
				ExitKind:          WorkerExitNormal,
				TurnsCompleted:    2,
				SoftStop:          true,
				SoftStopReason:    "blocked",
				SoftStopStatement: tt.statement,
			}, false)

			comment := exit.singleComment(t)
			want := commentIssueCall{
				IssueID: "STOP-1",
				Text:    "Sortie session completed (agent signaled: blocked).\nDuration: 1m0s\nTurns: 2",
			}
			if comment != want {
				t.Errorf("tracker comment = %+v, want %+v (no literal block)", comment, want)
			}
			if got := exit.singleNotification(t).Message.AgentText; got != "" {
				t.Errorf("slack AgentText = %q, want empty", got)
			}
		})
	}
}

func TestStopStatement_FailedSessionCarriesNoReason(t *testing.T) {
	t.Parallel()

	exit := exitWithStatement(t, WorkerResult{
		ExitKind:          WorkerExitNormal,
		TurnsCompleted:    1,
		SoftStop:          true,
		SoftStopReason:    "needs-human-review",
		SoftStopStatement: workspace.StatusStatement{Text: "Check the retry path."},
	}, true)

	notification := exit.singleNotification(t)
	if notification.Envelope.EventType != domain.EventSessionFailed {
		t.Fatalf("slack EventType = %q, want %q (handoff evidence withheld the transition)", notification.Envelope.EventType, domain.EventSessionFailed)
	}
	if notification.Message.AgentText != "" {
		t.Errorf("session.failed AgentText = %q, want empty", notification.Message.AgentText)
	}
	comment := exit.singleComment(t)
	if comment.Literal != "" || !strings.HasPrefix(comment.Text, "Sortie session failed.") {
		t.Errorf("tracker comment = %+v, want the failure comment with no literal block", comment)
	}
}

func TestStopStatement_ErrorExitCarriesNoReason(t *testing.T) {
	t.Parallel()

	exit := exitWithStatement(t, WorkerResult{
		ExitKind:          WorkerExitError,
		Error:             &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "turn timed out"},
		SoftStopStatement: workspace.StatusStatement{Text: "left over"},
	}, false)

	if got := exit.singleNotification(t).Message.AgentText; got != "" {
		t.Errorf("session.failed AgentText = %q, want empty", got)
	}
	if comment := exit.singleComment(t); comment.Literal != "" {
		t.Errorf("tracker comment literal = %q, want empty", comment.Literal)
	}
}

type statusScenario struct {
	name       string
	maxTurns   int
	verify     string
	coding     string
	review     string
	reviewPass bool
	wantReason string
	wantText   string
	wantTrunc  bool
	wantNoStop bool
}

func (s statusScenario) run(t *testing.T) WorkerResult {
	t.Helper()

	cfg := defaultWorkerConfig(t.TempDir())
	cfg.Agent.MaxTurns = s.maxTurns
	if s.verify != "" {
		cfg.SelfReview = config.SelfReviewConfig{
			Enabled:               true,
			MaxIterations:         1,
			VerificationCommands:  []string{s.verify},
			VerificationTimeoutMS: 5000,
		}
	}
	startFn, wsPath := captureWorkspacePath()
	ec := newExitCapture()
	codingWritten := false

	deps := WorkerDeps{
		TrackerAdapter: &mockTrackerAdapter{},
		AgentAdapter: &mockAgentAdapter{
			startSessionFn: startFn,
			runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
				switch {
				case isSelfReviewTurnPrompt(params.Prompt):
					if s.review != "" {
						writeStatusFile(t, wsPath(), s.review)
					}
					if s.reviewPass {
						writeVerdictFile(t, wsPath(), domain.ReviewVerdict{Verdict: "pass", Summary: "looks good"})
					}
				case !isSelfReviewFixPrompt(params.Prompt) && !codingWritten:
					codingWritten = true
					if s.coding != "" {
						writeStatusFile(t, wsPath(), s.coding)
					}
				}
				return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	return ec.waitResult(t)
}

func TestStopStatement_RunWorkerAttemptCapturesTheStatementOfThePendingToken(t *testing.T) {
	t.Parallel()

	longReason := strings.Repeat("a", 1025-len("blocked\n"))
	tests := []statusScenario{
		{
			name:       "coding turn blocked keeps the lines after the token",
			maxTurns:   3,
			coding:     "blocked\nneed a schema\nsecond line\n",
			wantReason: "blocked",
			wantText:   "need a schema\nsecond line\n",
		},
		{
			name:       "coding turn no-change-needed keeps its reason",
			maxTurns:   3,
			coding:     "no-change-needed\nAlready fixed on main.",
			wantReason: "no-change-needed",
			wantText:   "Already fixed on main.",
		},
		{
			name:       "coding turn needs-human-review keeps its reason",
			maxTurns:   3,
			coding:     "needs-human-review\ncheck the retry path",
			wantReason: "needs-human-review",
			wantText:   "check the retry path",
		},
		{
			name:       "token alone leaves the statement empty",
			maxTurns:   3,
			coding:     "blocked",
			wantReason: "blocked",
		},
		{
			name:       "file longer than the read bound flags the cut",
			maxTurns:   3,
			coding:     "blocked\n" + longReason,
			wantReason: "blocked",
			wantText:   longReason[:len(longReason)-1],
			wantTrunc:  true,
		},
		{
			name:       "in-phase blocked replaces the pending token and its statement",
			maxTurns:   10,
			verify:     "echo ok",
			coding:     "needs-human-review\nfirst reason",
			review:     "blocked\nsecond reason",
			wantReason: "blocked",
			wantText:   "second reason",
		},
		{
			name:       "in-phase no-change-needed is discarded and the pending statement stays",
			maxTurns:   10,
			verify:     "echo ok",
			coding:     "needs-human-review\nfirst reason",
			review:     "no-change-needed\nlater reason",
			reviewPass: true,
			wantReason: "needs-human-review",
			wantText:   "first reason",
		},
		{
			name:       "in-phase needs-human-review is discarded and the pending statement stays",
			maxTurns:   10,
			verify:     "echo ok",
			coding:     "no-change-needed\nfirst reason",
			review:     "needs-human-review\nlater reason",
			reviewPass: true,
			wantReason: "no-change-needed",
			wantText:   "first reason",
		},
		{
			name:       "retracted no-change-needed clears its statement",
			maxTurns:   10,
			verify:     "exit 1",
			coding:     "no-change-needed\nAlready fixed on main.",
			reviewPass: true,
			wantNoStop: true,
		},
		{
			name:       "run without a status file has no statement",
			maxTurns:   1,
			wantNoStop: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := tt.run(t)

			if tt.wantNoStop {
				if result.SoftStop || result.SoftStopReason != "" {
					t.Errorf("SoftStop, SoftStopReason = %v, %q, want false and empty", result.SoftStop, result.SoftStopReason)
				}
			} else if result.SoftStopReason != tt.wantReason {
				t.Errorf("SoftStopReason = %q, want %q", result.SoftStopReason, tt.wantReason)
			}
			want := workspace.StatusStatement{Text: tt.wantText, Truncated: tt.wantTrunc}
			if result.SoftStopStatement != want {
				t.Errorf("SoftStopStatement = %+v, want %+v", result.SoftStopStatement, want)
			}
		})
	}
}

func TestStopStatement_ReadAndConsumeReturnsTheStatementAndRemovesTheFile(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeStatusFile(t, wsPath, "blocked\nneed a schema\n")

	got := readAndConsumeStatusSignal(wsPath, discardLogger())

	want := workspace.StatusFile{
		Signal:    workspace.StatusBlocked,
		Statement: workspace.StatusStatement{Text: "need a schema\n"},
	}
	if got != want {
		t.Errorf("readAndConsumeStatusSignal() = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(wsPath, ".sortie", "status")); !os.IsNotExist(err) {
		t.Errorf("os.Stat(status) error = %v, want not-exist after consuming a recognized signal", err)
	}
}
