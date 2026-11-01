package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/storage"
)

const (
	// maxRetainedEvents and maxRetainedEventBytes bound one running run's
	// buffer.
	maxRetainedEvents     = 1000
	maxRetainedEventBytes = 256 << 10

	// retainedMessageRunes leaves room for the ellipsis [redact.Truncate]
	// appends, so a retained message never exceeds 1,000 runes.
	retainedMessageRunes = 999

	// maxHeldRunEventBytes bounds the events awaiting a retry, all runs
	// together.
	maxHeldRunEventBytes = 4 << 20
)

// runEventStoreLimits bounds the stored events of every run together.
var runEventStoreLimits = storage.RunEventLimits{
	MaxAge:         365 * 24 * time.Hour,
	MaxBytes:       2 << 30,
	MaxExpiredRuns: 4,
}

// defaultRunEventRetryDelays holds the wait before each successive retry of a
// failed events write.
var defaultRunEventRetryDelays = []time.Duration{
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	64 * time.Second,
}

// RunEvent is one retained agent event of a run, immutable once retained.
type RunEvent struct {
	// Seq starts at 1 and rises by one per retained event of the run;
	// eviction never renumbers.
	Seq        int64
	At         time.Time
	Event      domain.AgentEventType
	Message    string
	SessionID  string
	ToolName   string
	DurationMS int64
	ToolError  bool

	// ToolKind and ToolArgument are set on tool_result events only; both are
	// empty for an other tool and for an event stored without them.
	ToolKind     domain.ToolKind
	ToolArgument string

	// Usage is nil while the run's usage is unmeasured. It is read only on
	// the turn-ending types.
	Usage *domain.TokenUsage

	size int64
}

// RunEventPage is the part of one run's retained events that follows a cursor.
type RunEventPage struct {
	// Events ascend by seq: those above the cursor, or every retained event
	// when Expired.
	Events []RunEvent
	// Expired reports that events between the cursor and FirstSeq were evicted.
	Expired bool
	// FirstSeq and LastSeq are the seq of the oldest and the newest retained
	// event; both are 0 when the run retained none.
	FirstSeq, LastSeq int64
}

// PageAfter returns the page of events, which ascend by seq, that follows
// cursor. A cursor above the newest seq reads as the newest seq.
func PageAfter(events []RunEvent, cursor int64) RunEventPage {
	if len(events) == 0 {
		return RunEventPage{}
	}
	page := RunEventPage{FirstSeq: events[0].Seq, LastSeq: events[len(events)-1].Seq}
	cursor = min(cursor, page.LastSeq)
	if cursor < page.FirstSeq-1 {
		page.Events = events
		page.Expired = true
		return page
	}
	page.Events = events[sort.Search(len(events), func(i int) bool { return events[i].Seq > cursor }):]
	return page
}

// runEventPayload is the stored and served form of the members of a
// [RunEvent] other than Seq, At, and Event. Presence depends on the event's
// type and values alone.
type runEventPayload struct {
	Message    string          `json:"message"`
	SessionID  *string         `json:"session_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	ToolError  *bool           `json:"tool_error,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`

	ToolKind     string `json:"tool_kind,omitempty"`
	ToolArgument string `json:"tool_argument,omitempty"`
}

type runEventUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Payload returns the JSON object of the event's message and the members its
// type carries. It is the only encoder of an event's members, for the store
// and for every response, and never includes seq, at, event, or text.
func (e RunEvent) Payload() []byte {
	payload := runEventPayload{Message: e.Message}
	switch {
	case e.Event == domain.EventSessionStarted:
		payload.SessionID = &e.SessionID
	case e.Event == domain.EventToolResult:
		payload.ToolName = e.ToolName
		payload.ToolKind = string(e.ToolKind)
		payload.ToolArgument = e.ToolArgument
		payload.DurationMS = max(e.DurationMS, 0)
		payload.ToolError = &e.ToolError
	case isTurnTerminalEvent(e.Event):
		payload.Usage = json.RawMessage("null")
		if e.Usage != nil {
			// A struct of integers always marshals.
			payload.Usage, _ = json.Marshal(runEventUsage(*e.Usage))
		}
	}
	// A struct of strings, integers, and valid raw JSON always marshals.
	encoded, _ := json.Marshal(payload)
	return encoded
}

func (e RunEvent) atText() string {
	return e.At.Format(time.RFC3339Nano)
}

// withSize returns e with its encoded size set: the byte length of the stored
// at, event, and payload text together.
func (e RunEvent) withSize() RunEvent {
	e.size = int64(len(e.atText()) + len(e.Event) + len(e.Payload()))
	return e
}

// storeRow returns the stored form of e.
func (e RunEvent) storeRow() storage.RunEvent {
	return storage.RunEvent{
		Seq:     e.Seq,
		At:      e.atText(),
		Event:   string(e.Event),
		Payload: string(e.Payload()),
		Size:    e.size,
	}
}

// RunEventsFromRows decodes stored events, oldest first, and returns an
// error for a row that does not decode. It returns an empty non-nil slice
// for no rows.
func RunEventsFromRows(rows []storage.RunEvent) ([]RunEvent, error) {
	events := make([]RunEvent, 0, len(rows))
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339Nano, row.At)
		if err != nil {
			return nil, fmt.Errorf("decode run event seq %d: parse at: %w", row.Seq, err)
		}
		var payload runEventPayload
		if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
			return nil, fmt.Errorf("decode run event seq %d: payload: %w", row.Seq, err)
		}

		event := RunEvent{
			Seq:          row.Seq,
			At:           at.UTC(),
			Event:        domain.AgentEventType(row.Event),
			Message:      payload.Message,
			ToolName:     payload.ToolName,
			DurationMS:   payload.DurationMS,
			ToolKind:     domain.ToolKind(payload.ToolKind),
			ToolArgument: payload.ToolArgument,
			size:         row.Size,
		}
		if payload.SessionID != nil {
			event.SessionID = *payload.SessionID
		}
		if payload.ToolError != nil {
			event.ToolError = *payload.ToolError
		}
		if len(payload.Usage) > 0 && string(payload.Usage) != "null" {
			var usage runEventUsage
			if err := json.Unmarshal(payload.Usage, &usage); err != nil {
				return nil, fmt.Errorf("decode run event seq %d: usage: %w", row.Seq, err)
			}
			event.Usage = new(domain.TokenUsage(usage))
		}
		events = append(events, event)
	}
	return events, nil
}

// RunEventLog holds the event buffer of every running run by dispatch ID. It
// is safe for concurrent use.
type RunEventLog struct {
	mu      sync.Mutex
	buffers map[string]*runEventBuffer // guarded by mu
}

// NewRunEventLog returns an empty log.
func NewRunEventLog() *RunEventLog {
	return &RunEventLog{buffers: make(map[string]*runEventBuffer)}
}

// Events returns a copy of a running run's events, oldest first, and false
// when no running run carries dispatchID.
func (l *RunEventLog) Events(dispatchID string) ([]RunEvent, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	buffer, ok := l.buffers[dispatchID]
	l.mu.Unlock()
	if !ok {
		return nil, false
	}
	return buffer.snapshot(), true
}

// After returns [PageAfter] of the running run that carries dispatchID, with
// Events copied so that no later append or eviction changes them, and false
// when no running run carries it.
func (l *RunEventLog) After(dispatchID string, cursor int64) (RunEventPage, bool) {
	if l == nil {
		return RunEventPage{}, false
	}
	l.mu.Lock()
	buffer, ok := l.buffers[dispatchID]
	l.mu.Unlock()
	if !ok {
		return RunEventPage{}, false
	}
	return buffer.pageAfter(cursor), true
}

// register creates the buffer of a run that has just started. It returns nil
// on a nil log, which retains nothing.
func (l *RunEventLog) register(dispatchID string) *runEventBuffer {
	if l == nil {
		return nil
	}
	buffer := &runEventBuffer{}
	l.mu.Lock()
	l.buffers[dispatchID] = buffer
	l.mu.Unlock()
	return buffer
}

// release forgets a run's buffer. It never mutates the buffer, so a reader
// that already holds it copies a complete list.
func (l *RunEventLog) release(dispatchID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.buffers, dispatchID)
	l.mu.Unlock()
}

// runEventBuffer is one running run's bounded event list. The event loop is
// its only writer; any goroutine may read it through snapshot.
type runEventBuffer struct {
	mu            sync.Mutex
	events        []RunEvent // live events are events[head:]; guarded by mu
	head          int        // guarded by mu
	bytes         int64      // guarded by mu
	nextSeq       int64      // guarded by mu
	startupFailed bool       // guarded by mu
}

func (b *runEventBuffer) snapshot() []RunEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := b.events[b.head:]
	events := make([]RunEvent, len(live))
	copy(events, live)
	return events
}

func (b *runEventBuffer) pageAfter(cursor int64) RunEventPage {
	b.mu.Lock()
	defer b.mu.Unlock()
	page := PageAfter(b.events[b.head:], cursor)
	page.Events = slices.Clone(page.Events)
	return page
}

func (b *runEventBuffer) hasStartupFailed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.startupFailed
}

// storeRows returns the buffer's events in stored form, oldest first.
func (b *runEventBuffer) storeRows() []storage.RunEvent {
	events := b.snapshot()
	rows := make([]storage.RunEvent, len(events))
	for i, e := range events {
		rows[i] = e.storeRow()
	}
	return rows
}

// repeatsNewest reports whether e restates the newest retained event, which
// only the repeatable types can: an adapter that resends the same
// notification adds nothing the operator has not seen.
func (b *runEventBuffer) repeatsNewest(e RunEvent) bool {
	switch e.Event {
	case domain.EventNotification, domain.EventOtherMessage, domain.EventMalformed:
	default:
		return false
	}
	if b.head == len(b.events) {
		return false
	}
	newest := b.events[len(b.events)-1]
	return newest.Event == e.Event && newest.Message == e.Message
}

// retain appends e unless it repeats the newest event, then evicts the oldest
// events while either bound is exceeded. Append and eviction are O(1)
// amortized.
func (b *runEventBuffer) retain(e RunEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.repeatsNewest(e) {
		return
	}
	b.nextSeq++
	e.Seq = b.nextSeq
	b.events = append(b.events, e)
	b.bytes += e.size
	if e.Event == domain.EventStartupFailed {
		b.startupFailed = true
	}

	for len(b.events)-b.head > 1 && (len(b.events)-b.head > maxRetainedEvents || b.bytes > maxRetainedEventBytes) {
		b.bytes -= b.events[b.head].size
		b.events[b.head] = RunEvent{}
		b.head++
	}
	if live := len(b.events) - b.head; b.head >= live {
		copy(b.events, b.events[b.head:])
		clear(b.events[live:])
		b.events = b.events[:live]
		b.head = 0
	}
}

// retainEvent adds event to the run's buffer. It does nothing for an entry
// with no buffer, a token_usage event, and a notification or other message
// whose text is blank, because those carry nothing to show an operator.
func retainEvent(entry *RunningEntry, event domain.AgentEvent) {
	if entry.events == nil || event.Type == domain.EventTokenUsage {
		return
	}
	if (event.Type == domain.EventNotification || event.Type == domain.EventOtherMessage) &&
		strings.TrimSpace(event.Message) == "" {
		return
	}

	retained := RunEvent{
		At:      event.Timestamp.UTC(),
		Event:   event.Type,
		Message: redact.Truncate(event.Message, retainedMessageRunes),
	}
	switch {
	case event.Type == domain.EventSessionStarted:
		retained.SessionID = event.SessionID
	case event.Type == domain.EventToolResult:
		retained.ToolName = event.ToolName
		retained.ToolKind = event.ToolKind
		retained.ToolArgument = event.ToolArgument
		retained.DurationMS = event.ToolDurationMS
		retained.ToolError = event.ToolError
	case isTurnTerminalEvent(event.Type) && entry.UsageMeasured:
		retained.Usage = &domain.TokenUsage{
			InputTokens:      entry.AgentInputTokens,
			OutputTokens:     entry.AgentOutputTokens,
			TotalTokens:      entry.AgentTotalTokens,
			CacheReadTokens:  entry.CacheReadTokens,
			CacheWriteTokens: entry.CacheWriteTokens,
		}
	}
	entry.events.retain(retained.withSize())
}

// HeldRunEvents is one run's events awaiting a retry.
type HeldRunEvents struct {
	IssueID     string
	Identifier  string
	DispatchID  string
	CompletedAt string // The run's run_history.completed_at.
	Events      []storage.RunEvent
}

func (h HeldRunEvents) size() int64 {
	var total int64
	for _, e := range h.Events {
		total += e.Size
	}
	return total
}

// heldRun is the retry's bookkeeping for one held run.
type heldRun struct {
	run     HeldRunEvents
	bytes   int64
	retries int
	lastErr error
	timer   *time.Timer
	log     *slog.Logger
}

// RunEventRetry holds the events of runs whose events part failed and retries
// writing them off the event loop. It is safe for concurrent use.
type RunEventRetry struct {
	store  storage.RunEventStore
	logger *slog.Logger
	delays []time.Duration
	ctx    context.Context
	cancel context.CancelFunc
	writes sync.WaitGroup

	mu        sync.Mutex
	held      []*heldRun // oldest first; guarded by mu
	heldBytes int64      // guarded by mu
}

// NewRunEventRetry returns a retry component that writes through store and
// logs to logger, which nil defaults to [slog.Default].
func NewRunEventRetry(store storage.RunEventStore, logger *slog.Logger) *RunEventRetry {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &RunEventRetry{
		store:  store,
		logger: logger,
		delays: defaultRunEventRetryDelays,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Hold keeps run's events and schedules their first retry. It performs no
// I/O and never waits for a retry, so the event loop may call it.
func (r *RunEventRetry) Hold(run HeldRunEvents) {
	h := &heldRun{
		run:   run,
		bytes: run.size(),
		log:   logging.WithIssue(r.logger, run.IssueID, run.Identifier),
	}

	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		logDroppedRunEvents(h, errRunEventRetryStopped)
		return
	}
	var dropped []*heldRun
	for r.heldBytes+h.bytes > maxHeldRunEventBytes && len(r.held) > 0 {
		dropped = append(dropped, r.held[0])
		r.removeLocked(r.held[0])
	}
	r.held = append(r.held, h)
	r.heldBytes += h.bytes
	h.timer = time.AfterFunc(r.delays[0], func() { r.retry(h) })
	r.mu.Unlock()

	for _, d := range dropped {
		logDroppedRunEvents(d, errRunEventHeldFull)
	}
}

// Stop drops every held run and returns once no retry is writing. A nil
// receiver holds nothing and stops at once.
func (r *RunEventRetry) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	dropped := r.held
	for _, h := range dropped {
		h.timer.Stop()
	}
	r.held = nil
	r.heldBytes = 0
	r.mu.Unlock()

	r.cancel()
	r.writes.Wait()

	for _, h := range dropped {
		logDroppedRunEvents(h, errRunEventRetryStopped)
	}
}

// retry writes a held run's events on the timer's goroutine and decides, once
// the write returns, whether the run is released, retried, or dropped. The
// run may have been dropped meanwhile, in which case the outcome is ignored.
func (r *RunEventRetry) retry(h *heldRun) {
	r.mu.Lock()
	if !r.isHeldLocked(h) {
		r.mu.Unlock()
		return
	}
	r.writes.Add(1)
	r.mu.Unlock()
	defer r.writes.Done()

	err := r.store.AppendRunEvents(r.ctx, h.run.DispatchID, h.run.CompletedAt, h.run.Events, runEventStoreLimits)

	r.mu.Lock()
	if !r.isHeldLocked(h) {
		r.mu.Unlock()
		return
	}
	h.retries++
	h.lastErr = err
	switch {
	case err == nil:
		r.removeLocked(h)
		r.mu.Unlock()
		h.log.Info("stored run events on retry",
			slog.String("dispatch_id", h.run.DispatchID),
			slog.Int("events", len(h.run.Events)),
			slog.Int("attempt", h.retries),
		)
	case h.retries < len(r.delays):
		h.timer.Reset(r.delays[h.retries])
		r.mu.Unlock()
	default:
		r.removeLocked(h)
		r.mu.Unlock()
		logDroppedRunEvents(h, err)
	}
}

func (r *RunEventRetry) isHeldLocked(h *heldRun) bool {
	return slices.Contains(r.held, h)
}

func (r *RunEventRetry) removeLocked(h *heldRun) {
	h.timer.Stop()
	if i := slices.Index(r.held, h); i >= 0 {
		r.held = slices.Delete(r.held, i, i+1)
		r.heldBytes -= h.bytes
	}
}

var (
	errRunEventHeldFull     = errors.New("held run events exceed their memory bound")
	errRunEventRetryStopped = errors.New("run event retry stopped")
)

// logDroppedRunEvents reports events that will never be stored. It carries
// the store's latest error when a retry failed, else reason.
func logDroppedRunEvents(h *heldRun, reason error) {
	cause := reason
	if h.lastErr != nil {
		cause = h.lastErr
	}
	h.log.Warn("dropped run events",
		slog.String("dispatch_id", h.run.DispatchID),
		slog.Int("events", len(h.run.Events)),
		slog.Int("attempts", h.retries),
		slog.Any("error", cause),
	)
}
