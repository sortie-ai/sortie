package github

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// graphqlEndpointPath is the path segment appended to the adapter's
// GraphQL-effective base URL to form the GraphQL POST target. For
// github.com it resolves to "/graphql"; for GitHub Enterprise Server it
// resolves to "/api/graphql" once graphqlBasePath has rewritten the
// configured "/api/v3" REST suffix.
const graphqlEndpointPath = "/graphql"

// reviewDecisionQuery is the static GraphQL query used to read the
// authoritative PullRequest.reviewDecision field. Operators MUST NOT
// extend this query without updating the request-shape unit test that
// pins the exact text.
const reviewDecisionQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewDecision
    }
  }
}`

// mergeCommitQuery is the static GraphQL query used to read the merge
// commit identifier for a merged pull request. Operators MUST NOT extend
// this query without updating the request-shape unit test that pins the
// exact text: requesting a field beyond mergeCommit would risk pulling in
// GitHub's test-merge commit through potentialMergeCommit.
const mergeCommitQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      mergeCommit {
        oid
      }
    }
  }
}`

// graphqlRequestBody is the wire shape of a GitHub GraphQL POST body.
type graphqlRequestBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphqlErrorObj captures one entry of the GitHub GraphQL errors array.
type graphqlErrorObj struct {
	Message   string `json:"message"`
	Type      string `json:"type,omitempty"`
	Path      []any  `json:"path,omitempty"`
	Locations []struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"locations,omitempty"`
}

// graphqlResponseEnvelope is the wire shape of a GitHub GraphQL
// response. Data is decoded into the caller-provided generic type;
// Errors is non-empty when GitHub returns HTTP 200 with query-time
// errors.
type graphqlResponseEnvelope[T any] struct {
	Data   T                 `json:"data"`
	Errors []graphqlErrorObj `json:"errors,omitempty"`
}

// reviewDecisionResponseData is the call-specific Data shape for the
// reviewDecisionQuery. The PullRequest field is a pointer so the JSON
// decoder distinguishes a missing pullRequest payload (decoded as nil)
// from a present payload with a null reviewDecision (decoded as a
// non-nil struct with a nil ReviewDecision pointer).
type reviewDecisionResponseData struct {
	Repository struct {
		PullRequest *struct {
			ReviewDecision *string `json:"reviewDecision"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// mergeCommitResponseData is the call-specific Data shape for
// mergeCommitQuery. Both PullRequest and MergeCommit are pointers: a nil
// PullRequest means the payload is missing, and a nil MergeCommit means
// the provider reported no merge commit for a pull request that is
// present.
type mergeCommitResponseData struct {
	Repository struct {
		PullRequest *struct {
			MergeCommit *struct {
				OID string `json:"oid"`
			} `json:"mergeCommit"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// graphqlBasePath returns the GraphQL-effective base URL derived from
// the adapter's configured REST base URL. When the configured base
// ends with "/api/v3" (the GitHub Enterprise Server REST convention),
// the trailing "/v3" is stripped so the GraphQL request lands on
// "/api/graphql". All other base URLs pass through unchanged.
func (a *GitHubSCMAdapter) graphqlBasePath() string {
	trimmed := strings.TrimRight(a.baseURL, "/")
	if strings.HasSuffix(trimmed, "/api/v3") {
		return strings.TrimSuffix(trimmed, "/v3")
	}
	return trimmed
}

// postGraphQL issues a POST to the GraphQL endpoint with the given
// query and variables. The response envelope is decoded into the
// caller-provided pointer. A non-empty Errors slice on a 200 response
// is surfaced as *domain.SCMError with Kind ErrSCMAPI. Transport, auth,
// and other HTTP failures are routed through [scmcore.ToSCMError], which
// performs no conflict promotion, so this read surface never produces
// ErrSCMConflict.
func (a *GitHubSCMAdapter) postGraphQL(ctx context.Context, query string, variables map[string]any, dest envelopeWithErrors) error {
	body := graphqlRequestBody{Query: query, Variables: variables}
	raw, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to encode graphql request body",
			Err:     marshalErr,
		}
	}

	respBody, err := a.graphqlClient.Send(ctx, http.MethodPost, graphqlEndpointPath, bytes.NewReader(raw))
	if err != nil {
		return scmcore.ToSCMError(err)
	}

	if unmarshalErr := json.Unmarshal(respBody, dest); unmarshalErr != nil {
		return &domain.SCMError{
			Kind:    domain.ErrSCMPayload,
			Message: "failed to parse graphql response",
			Err:     unmarshalErr,
		}
	}

	if errs := dest.envelopeErrors(); len(errs) > 0 {
		return &domain.SCMError{
			Kind:    domain.ErrSCMAPI,
			Message: "graphql query returned errors: " + joinErrorMessages(errs),
		}
	}

	return nil
}

// envelopeWithErrors is the interface postGraphQL uses to inspect the
// decoded envelope's Errors slice without referencing the generic
// type parameter of graphqlResponseEnvelope. Each call-site envelope
// satisfies it via the envelopeErrors method declared below.
type envelopeWithErrors interface {
	envelopeErrors() []graphqlErrorObj
}

// envelopeErrors returns the decoded GraphQL errors array. Implements
// envelopeWithErrors.
func (e *graphqlResponseEnvelope[T]) envelopeErrors() []graphqlErrorObj {
	return e.Errors
}

// joinErrorMessages concatenates the Message field of every entry in
// errs with "; " as the separator. Empty messages are skipped so the
// joined output never contains adjacent separators.
func joinErrorMessages(errs []graphqlErrorObj) string {
	messages := make([]string, 0, len(errs))
	for _, e := range errs {
		if e.Message == "" {
			continue
		}
		messages = append(messages, e.Message)
	}
	return strings.Join(messages, "; ")
}

// mapReviewDecision translates the GraphQL PullRequest.reviewDecision
// enum onto domain.ReviewDecision. A nil pointer (the platform's null
// answer) maps to ReviewDecisionNotRequired. Unknown values map to
// ReviewDecisionNotRequired with the bool return set to false so the
// caller can emit a structured warning.
func mapReviewDecision(value *string) (domain.ReviewDecision, bool) {
	if value == nil {
		return domain.ReviewDecisionNotRequired, true
	}
	switch *value {
	case "APPROVED":
		return domain.ReviewDecisionApproved, true
	case "CHANGES_REQUESTED":
		return domain.ReviewDecisionChangesRequested, true
	case "REVIEW_REQUIRED":
		return domain.ReviewDecisionReviewRequired, true
	default:
		return domain.ReviewDecisionNotRequired, false
	}
}
