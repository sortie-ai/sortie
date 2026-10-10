// Package fakemodel provides a loopback model endpoint that answers from a
// fixed script, and a conformance driver that launches an agent adapter's
// real runtime against it.
//
// [Start] serves the scripted answers on 127.0.0.1 and records every request
// as an [Exchange]. [AssertConformance] is the entry point a gated agent
// suite calls with its [Binding]. The package is imported by tests only: its
// non-test files import testing, so production code must not reach it.
package fakemodel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Usage is the token accounting one answer reports.
type Usage struct {
	Prompt        int64 // every input token; CachedContent and CacheWrite are subsets of it
	Candidates    int64 // visible output tokens
	Thoughts      int64 // reasoning output tokens, outside Candidates
	CachedContent int64 // input tokens read from the provider's prompt cache
	CacheWrite    int64 // input tokens written to the provider's prompt cache
}

// Response is one scripted turn answer: Text when Call is nil and Hold is
// false, a function call when Call is set, and a held request when Hold is
// true.
type Response struct {
	Text  string
	Call  ToolChoice
	Hold  bool // keeps the request open and answers nothing until the client disconnects
	Usage Usage
}

// Tool is one function tool a model request declares.
type Tool struct {
	Name       string
	Namespace  string          // the group the request nests the tool in; empty when none
	Parameters json.RawMessage // the argument JSON Schema exactly as declared
}

// FunctionCall is a call resolved against one request's declared tools.
type FunctionCall struct {
	Name      string
	Namespace string          // the called tool's Namespace
	Arguments json.RawMessage // a JSON object
}

// ToolChoice resolves a scripted call against the tools the answered request
// declares.
type ToolChoice func(declared []Tool) (FunctionCall, error)

// Wire names one model API dialect the server answers.
type Wire string

const (
	WireGenerateContent Wire = "generateContent"
	WireMessages        Wire = "messages"
	WireResponses       Wire = "responses"
)

// ExchangeKind classifies how the server answered one request.
type ExchangeKind string

const (
	ExchangeTurn        ExchangeKind = "turn"        // answered from the script
	ExchangeAuxiliary   ExchangeKind = "auxiliary"   // answered outside the script
	ExchangeExhausted   ExchangeKind = "exhausted"   // a turn request after the last script entry
	ExchangeUnmatched   ExchangeKind = "unmatched"   // the entry's ToolChoice matched no declared tool
	ExchangeUnrouted    ExchangeKind = "unrouted"    // no wire routes the method and path
	ExchangeUnsupported ExchangeKind = "unsupported" // a routed request in a shape its wire does not answer
	ExchangeInvalid     ExchangeKind = "invalid"     // encoded body, or a body that is not a JSON object
	ExchangeHeld        ExchangeKind = "held"        // answered by a held answer; nothing written unless the server released it
)

// ToolResult is one tool result a model request carries back.
type ToolResult struct {
	CallID string // the correlation id the request carries back; empty when the wire has none
	Name   string // the tool name, when the wire carries one
	Output string // the result content, text parts joined in order
}

// Answer is what the server wrote in reply to one request.
type Answer struct {
	Status int
	Text   string // the answer text, or the error message of a failed request
	Call   *FunctionCall
	CallID string // correlation id issued with Call; empty on WireGenerateContent
	Usage  Usage  // the usage the wire reported, normalized per wire
	Body   []byte // the bytes written
}

// Exchange is one request the server received and the answer it gave.
type Exchange struct {
	Seq         int  // 1-based arrival order
	Wire        Wire // empty for ExchangeUnrouted
	Kind        ExchangeKind
	Method      string
	Path        string // path and raw query as received
	Header      http.Header
	Body        []byte // the request body as received
	Model       string
	Streaming   bool
	Tools       []Tool
	ToolResults []ToolResult
	Credentials []string // non-empty credential header values, a Bearer scheme stripped
	Step        int      // 0-based script index for turn and unmatched exchanges; -1 otherwise
	Answer      Answer
}

func (e Exchange) clone() Exchange {
	e.Header = e.Header.Clone()
	e.Body = slices.Clone(e.Body)
	e.Tools = slices.Clone(e.Tools)
	for i := range e.Tools {
		e.Tools[i].Parameters = slices.Clone(e.Tools[i].Parameters)
	}
	e.ToolResults = slices.Clone(e.ToolResults)
	e.Credentials = slices.Clone(e.Credentials)
	e.Answer.Call = e.Answer.Call.clone()
	e.Answer.Body = slices.Clone(e.Answer.Body)
	return e
}

func (c *FunctionCall) clone() *FunctionCall {
	if c == nil {
		return nil
	}
	dup := *c
	dup.Arguments = slices.Clone(c.Arguments)
	return &dup
}

const maxBodyBytes = 32 << 20

// auxiliaryUsage is the usage every auxiliary answer reports. Its basis
// (Prompt plus Candidates) must stay below the basis of every turn answer the
// driver scripts, because the one registered usage source stops reading once
// its figure covers the turn's own bound and so cannot stop on a read that
// lacks a turn record.
var auxiliaryUsage = Usage{Prompt: 7, Candidates: 3, Thoughts: 2}

type route struct {
	model     string
	streaming bool
}

// responseSchema is the top-level object schema a request asks its answer to
// conform to: each property name with its lowercased type.
type responseSchema struct {
	properties map[string]string
}

type decoded struct {
	model       string
	streaming   bool
	tools       []Tool
	toolResults []ToolResult
	schema      *responseSchema
	unsupported string // "non-streaming answer" or "response schema"; empty when the wire answers the shape
}

type reply struct {
	seq       int
	model     string
	streaming bool
	text      string
	call      *FunctionCall
	usage     Usage
}

type rendered struct {
	contentType string
	body        []byte
	callID      string
	usage       Usage // the usage as the wire reports it, normalized back to Usage
}

// codec is one model wire: how its requests are routed and read, and how its
// answers and errors are written.
type codec interface {
	wire() Wire
	route(method, path string) (route, bool)
	decode(rt route, body []byte) (decoded, error)
	render(r reply) rendered
	errorBody(status int, message string) []byte
	credentialHeaders() []string
}

// wireCodecs lists the codecs the server routes to, in routing order.
var wireCodecs = []codec{generateContentCodec{}, messagesCodec{}, responsesCodec{}}

// unroutedCredentialHeaders are the credential headers read from a request no
// wire routes: the union of every wire's own.
var unroutedCredentialHeaders = []string{"x-goog-api-key", "x-api-key", "Authorization"}

// Server is a running scripted model endpoint.
type Server struct {
	t      testing.TB
	script []Response
	http   *httptest.Server

	holdingOnce sync.Once
	holdingCh   chan struct{} // closed when the first held request is held
	releaseOnce sync.Once
	released    chan struct{}

	mu         sync.Mutex
	served     []bool        // guarded by mu
	log        []Exchange    // guarded by mu
	open       map[int]bool  // guarded by mu; Seq of every held request still open
	holdsMoved chan struct{} // guarded by mu; closed and replaced whenever open changes
}

// Start serves script on 127.0.0.1 with an OS-assigned port and closes the
// server through t.Cleanup.
//
// Start fails t with Fatalf, naming the first invalid index, unless each
// entry has exactly one of a non-empty Text, a non-nil Call and a true Hold,
// no negative count, and CachedContent plus CacheWrite no greater than Prompt.
// A held entry reports no usage. At cleanup it ends every held request and
// reports every entry no request was served from. It must be called on the
// test goroutine.
func Start(t testing.TB, script []Response) *Server {
	t.Helper()

	for i, entry := range script {
		if rule := invalidScriptRule(entry); rule != "" {
			t.Fatalf("scripted model: script response %d %s", i, rule)
		}
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("scripted model: listen on 127.0.0.1: %v", err)
	}

	s := &Server{
		t:      t,
		script: slices.Clone(script),
		served: make([]bool, len(script)),

		holdingCh:  make(chan struct{}),
		released:   make(chan struct{}),
		open:       make(map[int]bool),
		holdsMoved: make(chan struct{}),
	}
	s.http = &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 30 * time.Second},
	}
	s.http.Start()
	t.Cleanup(s.close)
	return s
}

func invalidScriptRule(r Response) string {
	u := r.Usage
	switch {
	case !exactlyOne(r.Text != "", r.Call != nil, r.Hold):
		return "must hold exactly one of Text, Call and Hold"
	case r.Hold && u != (Usage{}):
		return "holds a request but reports usage"
	case min(u.Prompt, u.Candidates, u.Thoughts, u.CachedContent, u.CacheWrite) < 0:
		return "reports a negative usage count"
	case u.CachedContent+u.CacheWrite > u.Prompt:
		return "reports CachedContent plus CacheWrite above Prompt"
	}
	return ""
}

func exactlyOne(flags ...bool) bool {
	set := 0
	for _, flag := range flags {
		if flag {
			set++
		}
	}
	return set == 1
}

// URL returns the server's base address, http://127.0.0.1:<port>, with no
// path and no trailing slash.
func (s *Server) URL() string {
	return s.http.URL
}

// Exchanges returns a copy of the request log in arrival order. It is safe
// from any goroutine, and a later request never changes a returned slice.
func (s *Server) Exchanges() []Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Exchange, len(s.log))
	for i, ex := range s.log {
		out[i] = ex.clone()
	}
	return out
}

// holding returns a channel closed once the first request a held answer
// answers is read in full and held; Exchanges lists that request from then
// on. It is safe from any goroutine.
func (s *Server) holding() <-chan struct{} {
	return s.holdingCh
}

// release ends every hold, current and later, with the release answer. It is
// idempotent.
func (s *Server) release() {
	s.releaseOnce.Do(func() { close(s.released) })
}

// awaitHolds returns the held requests still open once all have ended or
// bound has elapsed.
func (s *Server) awaitHolds(bound time.Duration) []Exchange {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		s.mu.Lock()
		moved := s.holdsMoved
		pending := len(s.open)
		s.mu.Unlock()
		if pending == 0 {
			return nil
		}
		select {
		case <-moved:
		case <-timer.C:
			return s.openHolds()
		}
	}
}

func (s *Server) openHolds() []Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()

	var open []Exchange
	for _, seq := range slices.Sorted(maps.Keys(s.open)) {
		open = append(open, s.log[seq-1].clone())
	}
	return open
}

func (s *Server) setHold(seq int, open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if open {
		s.open[seq] = true
	} else {
		delete(s.open, seq)
	}
	close(s.holdsMoved)
	s.holdsMoved = make(chan struct{})
}

func (s *Server) close() {
	s.release()
	s.http.Close()

	s.mu.Lock()
	var unserved []int
	for i, served := range s.served {
		if !served {
			unserved = append(unserved, i)
		}
	}
	s.mu.Unlock()

	for _, i := range unserved {
		s.t.Errorf("scripted model: script response %d was never requested", i)
	}
}

func (s *Server) begin(r *http.Request) Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()

	ex := Exchange{
		Seq:    len(s.log) + 1,
		Method: r.Method,
		Path:   r.RequestURI,
		Header: r.Header.Clone(),
		Step:   -1,
	}
	s.log = append(s.log, ex.clone())
	return ex
}

func (s *Server) record(ex Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log[ex.Seq-1] = ex.clone()
}

func (s *Server) claimStep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, served := range s.served {
		if !served {
			s.served[i] = true
			return i
		}
	}
	return -1
}

// serve answers one request. It records the exchange before it writes the
// answer, and writes only after every lock is released.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	ex := s.begin(r)
	contentType, failure := s.respond(&ex, r)
	if ex.Kind == ExchangeHeld {
		s.hold(w, r, ex)
		return
	}
	s.record(ex)
	if failure != "" {
		s.t.Errorf("%s", failure)
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(ex.Answer.Status)
	_, _ = w.Write(ex.Answer.Body)
}

// respond fills ex with the answer to r and returns the content type to
// write it under and the failure to report, empty when the request is one the
// server answers as designed.
func (s *Server) respond(ex *Exchange, r *http.Request) (contentType, failure string) {
	ex.Credentials = credentialValues(r.Header, unroutedCredentialHeaders)
	label := failureLabel(*ex)

	if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return failGeneric(ex, http.StatusUnsupportedMediaType, ExchangeInvalid,
			label+" is encoded as "+encoding+", which this server does not decode")
	}

	c, rt, routed := findRoute(r.Method, r.URL.Path)
	if !routed {
		return failGeneric(ex, http.StatusNotFound, ExchangeUnrouted, label+" matches no route")
	}
	ex.Wire = c.wire()
	ex.Credentials = credentialValues(r.Header, c.credentialHeaders())

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	ex.Body = body
	if err != nil || len(body) > maxBodyBytes || !isJSONObject(body) {
		return failWire(ex, c, ExchangeInvalid, label+" carries no JSON object body", "")
	}
	dec, err := c.decode(rt, body)
	if err != nil {
		return failWire(ex, c, ExchangeInvalid, label+" carries a body this server cannot decode: "+err.Error(), "")
	}
	ex.Model, ex.Streaming, ex.Tools, ex.ToolResults = dec.model, dec.streaming, dec.tools, dec.toolResults

	if dec.unsupported != "" {
		return failWire(ex, c, ExchangeUnsupported,
			label+" asks for a "+dec.unsupported+" this server does not answer", "")
	}
	if len(dec.tools) == 0 {
		text := auxiliaryText(dec.schema)
		return answerWire(ex, c, ExchangeAuxiliary, reply{seq: ex.Seq, model: dec.model, streaming: dec.streaming, text: text, usage: auxiliaryUsage})
	}

	step := s.claimStep()
	if step < 0 {
		return failWire(ex, c, ExchangeExhausted,
			label+" arrived after the script was exhausted",
			fmt.Sprintf("; the script holds %d responses", len(s.script)))
	}
	ex.Step = step
	entry := s.script[step]
	if entry.Hold {
		ex.Kind = ExchangeHeld
		return "", ""
	}
	turn := reply{seq: ex.Seq, model: dec.model, streaming: dec.streaming, text: entry.Text, usage: entry.Usage}
	if entry.Call != nil {
		call, err := entry.Call(dec.tools)
		if err == nil && !isJSONObject(call.Arguments) {
			err = fmt.Errorf("the resolved call %q carries arguments that are not a JSON object", call.Name)
		}
		if err != nil {
			return failWire(ex, c, ExchangeUnmatched,
				label+" declares no tool matching the script: "+err.Error(),
				"; declared: "+strings.Join(toolNames(dec.tools), ", "))
		}
		turn.call = &call
	}
	return answerWire(ex, c, ExchangeTurn, turn)
}

const jsonContentType = "application/json"

func failureLabel(ex Exchange) string {
	return fmt.Sprintf("scripted model: request %d (%s %s)", ex.Seq, ex.Method, ex.Path)
}

// hold keeps a claimed held request open without writing, so the runtime
// sees a model that has not answered yet. The client's disconnect, noticed
// through the background read net/http starts once the body is read to EOF,
// ends the hold with nothing written.
func (s *Server) hold(w http.ResponseWriter, r *http.Request, ex Exchange) {
	s.setHold(ex.Seq, true)
	s.record(ex)
	s.holdingOnce.Do(func() { close(s.holdingCh) })

	select {
	case <-r.Context().Done():
	case <-s.released:
	}
	defer s.setHold(ex.Seq, false)
	// A release wake may race a disconnect that already ended the context, so
	// a context error decides, and a disconnect is never a failure.
	if r.Context().Err() != nil {
		return
	}

	c, _, _ := findRoute(r.Method, r.URL.Path)
	text := failureLabel(ex) + " was still held when the server released it"
	ex.Answer = Answer{Status: http.StatusBadRequest, Text: text, Body: c.errorBody(http.StatusBadRequest, text)}
	s.record(ex)
	s.t.Errorf("%s", text)

	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(ex.Answer.Status)
	_, _ = w.Write(ex.Answer.Body)
}

func failGeneric(ex *Exchange, status int, kind ExchangeKind, message string) (contentType, failure string) {
	envelope := struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	envelope.Error.Code, envelope.Error.Message = status, message
	return fail(ex, status, kind, message, encodeJSON(envelope), "")
}

func failWire(ex *Exchange, c codec, kind ExchangeKind, message, suffix string) (contentType, failure string) {
	return fail(ex, http.StatusBadRequest, kind, message, c.errorBody(http.StatusBadRequest, message), suffix)
}

func fail(ex *Exchange, status int, kind ExchangeKind, message string, body []byte, suffix string) (contentType, failure string) {
	ex.Kind = kind
	ex.Answer = Answer{Status: status, Text: message, Body: body}
	return jsonContentType, message + suffix
}

func answerWire(ex *Exchange, c codec, kind ExchangeKind, r reply) (contentType, failure string) {
	out := c.render(r)
	ex.Kind = kind
	ex.Answer = Answer{Status: http.StatusOK, Text: r.text, Call: r.call, CallID: out.callID, Usage: out.usage, Body: out.body}
	return out.contentType, ""
}

func findRoute(method, path string) (codec, route, bool) {
	for _, c := range wireCodecs {
		if rt, ok := c.route(method, path); ok {
			return c, rt, true
		}
	}
	return nil, route{}, false
}

func credentialValues(header http.Header, names []string) []string {
	var values []string
	for _, name := range names {
		for _, value := range header.Values(name) {
			value = strings.TrimSpace(value)
			if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
				value = strings.TrimSpace(value[len("Bearer "):])
			}
			if value != "" {
				values = append(values, value)
			}
		}
	}
	return values
}

func toolNames(tools []Tool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}

// auxiliaryText is the answer to a request that declares no tool: a JSON
// object holding every property of schema when the request declares one, and
// a fixed sentence otherwise.
func auxiliaryText(schema *responseSchema) string {
	if schema == nil {
		return "scripted auxiliary answer"
	}
	object := make(map[string]any, len(schema.properties))
	for name, typ := range schema.properties {
		if typ == "integer" {
			object[name] = 1
			continue
		}
		object[name] = "scripted"
	}
	return string(encodeJSON(object))
}

func isJSONObject(raw []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// encodeJSON marshals a value this package built from strings, integers and
// validated raw messages, which cannot fail to encode.
func encodeJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("fakemodel: encode %T: %v", v, err))
	}
	return out
}
