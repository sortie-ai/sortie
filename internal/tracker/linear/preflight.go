package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// preflightBackoff is the bounded exponential backoff applied to transient
// preflight failures. A config error fails construction immediately with no
// retry; these delays absorb a brief outage before construction fails.
//
// preflightBackoff is a package variable, not a [httpkit.RetryWithBackoff]
// argument baked in at each call site, so a test can substitute a fast
// schedule for the retry-exhaustion subtests.
var preflightBackoff = httpkit.DefaultPreflightBackoff()

// terminalStateTypes are the workflow-state categories that close an issue.
var terminalStateTypes = map[string]struct{}{
	"completed": {},
	"canceled":  {},
	"duplicate": {},
}

// runPreflight validates credentials and the configured state names against
// the team's workflow states, returning a cache mapping each lowercase state
// name to its canonical casing.
//
// It runs the viewer credential check and the team-states query through client.
// A config error (auth, payload) fails immediately; a transient error
// (transport, API) is retried with a bounded backoff before failing. An unknown
// team key or a configured state name absent from the team is a payload error.
// A configured name whose category contradicts its list emits a WARN without
// failing.
func runPreflight(ctx context.Context, client graphQLClient, project string, active, terminal []string, handoff string, log *slog.Logger) (map[string]string, error) {
	if err := httpkit.RetryWithBackoff(ctx, preflightBackoff, func() error {
		return checkViewer(ctx, client)
	}); err != nil {
		return nil, err
	}

	var states []teamState
	if err := httpkit.RetryWithBackoff(ctx, preflightBackoff, func() error {
		var fetchErr error
		states, fetchErr = fetchTeamStates(ctx, client, project)
		return fetchErr
	}); err != nil {
		return nil, err
	}

	casingCache := make(map[string]string, len(states))
	typeByLower := make(map[string]string, len(states))
	for _, state := range states {
		lower := strings.ToLower(state.Name)
		casingCache[lower] = state.Name
		typeByLower[lower] = state.Type
	}

	configured := make([]string, 0, len(active)+len(terminal)+1)
	configured = append(configured, active...)
	configured = append(configured, terminal...)
	if handoff != "" {
		configured = append(configured, handoff)
	}

	for _, name := range configured {
		if _, ok := casingCache[strings.ToLower(name)]; !ok {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("state %q not found in team %q", name, project),
			}
		}
	}

	warnWrongCategory(active, terminal, typeByLower, log)

	return casingCache, nil
}

// warnWrongCategory emits a WARN when a configured active state has a terminal
// category, or a configured terminal state has a non-terminal category. The
// categories do not drive selection, so a mismatch is a tripwire, not an error.
func warnWrongCategory(active, terminal []string, typeByLower map[string]string, log *slog.Logger) {
	if log == nil {
		return
	}
	for _, name := range active {
		stateType := typeByLower[strings.ToLower(name)]
		if _, isTerminal := terminalStateTypes[stateType]; isTerminal {
			log.Warn("active state has terminal category",
				slog.String("state", name),
				slog.String("category", stateType))
		}
	}
	for _, name := range terminal {
		stateType := typeByLower[strings.ToLower(name)]
		if _, isTerminal := terminalStateTypes[stateType]; !isTerminal {
			log.Warn("terminal state has non-terminal category",
				slog.String("state", name),
				slog.String("category", stateType))
		}
	}
}

// checkViewer runs the credential-check query and classifies any error.
func checkViewer(ctx context.Context, client graphQLClient) error {
	body, _, err := client.Execute(ctx, queryViewer, nil)
	if err != nil {
		return err
	}
	var resp graphQLResponse[viewerData]
	if err := json.Unmarshal(body, &resp); err != nil {
		return payloadError(err)
	}
	return classifyGraphQLErrors(resp.Errors)
}

// fetchTeamStates runs the team-states query and returns the team's workflow
// states. A team with no matching node is an unknown-team payload error.
func fetchTeamStates(ctx context.Context, client graphQLClient, project string) ([]teamState, error) {
	body, _, err := client.Execute(ctx, queryTeamStates, map[string]any{"teamKey": project})
	if err != nil {
		return nil, err
	}
	var resp graphQLResponse[teamStatesData]
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, payloadError(err)
	}
	if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
		return nil, classified
	}
	if len(resp.Data.Teams.Nodes) == 0 {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("unknown team key %q", project),
		}
	}
	return resp.Data.Teams.Nodes[0].States.Nodes, nil
}
