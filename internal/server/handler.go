package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// --- Wire types for JSON serialization ---

// stateResponse is the JSON wire format for GET /api/v1/state.
type stateResponse struct {
	GeneratedAt time.Time                        `json:"generated_at"`
	Counts      stateCounts                      `json:"counts"`
	Running     []runningEntryResponse           `json:"running"`
	Retrying    []retryEntryResponse             `json:"retrying"`
	AgentTotals orchestrator.SnapshotAgentTotals `json:"agent_totals"`
	RateLimits  map[string]any                   `json:"rate_limits"`
}

type stateCounts struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

type runningEntryResponse struct {
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	State           string    `json:"state"`
	SessionID       string    `json:"session_id"`
	TurnCount       int       `json:"turn_count"`
	LastEvent       string    `json:"last_event"`
	LastMessage     string    `json:"last_message"`
	StartedAt       time.Time `json:"started_at"`
	LastEventAt     time.Time `json:"last_event_at"`
	WorkspacePath   string    `json:"workspace_path"`
	Tokens          tokenInfo `json:"tokens"`
}

type tokenInfo struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type retryEntryResponse struct {
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	Attempt         int       `json:"attempt"`
	DueAt           time.Time `json:"due_at"`
	Error           string    `json:"error"`
}

type issueDetailResponse struct {
	IssueIdentifier string                `json:"issue_identifier"`
	IssueID         string                `json:"issue_id"`
	Status          string                `json:"status"`
	Workspace       *workspaceInfo        `json:"workspace"`
	Attempts        *attemptsInfo         `json:"attempts"`
	Running         *runningEntryResponse `json:"running"`
	Retry           *retryEntryResponse   `json:"retry"`
	RecentEvents    []any                 `json:"recent_events"`
	LastError       *string               `json:"last_error"`
	Tracked         map[string]any        `json:"tracked"`
}

type workspaceInfo struct {
	Path string `json:"path"`
}

type attemptsInfo struct {
	RestartCount        int `json:"restart_count"`
	CurrentRetryAttempt int `json:"current_retry_attempt"`
}

type refreshResponse struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Wire-type constructors ---

func toRunningEntryResponse(e orchestrator.SnapshotRunningEntry) runningEntryResponse {
	return runningEntryResponse{
		IssueID:         e.IssueID,
		IssueIdentifier: e.Identifier,
		State:           e.State,
		SessionID:       e.SessionID,
		TurnCount:       e.TurnCount,
		LastEvent:       string(e.LastAgentEvent),
		LastMessage:     e.LastAgentMessage,
		StartedAt:       e.StartedAt.UTC(),
		LastEventAt:     e.LastAgentTimestamp.UTC(),
		WorkspacePath:   e.WorkspacePath,
		Tokens: tokenInfo{
			InputTokens:  e.AgentInputTokens,
			OutputTokens: e.AgentOutputTokens,
			TotalTokens:  e.AgentTotalTokens,
		},
	}
}

func toRetryEntryResponse(e orchestrator.SnapshotRetryEntry) retryEntryResponse {
	return retryEntryResponse{
		IssueID:         e.IssueID,
		IssueIdentifier: e.Identifier,
		Attempt:         e.Attempt,
		DueAt:           time.UnixMilli(e.DueAtMS).UTC(),
		Error:           e.Error,
	}
}

func toStateResponse(snap orchestrator.RuntimeSnapshotResult) stateResponse {
	running := make([]runningEntryResponse, 0, len(snap.Running))
	for _, e := range snap.Running {
		running = append(running, toRunningEntryResponse(e))
	}

	retrying := make([]retryEntryResponse, 0, len(snap.Retrying))
	for _, e := range snap.Retrying {
		retrying = append(retrying, toRetryEntryResponse(e))
	}

	rateLimits := snap.RateLimits
	if rateLimits == nil {
		rateLimits = map[string]any{}
	}

	return stateResponse{
		GeneratedAt: snap.GeneratedAt.UTC(),
		Counts: stateCounts{
			Running:  len(running),
			Retrying: len(retrying),
		},
		Running:     running,
		Retrying:    retrying,
		AgentTotals: snap.AgentTotals,
		RateLimits:  rateLimits,
	}
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		logger.Error("failed to marshal JSON response", slog.Any("error", err))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"failed to encode response"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		logger.Warn("failed to write JSON response", slog.Any("error", err))
	}
}

func writeErrorJSON(w http.ResponseWriter, logger *slog.Logger, status int, code string, message string) {
	writeJSON(w, logger, status, errorResponse{
		Error: errorDetail{Code: code, Message: message},
	})
}

// --- Route dispatchers (method enforcement with JSON 405) ---

func (s *Server) routeState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleState(w, r)
}

func (s *Server) routeRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	s.handleRefresh(w, r)
}

func (s *Server) routeIssueDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleIssueDetail(w, r)
}

// --- Handlers ---

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	snap, err := s.snapshotFn()
	if err != nil {
		s.logger.Warn("snapshot request failed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable,
			"snapshot_unavailable",
			"orchestrator state snapshot unavailable")
		return
	}

	resp := toStateResponse(snap)
	writeJSON(w, s.logger, http.StatusOK, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	identifier := r.PathValue("identifier")
	if identifier == "" {
		writeErrorJSON(w, s.logger, http.StatusNotFound,
			"issue_not_found",
			"issue identifier is empty")
		return
	}

	snap, err := s.snapshotFn()
	if err != nil {
		s.logger.Warn("snapshot request failed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable,
			"snapshot_unavailable",
			"orchestrator state snapshot unavailable")
		return
	}

	resp := buildIssueDetail(identifier, snap)
	if resp == nil {
		writeErrorJSON(w, s.logger, http.StatusNotFound,
			"issue_not_found",
			fmt.Sprintf("issue identifier %q not found in current state", identifier))
		return
	}

	writeJSON(w, s.logger, http.StatusOK, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	accepted := s.refreshFn()

	resp := refreshResponse{
		Queued:      true,
		Coalesced:   !accepted,
		RequestedAt: time.Now().UTC(),
		Operations:  []string{"poll", "reconcile"},
	}

	writeJSON(w, s.logger, http.StatusAccepted, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusAccepted),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	s.logger.Warn("method not allowed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
	}
	writeErrorJSON(w, s.logger, http.StatusMethodNotAllowed,
		"method_not_allowed",
		fmt.Sprintf("method %s is not allowed on this endpoint", r.Method))
}

// --- Issue detail builder ---

func buildIssueDetail(identifier string, snap orchestrator.RuntimeSnapshotResult) *issueDetailResponse {
	var runEntry *runningEntryResponse
	var retryEntry *retryEntryResponse

	for _, e := range snap.Running {
		if e.Identifier == identifier {
			re := toRunningEntryResponse(e)
			runEntry = &re
			break
		}
	}

	for _, e := range snap.Retrying {
		if e.Identifier == identifier {
			re := toRetryEntryResponse(e)
			retryEntry = &re
			break
		}
	}

	if runEntry == nil && retryEntry == nil {
		return nil
	}

	var status string
	var issueID string

	switch {
	case runEntry != nil:
		status = "running"
		issueID = runEntry.IssueID
	case retryEntry != nil:
		status = "retrying"
		issueID = retryEntry.IssueID
	default:
		status = "unknown"
	}

	var ws *workspaceInfo
	if runEntry != nil && runEntry.WorkspacePath != "" {
		ws = &workspaceInfo{Path: runEntry.WorkspacePath}
	}

	var attempts *attemptsInfo
	if retryEntry != nil {
		attempts = &attemptsInfo{
			RestartCount:        retryEntry.Attempt,
			CurrentRetryAttempt: retryEntry.Attempt,
		}
	} else {
		attempts = &attemptsInfo{}
	}

	var lastError *string
	if retryEntry != nil && retryEntry.Error != "" {
		lastError = &retryEntry.Error
	}

	return &issueDetailResponse{
		IssueIdentifier: identifier,
		IssueID:         issueID,
		Status:          status,
		Workspace:       ws,
		Attempts:        attempts,
		Running:         runEntry,
		Retry:           retryEntry,
		RecentEvents:    []any{},
		LastError:       lastError,
		Tracked:         map[string]any{},
	}
}
