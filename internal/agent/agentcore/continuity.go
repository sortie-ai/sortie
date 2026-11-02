package agentcore

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// ResumeVerdict is what a kind's runtime signal says about the session a start
// was asked to continue. The zero value is ResumeUnknown.
type ResumeVerdict uint8

const (
	// ResumeUnknown means the check ran and the signal did not say.
	ResumeUnknown ResumeVerdict = iota
	// ResumeCarried means the runtime holds the session and continues it with
	// its history.
	ResumeCarried
	// ResumeReplaced means the runtime holds no session under the id, or
	// declines to continue it.
	ResumeReplaced
)

// String returns "unknown", "carried", or "replaced".
func (v ResumeVerdict) String() string {
	switch v {
	case ResumeCarried:
		return "carried"
	case ResumeReplaced:
		return "replaced"
	default:
		return "unknown"
	}
}

// ResumeSignal is a verdict, the runtime's answer when it continued the
// session, and what the runtime reported or why the check got no answer.
type ResumeSignal[A any] struct {
	Verdict ResumeVerdict
	Answer  A     // what the continuing call returned; set only with ResumeCarried
	Cause   error // nil when there is nothing to add; never set with ResumeCarried
}

// SessionStart is how a session starts: continuing the session it was asked to
// resume, with the runtime's answer, or new. The zero value is a new session.
type SessionStart[A any] struct {
	resumeID string
	answer   A
}

// ResumeProbe is the seam every agent kind implements: it reads the kind's own
// runtime signal for the session resumeID names. A non-nil error means the
// agent connection the check ran over ended; every other outcome is a signal.
type ResumeProbe[A any] interface {
	ProbeResume(ctx context.Context, resumeID string) (ResumeSignal[A], *domain.AgentError)
}

// ResumeProbeFunc adapts a function to [ResumeProbe].
type ResumeProbeFunc[A any] func(ctx context.Context, resumeID string) (ResumeSignal[A], *domain.AgentError)

// ProbeResume calls f.
func (f ResumeProbeFunc[A]) ProbeResume(ctx context.Context, resumeID string) (ResumeSignal[A], *domain.AgentError) {
	return f(ctx, resumeID)
}

// ResolveSessionStart decides whether a start continues resumeID, calling the
// probe at most once. It returns a start failure when the start's context
// ended or the check's transport was lost, and otherwise logs one warning for
// a session that did not continue.
//
// The ctx MUST be the context StartSession received: a probe bounds its own
// read on a context it derives from the one it is given, so reaching its own
// bound is [ResumeUnknown] and never an ended start context.
func ResolveSessionStart[A any](ctx context.Context, resumeID string, probe ResumeProbe[A], logger *slog.Logger) (SessionStart[A], *domain.AgentError) {
	if ctx.Err() != nil {
		return SessionStart[A]{}, startCancelled(ctx)
	}
	if resumeID == "" {
		return SessionStart[A]{}, nil
	}
	signal, err := probe.ProbeResume(ctx, resumeID)
	if ctx.Err() != nil {
		return SessionStart[A]{}, startCancelled(ctx)
	}
	if err != nil {
		return SessionStart[A]{}, err
	}
	if errors.Is(signal.Cause, sshutil.ErrConnectionFailed) {
		return SessionStart[A]{}, ConnectionFailedError()
	}
	if signal.Verdict == ResumeCarried {
		return SessionStart[A]{resumeID: resumeID, answer: signal.Answer}, nil
	}
	attrs := []any{slog.String("resume_id", resumeID), slog.String("resume_verdict", signal.Verdict.String())}
	if signal.Cause != nil {
		attrs = append(attrs, slog.Any("error", signal.Cause))
	}
	logger.Warn("session resume not continued, starting new session", attrs...)
	return SessionStart[A]{}, nil
}

// ResumeID returns the continued id, empty for a new session.
func (s SessionStart[A]) ResumeID() string { return s.resumeID }

// CarriedHistory reports whether the session continues the one it was asked to
// resume.
func (s SessionStart[A]) CarriedHistory() bool { return s.resumeID != "" }

// Answer returns the probe's answer when the session continues, the zero value
// for a new session.
func (s SessionStart[A]) Answer() A { return s.answer }

// AssignedID returns the continued id, or a new v4 UUID for a kind that names
// its own sessions.
func (s SessionStart[A]) AssignedID() (string, *domain.AgentError) {
	if s.resumeID != "" {
		return s.resumeID, nil
	}
	id, err := NewUUIDv4()
	if err != nil {
		return "", &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "could not generate a session id",
			Err:     err,
		}
	}
	return id, nil
}

// RuntimeFileProbe returns the probe of a runtime that keeps each session in a
// file under its home: a present file continues the session and is the answer,
// an absent file or home replaces it, and a failed read leaves the verdict
// unknown. The read is bounded by bound on a context derived inside the probe.
func RuntimeFileProbe(target LaunchTarget, bound time.Duration, file func(sessionID string) RuntimePath) ResumeProbe[RuntimePath] {
	return ResumeProbeFunc[RuntimePath](func(ctx context.Context, resumeID string) (ResumeSignal[RuntimePath], *domain.AgentError) {
		ctx, cancel := context.WithTimeout(ctx, bound)
		defer cancel()
		matches, err := target.MatchRuntimeFiles(ctx, nil, file(resumeID))
		switch {
		case err != nil:
			return ResumeSignal[RuntimePath]{Cause: err}, nil
		case len(matches) > 0:
			return ResumeSignal[RuntimePath]{Verdict: ResumeCarried, Answer: matches[0]}, nil
		default:
			return ResumeSignal[RuntimePath]{Verdict: ResumeReplaced}, nil
		}
	})
}
