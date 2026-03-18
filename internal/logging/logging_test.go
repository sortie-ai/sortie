package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/sergeyklay/sortie/internal/logging"
)

func TestSetup(t *testing.T) {
	var buf bytes.Buffer
	logging.Setup(&buf, slog.LevelInfo)

	slog.Default().Info("startup complete")
	output := buf.String()

	if !strings.Contains(output, "startup complete") {
		t.Errorf("expected log output to contain message, got: %s", output)
	}

	buf.Reset()
	slog.Default().Debug("should be filtered")
	if buf.Len() != 0 {
		t.Errorf("expected DEBUG message to be filtered at INFO level, got: %s", buf.String())
	}
}

func TestWithIssue(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	issueLogger := logging.WithIssue(logger, "10042", "PROJ-123")
	issueLogger.Info("processing issue")

	output := buf.String()

	if !strings.Contains(output, "issue_id=10042") {
		t.Errorf("expected issue_id=10042 in output, got: %s", output)
	}
	if !strings.Contains(output, "issue_identifier=PROJ-123") {
		t.Errorf("expected issue_identifier=PROJ-123 in output, got: %s", output)
	}
}

func TestWithSession(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	sessionLogger := logging.WithSession(logger, "sess-abc-def")
	sessionLogger.Info("session started")

	output := buf.String()

	if !strings.Contains(output, "session_id=sess-abc-def") {
		t.Errorf("expected session_id=sess-abc-def in output, got: %s", output)
	}
}

func TestWithIssueAndSession(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	combined := logging.WithSession(logging.WithIssue(logger, "10042", "PROJ-123"), "sess-abc-def")
	combined.Info("dispatching agent")

	output := buf.String()

	for _, key := range []string{"issue_id=10042", "issue_identifier=PROJ-123", "session_id=sess-abc-def"} {
		if !strings.Contains(output, key) {
			t.Errorf("expected %s in output, got: %s", key, output)
		}
	}
}

func TestWithIssue_SpecialValues(t *testing.T) {
	tests := []struct {
		name            string
		issueID         string
		issueIdentifier string
		wantID          string
		wantIdentifier  string
	}{
		{
			name:            "spaces are quoted",
			issueID:         "100 42",
			issueIdentifier: "PROJ 123",
			wantID:          `issue_id="100 42"`,
			wantIdentifier:  `issue_identifier="PROJ 123"`,
		},
		{
			name:            "double quotes are escaped",
			issueID:         `say "hello"`,
			issueIdentifier: `PROJ-"X"`,
			wantID:          `issue_id="say \"hello\""`,
			wantIdentifier:  `issue_identifier="PROJ-\"X\""`,
		},
		{
			name:            "equals sign is quoted",
			issueID:         "a=b",
			issueIdentifier: "X=Y",
			wantID:          `issue_id="a=b"`,
			wantIdentifier:  `issue_identifier="X=Y"`,
		},
		{
			name:            "unicode preserved",
			issueID:         "задача-1",
			issueIdentifier: "プロジェクト-42",
			wantID:          `issue_id=задача-1`,
			wantIdentifier:  `issue_identifier=プロジェクト-42`,
		},
		{
			name:            "newline is escaped",
			issueID:         "line1\nline2",
			issueIdentifier: "a\nb",
			wantID:          `issue_id="line1\nline2"`,
			wantIdentifier:  `issue_identifier="a\nb"`,
		},
		{
			name:            "empty values",
			issueID:         "",
			issueIdentifier: "",
			wantID:          `issue_id=""`,
			wantIdentifier:  `issue_identifier=""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			issueLogger := logging.WithIssue(logger, tt.issueID, tt.issueIdentifier)
			issueLogger.Info("test")

			output := buf.String()

			if !strings.Contains(output, tt.wantID) {
				t.Errorf("expected %s in output, got: %s", tt.wantID, output)
			}
			if !strings.Contains(output, tt.wantIdentifier) {
				t.Errorf("expected %s in output, got: %s", tt.wantIdentifier, output)
			}
		})
	}
}

func TestWithSession_SpecialValues(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		want      string
	}{
		{
			name:      "spaces are quoted",
			sessionID: "sess abc",
			want:      `session_id="sess abc"`,
		},
		{
			name:      "double quotes are escaped",
			sessionID: `sess-"x"`,
			want:      `session_id="sess-\"x\""`,
		},
		{
			name:      "empty value",
			sessionID: "",
			want:      `session_id=""`,
		},
		{
			name:      "unicode preserved",
			sessionID: "сессия-αβγ",
			want:      `session_id=сессия-αβγ`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			sessionLogger := logging.WithSession(logger, tt.sessionID)
			sessionLogger.Info("test")

			output := buf.String()

			if !strings.Contains(output, tt.want) {
				t.Errorf("expected %s in output, got: %s", tt.want, output)
			}
		})
	}
}
