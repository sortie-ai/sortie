package server

import (
	"net/http"
	"reflect"
	"slices"

	"github.com/sortie-ai/sortie/internal/runstats"
)

// apiRoute describes one pattern New registers, for the generated OpenAPI
// document.
type apiRoute struct {
	pattern   string              // the pattern New passes to Handle or HandleFunc, verbatim
	method    string              // the one method the route serves
	query     []string            // query parameter names, in order
	headers   []string            // request header names, in order
	params    map[string]apiParam // keyed by query parameter or request header name
	responses []apiResponse
}

type apiParam struct {
	required    bool
	description string
	schema      map[string]any
}

type apiResponse struct {
	status int
	body   reflect.Type // the application/json body; nil for a stream
	events []apiEvent   // the text/event-stream frames; nil for a JSON body
}

type apiEvent struct {
	name string       // the frame's event field
	data reflect.Type // Go type of the JSON in the data field; nil for a bare JSON object
}

var (
	textParam    = apiParam{schema: map[string]any{"type": "string"}}
	instantParam = apiParam{schema: map[string]any{"type": "string", "format": "date-time"}}
	boundParam   = apiParam{
		description: "An RFC 3339 timestamp, a YYYY-MM-DD date read as midnight UTC, or a positive duration such as 24h read as that long before now.",
		schema:      textParam.schema,
	}
)

func limitParam(upper int) apiParam {
	return apiParam{schema: map[string]any{"type": "integer", "minimum": 1, "maximum": upper}}
}

var runsQuery = map[string]apiParam{
	"issue":       textParam,
	"issue_id":    textParam,
	"rule":        textParam,
	"status":      textParam,
	"stage":       textParam,
	"cursor":      textParam,
	"since":       instantParam,
	"until":       instantParam,
	"ended_since": instantParam,
	"ended_until": instantParam,
	"limit":       limitParam(maxRunsLimit),
}

var issueGroupsQuery = map[string]apiParam{
	"issue":  textParam,
	"q":      textParam,
	"rule":   textParam,
	"status": textParam,
	"stage":  textParam,
	"cursor": textParam,
	"limit":  limitParam(maxIssueGroupsLimit),
}

var statsQuery = map[string]apiParam{
	"since":  boundParam,
	"until":  boundParam,
	"period": {schema: map[string]any{"type": "string", "enum": spendPeriodValues()}},
}

var spanQuery = map[string]apiParam{
	"since": {required: true, description: boundParam.description, schema: boundParam.schema},
	"until": boundParam,
}

var eventsQuery = map[string]apiParam{
	"run": {schema: map[string]any{
		"type":     "array",
		"items":    textParam.schema,
		"maxItems": maxRunSubscriptions,
	}},
	"Last-Event-ID": {
		description: "The id of the last frame received: the state revision, then a semicolon and dispatch_id:sequence for each subscribed run. A sequence given here replaces the one the run parameter gives for the same run.",
		schema:      textParam.schema,
	},
}

func jsonResponses(body reflect.Type, statuses ...int) []apiResponse {
	out := make([]apiResponse, len(statuses))
	for i, status := range statuses {
		out[i] = apiResponse{status: status, body: body}
	}
	return out
}

func errorResponses(statuses ...int) []apiResponse {
	return jsonResponses(reflect.TypeFor[errorResponse](), statuses...)
}

var apiRoutes = []apiRoute{
	{
		pattern:   "GET /livez",
		method:    http.MethodGet,
		responses: jsonResponses(reflect.TypeFor[healthResponse](), http.StatusOK, http.StatusServiceUnavailable),
	},
	{
		pattern:   "GET /readyz",
		method:    http.MethodGet,
		responses: jsonResponses(reflect.TypeFor[readyResponse](), http.StatusOK, http.StatusServiceUnavailable),
	},
	{
		pattern:   "/api/v1/state",
		method:    http.MethodGet,
		responses: jsonResponses(reflect.TypeFor[stateResponse](), http.StatusOK),
	},
	{
		pattern: "/api/v1/runs",
		method:  http.MethodGet,
		query:   runsParams,
		params:  runsQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runsResponse](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/runs/issues",
		method:  http.MethodGet,
		query:   issueGroupsParams,
		params:  issueGroupsQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[issueGroupsResponse](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/runs/{dispatch_id}/events",
		method:  http.MethodGet,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runEventsResponse](), http.StatusOK),
			errorResponses(http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/stats",
		method:  http.MethodGet,
		query:   statsParams,
		params:  statsQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runstats.Report](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/stats/span",
		method:  http.MethodGet,
		query:   spanParams,
		params:  spanQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runstats.SpanReport](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/prices",
		method:  http.MethodGet,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[pricesResponse](), http.StatusOK),
			errorResponses(http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/refresh",
		method:  http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[refreshResponse](), http.StatusAccepted, http.StatusConflict),
			errorResponses(http.StatusForbidden),
		),
	},
	{
		pattern: "/api/v1/retries/{identifier}/run",
		method:  http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[retryNowResponse](), http.StatusAccepted, http.StatusConflict),
			errorResponses(http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/attention/{id}/acknowledge",
		method:  http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[acknowledgeResponse](), http.StatusOK, http.StatusConflict),
			errorResponses(http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/reactions/{identifier}/{kind}/check",
		method:  http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[checkNowResponse](), http.StatusAccepted, http.StatusConflict),
			errorResponses(http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
	{
		pattern: "/api/v1/events",
		method:  http.MethodGet,
		query:   []string{"run"},
		headers: []string{"Last-Event-ID"},
		params:  eventsQuery,
		responses: slices.Concat(
			[]apiResponse{{status: http.StatusOK, events: []apiEvent{
				{name: "change"},
				{name: "heartbeat"},
				{name: "agent_event", data: reflect.TypeFor[agentEventFrame]()},
				{name: "run_ended", data: reflect.TypeFor[runEndedFrame]()},
				{name: "reset", data: reflect.TypeFor[runResetFrame]()},
			}}},
			errorResponses(http.StatusBadRequest),
		),
	},
	{
		pattern: "/api/v1/{identifier}",
		method:  http.MethodGet,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[issueDetailResponse](), http.StatusOK),
			errorResponses(http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
}
