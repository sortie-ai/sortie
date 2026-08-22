// Package blockers resolves the blocker list of a candidate issue
// whose tracker adapter cannot carry one on the candidate path. Start
// with [Resolver] and [NewResolver].
package blockers

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Resolver completes the blocker list of one issue according to the
// blocker source its tracker adapter declared. It is safe for
// concurrent use and holds no per-tick state.
type Resolver struct {
	adapter domain.TrackerAdapter
	source  registry.BlockerSource
	reader  domain.BlockerReader
	logger  *slog.Logger
}

// NewResolver returns a Resolver for one tracker adapter and the
// blocker source that adapter declared. A nil adapter yields a nil
// Resolver, which resolves nothing.
func NewResolver(adapter domain.TrackerAdapter, source registry.BlockerSource, logger *slog.Logger) *Resolver {
	if adapter == nil {
		return nil
	}

	reader, _ := adapter.(domain.BlockerReader)

	return &Resolver{
		adapter: adapter,
		source:  source,
		reader:  reader,
		logger:  logger,
	}
}

// NeedsRead reports whether Resolve would issue a tracker request for
// issue. It answers from the declared source and the issue's own
// flag, makes no call, and is the same two inputs Resolve decides on,
// so a caller can price a candidate without learning the source.
func (r *Resolver) NeedsRead(issue domain.Issue) bool {
	if r == nil {
		return false
	}
	if r.source != registry.BlockersPerIssue {
		return false
	}
	return issue.BlockersUnresolved
}

// Resolve returns issue with an authoritative BlockedBy, or with
// BlockersUnresolved set when it could not produce one. It reads only
// when the issue still needs reading: an issue whose producer already
// resolved the list, cheaply or otherwise, costs no request. The
// returned issue is always safe to gate on: a caller that ignores the
// error still holds the issue, because the flag rather than the error
// is what the gate reads.
func (r *Resolver) Resolve(ctx context.Context, issue domain.Issue) (domain.Issue, error) {
	if r == nil {
		return issue, nil
	}
	if r.source != registry.BlockersPerIssue {
		return issue, nil
	}
	if !issue.BlockersUnresolved {
		return issue, nil
	}
	if r.reader == nil {
		issue.BlockersUnresolved = true
		return issue, domain.ErrNoBlockerReader
	}

	blockers, err := r.reader.FetchIssueBlockers(ctx, issue.ID)
	if err != nil {
		issue.BlockersUnresolved = true
		return issue, err
	}

	issue.BlockedBy = blockers
	issue.BlockersUnresolved = false
	return issue, nil
}
