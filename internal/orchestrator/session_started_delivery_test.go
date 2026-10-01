package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/prompt"
)

type dispatchCommentProbe struct {
	*spyMetrics

	workspace string

	existedMu        sync.Mutex
	workspaceExisted []bool
}

func (p *dispatchCommentProbe) IncTrackerComments(lifecycle, result string) {
	if lifecycle == "dispatch" {
		p.existedMu.Lock()
		p.workspaceExisted = append(p.workspaceExisted, dirExists(p.workspace))
		p.existedMu.Unlock()
	}
	p.spyMetrics.IncTrackerComments(lifecycle, result)
}

func (p *dispatchCommentProbe) existedAtCount() []bool {
	p.existedMu.Lock()
	defer p.existedMu.Unlock()
	return slices.Clone(p.workspaceExisted)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

type sessionStartedRun struct {
	root    string
	slack   *notifierSpy
	wg      *sync.WaitGroup
	tracker *mockTrackerAdapter
	metrics *dispatchCommentProbe
	logs    *lockedBuf
	done    chan struct{}
	release func()
}

func startSessionStartedRun(t *testing.T, cancelAtTransition bool) *sessionStartedRun {
	t.Helper()

	root := t.TempDir()
	cfg := defaultWorkerConfig(root)
	cfg.Tracker.InProgressState = "In Progress"
	issue := workerTestIssue()

	slack := &notifierSpy{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(slack.release) }) }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tracker := &mockTrackerAdapter{}
	if cancelAtTransition {
		tracker.transitionIssueFn = func(context.Context, string, string) error {
			cancel()
			return nil
		}
	}
	metrics := &dispatchCommentProbe{spyMetrics: &spyMetrics{}, workspace: filepath.Join(root, issue.Identifier)}
	router := mustRouter(t, tracker, spyLookup(slack), route.Inputs{
		Entries:  []config.NotificationBackend{subscribe("slack", domain.EventTypeSessionStarted)},
		Comments: config.TrackerCommentsConfig{OnDispatch: true},
	})

	logs := &lockedBuf{}
	run := &sessionStartedRun{
		root: root, slack: slack, wg: &sync.WaitGroup{}, tracker: tracker, metrics: metrics,
		logs: logs, done: make(chan struct{}), release: release,
	}
	t.Cleanup(run.wg.Wait)
	t.Cleanup(release)
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		Router:                 router,
		TrackerOpsWg:           run.wg,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
		OnEvent:                func(string, domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:                metrics,
	}

	go func() {
		defer close(run.done)
		RunWorkerAttempt(ctx, issue, nil, deps)
	}()
	return run
}

func (r *sessionStartedRun) waitForWorker(t *testing.T) {
	t.Helper()

	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunWorkerAttempt did not return while the detached delivery was blocked")
	}
}

func TestSessionStartedDelivery_DetachedSendDoesNotBlockTheWorker(t *testing.T) {
	t.Parallel()

	run := startSessionStartedRun(t, false)
	run.waitForWorker(t)

	select {
	case <-run.slack.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber's Send never started")
	}
	if !dirExists(filepath.Join(run.root, workerTestIssue().Identifier)) {
		t.Error("workspace not prepared by the time RunWorkerAttempt returned, want it reached while the Send was blocked")
	}

	waited := make(chan struct{})
	go func() {
		run.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("TrackerOpsWg.Wait returned while the detached Send was blocked, want the send counted")
	default:
	}
	run.release()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("TrackerOpsWg.Wait did not return after the Send was released")
	}

	sent := run.slack.notifications()
	if len(sent) != 1 || sent[0].Envelope.EventType != domain.EventTypeSessionStarted || sent[0].Message.Body != dispatchComment {
		t.Fatalf("subscriber notifications = %+v, want one session.started carrying the dispatch comment", sent)
	}
	if sent[0].Envelope.IssueID != workerTestIssue().ID || sent[0].Envelope.Identifier != workerTestIssue().Identifier {
		t.Errorf("Envelope = %+v, want the dispatched issue", sent[0].Envelope)
	}
}

func TestSessionStartedDelivery_TrackerCommentLoggedAndCountedBeforeWorkspacePrep(t *testing.T) {
	t.Parallel()

	run := startSessionStartedRun(t, false)
	run.waitForWorker(t)
	run.release()

	run.tracker.commentMu.Lock()
	comments := slices.Clone(run.tracker.commentCalls)
	run.tracker.commentMu.Unlock()
	if len(comments) != 1 || comments[0].Text != dispatchComment {
		t.Fatalf("tracker comments = %+v, want exactly one dispatch comment", comments)
	}
	if got := run.metrics.existedAtCount(); !slices.Equal(got, []bool{false}) {
		t.Errorf("workspace existed when the dispatch counter was recorded = %v, want [false]", got)
	}
	run.metrics.mu.Lock()
	counted := slices.Clone(run.metrics.trackerComments)
	run.metrics.mu.Unlock()
	if want := (trackerCommentCall{lifecycle: "dispatch", result: "success"}); !slices.Contains(counted, want) {
		t.Errorf("IncTrackerComments calls = %v, want %v", counted, want)
	}
	if !strings.Contains(run.logs.String(), "dispatch comment posted") {
		t.Errorf("log = %q, want the dispatch comment posted record", run.logs.String())
	}
}

func TestSessionStartedDelivery_CancelledWorkerStartsNoDetachedSend(t *testing.T) {
	t.Parallel()

	run := startSessionStartedRun(t, true)
	run.waitForWorker(t)

	waited := make(chan struct{})
	go func() {
		run.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("TrackerOpsWg.Wait blocked, want no detached delivery counted for a cancelled worker")
	}
	run.release()

	if sent := run.slack.notifications(); len(sent) != 0 {
		t.Errorf("subscriber notifications = %+v, want none from a cancelled worker", sent)
	}
}
