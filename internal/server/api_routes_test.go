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
	pattern     string              // the pattern New passes to Handle or HandleFunc, verbatim
	tag         string              // the group a viewer lists the route under; one of apiTags
	summary     string              // the route's title in a viewer
	description string              // what the route does, shown under the title
	method      string              // the one method the route serves
	query       []string            // query parameter names, in order
	headers     []string            // request header names, in order
	params      map[string]apiParam // keyed by query parameter or request header name
	responses   []apiResponse
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
		description: "The id of the last frame received. It holds the state revision, then a semicolon and dispatch_id:sequence for each subscribed run. A sequence given here replaces the one the run parameter gives for the same run.",
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

const (
	tagState   = "State"
	tagRuns    = "Run history"
	tagUsage   = "Statistics and prices"
	tagActions = "Operator actions"
	tagHealth  = "Health probes"
)

// apiTags lists the groups in the order a viewer shows them.
var apiTags = []string{tagState, tagRuns, tagUsage, tagActions, tagHealth}

var apiRoutes = []apiRoute{
	{
		pattern:     "GET /livez",
		tag:         tagHealth,
		summary:     "Liveness probe",
		description: "Returns `200` while the process is alive and `503` once it is draining.",
		method:      http.MethodGet,
		responses:   jsonResponses(reflect.TypeFor[healthResponse](), http.StatusOK, http.StatusServiceUnavailable),
	},
	{
		pattern:     "GET /readyz",
		tag:         tagHealth,
		summary:     "Readiness probe",
		description: "Reports whether Sortie is ready to work by checking database connectivity, preflight configuration and workflow loading. Use it for readiness probes and load balancer health checks.",
		method:      http.MethodGet,
		responses:   jsonResponses(reflect.TypeFor[readyResponse](), http.StatusOK, http.StatusServiceUnavailable),
	},
	{
		pattern:     "/api/v1/state",
		tag:         tagState,
		summary:     "System state",
		description: "Returns a snapshot of the orchestrator. It lists running sessions, the retry queue, issues that are parked, out of budget, waiting on a pull request reaction or between stages, and what needs a person, along with totals, rate limits, the workflow configuration in effect and the health of Sortie's own checks.",
		method:      http.MethodGet,
		responses:   jsonResponses(reflect.TypeFor[stateResponse](), http.StatusOK),
	},
	{
		pattern:     "/api/v1/runs",
		tag:         tagRuns,
		summary:     "Run history",
		description: "Returns finished runs, newest first, one page at a time. Each filter narrows the result before the page is cut.",
		method:      http.MethodGet,
		query:       runsParams,
		params:      runsQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runsResponse](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/runs/issues",
		tag:         tagRuns,
		summary:     "Run history by issue",
		description: "Returns run history grouped by issue, one page of issues at a time, the issue with the newest matching run first. Each issue carries its newest runs, its totals and a sentence that says what it is doing now or how it ended.",
		method:      http.MethodGet,
		query:       issueGroupsParams,
		params:      issueGroupsQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[issueGroupsResponse](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/runs/{dispatch_id}/events",
		tag:         tagRuns,
		summary:     "Run events",
		description: "Returns the agent events Sortie kept for one run, oldest first, each with a plain English sentence. To receive the same events as they arrive, name the run in the `run` parameter of `GET /api/v1/events`.",
		method:      http.MethodGet,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runEventsResponse](), http.StatusOK),
			errorResponses(http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/stats",
		tag:         tagUsage,
		summary:     "Run statistics",
		description: "Returns the report that `sortie stats --format json` prints. It covers run and issue counts, success rate, duration percentiles, token sums and estimated cost over the runs completed in a range, with tokens and cost per UTC period and breakdowns such as outcome, coding agent and model.",
		method:      http.MethodGet,
		query:       statsParams,
		params:      statsQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runstats.Report](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/stats/span",
		tag:         tagUsage,
		summary:     "Sessions behind a stretch of spend",
		description: "Returns the totals of a window of time and the sessions that spent in it, each with its share of the window's cost and tokens. Use it to see what is behind a stretch of the `spend` series of `GET /api/v1/stats`.",
		method:      http.MethodGet,
		query:       spanParams,
		params:      spanQuery,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[runstats.SpanReport](), http.StatusOK),
			errorResponses(http.StatusBadRequest, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/prices",
		tag:         tagUsage,
		summary:     "Prices in force",
		description: "Returns the rates in force when the request is served, per agent kind and model, with the layer each rate comes from and the state of every price source. The route prices no run, because a run keeps the cost recorded when it finished.",
		method:      http.MethodGet,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[pricesResponse](), http.StatusOK),
			errorResponses(http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/refresh",
		tag:         tagActions,
		summary:     "Trigger a poll cycle",
		description: "Queues an immediate poll and reconciliation cycle, so Sortie picks up new issues without waiting for the next poll interval.",
		method:      http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[refreshResponse](), http.StatusAccepted, http.StatusConflict),
			errorResponses(http.StatusForbidden),
		),
	},
	{
		pattern:     "/api/v1/retries/{identifier}/run",
		tag:         tagActions,
		summary:     "Run a scheduled retry now",
		description: "Makes the scheduled retry of an issue due now, so it runs without waiting out its delay. `{identifier}` is the `issue_identifier` of an entry in the `retrying` list of `GET /api/v1/state`.",
		method:      http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[retryNowResponse](), http.StatusAccepted, http.StatusConflict),
			errorResponses(http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/attention/{id}/acknowledge",
		tag:         tagActions,
		summary:     "Acknowledge an attention item",
		description: "Records that a person has seen an item of the `attention` list of `GET /api/v1/state`. `{id}` is the `id` of a current item.",
		method:      http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[acknowledgeResponse](), http.StatusOK, http.StatusConflict),
			errorResponses(http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/reactions/{identifier}/{kind}/check",
		tag:         tagActions,
		summary:     "Check a pending reaction now",
		description: "Makes the next check of a pending pull request reaction due now, so it runs on the next poll tick instead of at its scheduled time. `{identifier}` and `{kind}` are the `issue_identifier` and `kind` of a row of `pending_reactions` in `GET /api/v1/state`.",
		method:      http.MethodPost,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[checkNowResponse](), http.StatusAccepted, http.StatusConflict),
			errorResponses(http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
	{
		pattern:     "/api/v1/events",
		tag:         tagState,
		summary:     "Change signals and agent events",
		description: "Streams a signal each time something the API serves changes, so a client can refetch what it shows instead of polling, and streams the agent events of each run named with `run`. The server-sent events stream stays open until the client disconnects or the server shuts down. A change signal carries no data.",
		method:      http.MethodGet,
		query:       []string{"run"},
		headers:     []string{"Last-Event-ID"},
		params:      eventsQuery,
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
		pattern:     "/api/v1/{identifier}",
		tag:         tagState,
		summary:     "Issue detail",
		description: "Returns runtime and debug details of one issue and its run history. `{identifier}` is the issue identifier, such as `MT-649`, not the internal issue ID. An issue that is no longer active is answered from run history.",
		method:      http.MethodGet,
		responses: slices.Concat(
			jsonResponses(reflect.TypeFor[issueDetailResponse](), http.StatusOK),
			errorResponses(http.StatusNotFound, http.StatusServiceUnavailable),
		),
	},
}
