package fakemodel_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
)

const (
	testModel  = "scripted-model"
	sentinel   = "sentinel-token"
	readSchema = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
	maxBody    = 32 << 20
)

var (
	fullUsage      = fakemodel.Usage{Prompt: 500, Candidates: 40, Thoughts: 12, CachedContent: 100, CacheWrite: 30}
	auxiliaryUsage = fakemodel.Usage{Prompt: 7, Candidates: 3, Thoughts: 2}
)

type httpReply struct {
	status int
	body   []byte
}

func trySend(t testing.TB, base, method, target string, headers map[string]string, body []byte) (httpReply, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, base+target, bytes.NewReader(body))
	if err != nil {
		return httpReply{}, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return httpReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	return httpReply{status: resp.StatusCode, body: data}, err
}

func send(t testing.TB, base, method, target string, headers map[string]string, body []byte) httpReply {
	t.Helper()

	reply, err := trySend(t, base, method, target, headers, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return reply
}

func exchangeAt(t testing.TB, srv *fakemodel.Server, index int) fakemodel.Exchange {
	t.Helper()

	exchanges := srv.Exchanges()
	if index >= len(exchanges) {
		t.Fatalf("Exchanges() holds %d entries, want an entry at index %d", len(exchanges), index)
	}
	return exchanges[index]
}

func loadFixture(t testing.TB, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func decodeObject(t testing.TB, raw []byte) map[string]any {
	t.Helper()

	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return object
}

func unmarshal(t testing.TB, raw []byte, into any) {
	t.Helper()

	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decoding %q into %T: %v", raw, into, err)
	}
}

type failureLedger struct {
	testing.TB

	mu       sync.Mutex
	errs     []string
	fatals   []string
	cleanups []func()
	once     sync.Once
}

func (l *failureLedger) Helper() {}

func (l *failureLedger) Errorf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, fmt.Sprintf(format, args...))
}

func (l *failureLedger) Fatalf(format string, args ...any) {
	l.mu.Lock()
	l.fatals = append(l.fatals, fmt.Sprintf(format, args...))
	l.mu.Unlock()
	runtime.Goexit()
}

func (l *failureLedger) Cleanup(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanups = append(l.cleanups, f)
}

func (l *failureLedger) finish() {
	l.once.Do(func() {
		l.mu.Lock()
		cleanups := slices.Clone(l.cleanups)
		l.mu.Unlock()
		for _, f := range slices.Backward(cleanups) {
			f()
		}
	})
}

func (l *failureLedger) failures() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.errs)
}

func (l *failureLedger) fatalities() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.fatals)
}

func startLedgered(t *testing.T, script []fakemodel.Response) (*fakemodel.Server, *failureLedger) {
	t.Helper()

	ledger := &failureLedger{TB: t}
	srv := fakemodel.Start(ledger, script)
	t.Cleanup(ledger.finish)
	return srv, ledger
}

func startInvalid(script []fakemodel.Response) *failureLedger {
	ledger := &failureLedger{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fakemodel.Start(ledger, script)
	}()
	<-done
	ledger.finish()
	return ledger
}

type testWire struct {
	wire          fakemodel.Wire
	path          string
	turnBody      string
	auxBody       string
	hostedBody    string
	undecodable   string
	envelope      func(status int, message string) map[string]any
	normalize     func(fakemodel.Usage) fakemodel.Usage
	credentialsOf func(g, k, bearer string) []string
}

func genericEnvelope(status int, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": float64(status), "message": message}}
}

var testWires = []testWire{
	{
		wire:        fakemodel.WireGenerateContent,
		path:        "/v1beta/models/" + testModel + ":streamGenerateContent?alt=sse",
		turnBody:    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"read_file","parametersJsonSchema":` + readSchema + `}]}]}`,
		auxBody:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		hostedBody:  `{"contents":[],"tools":[{"googleSearch":{}}]}`,
		undecodable: `{"tools":"x"}`,
		envelope: func(status int, message string) map[string]any {
			return map[string]any{"error": map[string]any{"code": float64(status), "message": message, "status": "INVALID_ARGUMENT"}}
		},
		normalize: func(u fakemodel.Usage) fakemodel.Usage {
			u.CacheWrite = 0
			return u
		},
		credentialsOf: func(g, _, bearer string) []string { return []string{g, bearer} },
	},
	{
		wire:        fakemodel.WireMessages,
		path:        "/v1/messages",
		turnBody:    `{"model":"` + testModel + `","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","input_schema":` + readSchema + `}]}`,
		auxBody:     `{"model":"` + testModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		hostedBody:  `{"model":"` + testModel + `","stream":true,"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
		undecodable: `{"model":5}`,
		envelope: func(_ int, message string) map[string]any {
			return map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}}
		},
		normalize: func(u fakemodel.Usage) fakemodel.Usage {
			u.Candidates, u.Thoughts = u.Candidates+u.Thoughts, 0
			return u
		},
		credentialsOf: func(_, k, bearer string) []string { return []string{k, bearer} },
	},
	{
		wire:        fakemodel.WireResponses,
		path:        "/v1/responses",
		turnBody:    `{"model":"` + testModel + `","stream":true,"input":[],"tools":[{"type":"function","name":"read_file","parameters":` + readSchema + `}]}`,
		auxBody:     `{"model":"` + testModel + `","stream":true,"input":[]}`,
		hostedBody:  `{"model":"` + testModel + `","stream":true,"input":[],"tools":[{"type":"web_search"}]}`,
		undecodable: `{"tools":{}}`,
		envelope: func(_ int, message string) map[string]any {
			return map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error", "param": nil, "code": nil}}
		},
		normalize: func(u fakemodel.Usage) fakemodel.Usage {
			u.CacheWrite = 0
			return u
		},
		credentialsOf: func(_, _, bearer string) []string { return []string{bearer} },
	},
}

func (w testWire) name() string { return string(w.wire) }

func (w testWire) post(t testing.TB, srv *fakemodel.Server, body string) httpReply {
	t.Helper()
	return send(t, srv.URL(), http.MethodPost, w.path, nil, []byte(body))
}

func requestLabel(seq int, method, target string) string {
	return fmt.Sprintf("scripted model: request %d (%s %s)", seq, method, target)
}

type failureWant struct {
	seq           int
	status        int
	kind          fakemodel.ExchangeKind
	wire          fakemodel.Wire
	message       string
	messagePrefix string
	suffix        string
	envelope      func(status int, message string) map[string]any
}

func assertFailureRow(t *testing.T, srv *fakemodel.Server, ledger *failureLedger, got httpReply, want failureWant) {
	t.Helper()

	ex := exchangeAt(t, srv, want.seq-1)

	if got.status != want.status {
		t.Errorf("response status = %d, want %d", got.status, want.status)
	}
	if ex.Answer.Status != want.status {
		t.Errorf("Exchange.Answer.Status = %d, want %d", ex.Answer.Status, want.status)
	}
	if ex.Kind != want.kind {
		t.Errorf("Exchange.Kind = %q, want %q", ex.Kind, want.kind)
	}
	if ex.Wire != want.wire {
		t.Errorf("Exchange.Wire = %q, want %q", ex.Wire, want.wire)
	}
	if ex.Step != -1 && want.kind != fakemodel.ExchangeUnmatched {
		t.Errorf("Exchange.Step = %d, want -1", ex.Step)
	}
	switch {
	case want.message != "":
		if ex.Answer.Text != want.message {
			t.Errorf("Exchange.Answer.Text = %q, want %q", ex.Answer.Text, want.message)
		}
	case !strings.HasPrefix(ex.Answer.Text, want.messagePrefix):
		t.Errorf("Exchange.Answer.Text = %q, want prefix %q", ex.Answer.Text, want.messagePrefix)
	}
	if wantBody := want.envelope(want.status, ex.Answer.Text); !reflect.DeepEqual(decodeObject(t, got.body), wantBody) {
		t.Errorf("response body = %s, want the envelope %v", got.body, wantBody)
	}
	if !bytes.Equal(ex.Answer.Body, got.body) {
		t.Errorf("Exchange.Answer.Body = %q, want the response body %q", ex.Answer.Body, got.body)
	}
	if failures, wantFailures := ledger.failures(), []string{ex.Answer.Text + want.suffix}; !slices.Equal(failures, wantFailures) {
		t.Errorf("reported failures = %q, want %q", failures, wantFailures)
	}
}

func TestServerURL(t *testing.T) {
	t.Parallel()

	srv := fakemodel.Start(t, nil)

	if got := srv.URL(); !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+$`).MatchString(got) {
		t.Errorf("URL() = %q, want http://127.0.0.1:<port> with no path", got)
	}
}

func TestServerRecordsExchange(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "recorded", Usage: fullUsage}})

			reply := send(t, srv.URL(), http.MethodPost, w.path, map[string]string{"X-Probe": "probe-value"}, []byte(w.turnBody))

			ex := exchangeAt(t, srv, 0)
			if ex.Seq != 1 || ex.Step != 0 || ex.Wire != w.wire || ex.Kind != fakemodel.ExchangeTurn {
				t.Errorf("Exchange (Seq, Step, Wire, Kind) = (%d, %d, %q, %q), want (1, 0, %q, %q)", ex.Seq, ex.Step, ex.Wire, ex.Kind, w.wire, fakemodel.ExchangeTurn)
			}
			if ex.Method != http.MethodPost || ex.Path != w.path {
				t.Errorf("Exchange (Method, Path) = (%q, %q), want (%q, %q)", ex.Method, ex.Path, http.MethodPost, w.path)
			}
			if got := ex.Header.Get("X-Probe"); got != "probe-value" {
				t.Errorf("Exchange.Header[X-Probe] = %q, want %q", got, "probe-value")
			}
			if string(ex.Body) != w.turnBody {
				t.Errorf("Exchange.Body = %q, want the request body %q", ex.Body, w.turnBody)
			}
			if ex.Model != testModel || !ex.Streaming {
				t.Errorf("Exchange (Model, Streaming) = (%q, %t), want (%q, true)", ex.Model, ex.Streaming, testModel)
			}
			if len(ex.Tools) != 1 || ex.Tools[0].Name != "read_file" || string(ex.Tools[0].Parameters) != readSchema {
				t.Errorf("Exchange.Tools = %+v, want read_file declared with %s", ex.Tools, readSchema)
			}
			if ex.Answer.Status != http.StatusOK || ex.Answer.Text != "recorded" || ex.Answer.Call != nil {
				t.Errorf("Exchange.Answer = (%d, %q, %v), want (200, %q, nil)", ex.Answer.Status, ex.Answer.Text, ex.Answer.Call, "recorded")
			}
			if wantUsage := w.normalize(fullUsage); ex.Answer.Usage != wantUsage {
				t.Errorf("Exchange.Answer.Usage = %+v, want %+v", ex.Answer.Usage, wantUsage)
			}
			if reply.status != http.StatusOK || !bytes.Equal(reply.body, ex.Answer.Body) {
				t.Errorf("response = (%d, %q), want (200, Exchange.Answer.Body %q)", reply.status, reply.body, ex.Answer.Body)
			}
		})
	}
}

func TestServerExhaustedScript(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		for _, entries := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d entries", w.name(), entries), func(t *testing.T) {
				t.Parallel()

				script := make([]fakemodel.Response, entries)
				for i := range script {
					script[i] = fakemodel.Response{Text: "answer"}
				}
				srv, ledger := startLedgered(t, script)
				for range entries {
					if reply := w.post(t, srv, w.turnBody); reply.status != http.StatusOK {
						t.Fatalf("turn within the script: status = %d, want 200", reply.status)
					}
				}

				reply := w.post(t, srv, w.turnBody)

				assertFailureRow(t, srv, ledger, reply, failureWant{
					seq:    entries + 1,
					status: http.StatusBadRequest, kind: fakemodel.ExchangeExhausted, wire: w.wire,
					message:  requestLabel(entries+1, http.MethodPost, w.path) + " arrived after the script was exhausted",
					suffix:   fmt.Sprintf("; the script holds %d responses", entries),
					envelope: w.envelope,
				})
			})
		}
	}
}

func TestServerUnmatchedToolChoice(t *testing.T) {
	t.Parallel()

	nonObject := func([]fakemodel.Tool) (fakemodel.FunctionCall, error) {
		return fakemodel.FunctionCall{Name: "read_file", Arguments: json.RawMessage(`"path"`)}, nil
	}
	choices := []struct {
		name   string
		choice fakemodel.ToolChoice
	}{
		{"no such tool", fakemodel.Named("missing", json.RawMessage(`{}`))},
		{"non-object arguments", nonObject},
	}

	for _, w := range testWires {
		for _, c := range choices {
			t.Run(w.name()+"/"+c.name, func(t *testing.T) {
				t.Parallel()

				srv, ledger := startLedgered(t, []fakemodel.Response{{Call: c.choice}})

				reply := w.post(t, srv, w.turnBody)

				assertFailureRow(t, srv, ledger, reply, failureWant{
					seq:    1,
					status: http.StatusBadRequest, kind: fakemodel.ExchangeUnmatched, wire: w.wire,
					messagePrefix: requestLabel(1, http.MethodPost, w.path) + " declares no tool matching the script: ",
					suffix:        "; declared: read_file",
					envelope:      w.envelope,
				})
				if step := exchangeAt(t, srv, 0).Step; step != 0 {
					t.Errorf("Exchange.Step = %d, want 0", step)
				}
			})
		}
	}
}

func TestServerUnmatchedToolChoiceNamesEveryDeclaredTool(t *testing.T) {
	t.Parallel()

	body := `{"model":"` + testModel + `","stream":true,"messages":[],"tools":[` +
		`{"name":"bash","input_schema":` + readSchema + `},{"name":"view","input_schema":` + readSchema + `}]}`
	srv, ledger := startLedgered(t, []fakemodel.Response{{Call: fakemodel.Named("missing", json.RawMessage(`{}`))}})

	reply := send(t, srv.URL(), http.MethodPost, "/v1/messages", nil, []byte(body))

	if reply.status != http.StatusBadRequest {
		t.Errorf("response status = %d, want 400", reply.status)
	}
	failures := ledger.failures()
	if len(failures) != 1 {
		t.Fatalf("reported failures = %q, want exactly one", failures)
	}
	_, declared, found := strings.Cut(failures[0], "; declared: ")
	bash, view := strings.Index(declared, "bash"), strings.Index(declared, "view")
	if !found || bash < 0 || view < bash {
		t.Errorf("reported failure = %q, want it to end with the declared names bash then view", failures[0])
	}
}

func TestServerUnrouted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		target string
	}{
		{"wrong method", http.MethodGet, "/v1/messages"},
		{"unknown path", http.MethodPost, "/v1/unknown"},
		{"unknown model method", http.MethodPost, "/v1beta/models/" + testModel + ":embedContent"},
		{"put on a routed path", http.MethodPut, "/v1/responses"},
		{"path below a routed one", http.MethodPost, "/v1/messages/count_tokens"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, ledger := startLedgered(t, nil)
			headers := map[string]string{"x-goog-api-key": "goog-key", "x-api-key": "api-key", "Authorization": "Bearer bearer-token"}

			reply := send(t, srv.URL(), tt.method, tt.target, headers, []byte(`{}`))

			assertFailureRow(t, srv, ledger, reply, failureWant{
				seq:    1,
				status: http.StatusNotFound, kind: fakemodel.ExchangeUnrouted, wire: "",
				message:  requestLabel(1, tt.method, tt.target) + " matches no route",
				envelope: genericEnvelope,
			})
			got := slices.Sorted(slices.Values(exchangeAt(t, srv, 0).Credentials))
			if want := []string{"api-key", "bearer-token", "goog-key"}; !slices.Equal(got, want) {
				t.Errorf("Exchange.Credentials = %q, want %q", got, want)
			}
		})
	}
}

func TestServerEncodedBody(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			srv, ledger := startLedgered(t, nil)

			reply := send(t, srv.URL(), http.MethodPost, w.path, map[string]string{"Content-Encoding": "gzip"}, []byte(w.turnBody))

			assertFailureRow(t, srv, ledger, reply, failureWant{
				seq:    1,
				status: http.StatusUnsupportedMediaType, kind: fakemodel.ExchangeInvalid, wire: "",
				message:  requestLabel(1, http.MethodPost, w.path) + " is encoded as gzip, which this server does not decode",
				envelope: genericEnvelope,
			})
		})
	}
}

func TestServerIdentityEncodingIsDecoded(t *testing.T) {
	t.Parallel()

	for _, encoding := range []string{"identity", "Identity"} {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()

			w := testWires[1]
			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

			reply := send(t, srv.URL(), http.MethodPost, w.path, map[string]string{"Content-Encoding": encoding}, []byte(w.turnBody))

			if reply.status != http.StatusOK {
				t.Errorf("status with Content-Encoding %q = %d, want 200", encoding, reply.status)
			}
		})
	}
}

func TestServerBodyNotJSONObject(t *testing.T) {
	t.Parallel()

	bodies := []struct {
		name string
		body string
	}{
		{"array", `[]`},
		{"null", `null`},
		{"string", `"text"`},
		{"malformed", `{"model":`},
		{"empty", ``},
	}

	for _, w := range testWires {
		for _, b := range bodies {
			t.Run(w.name()+"/"+b.name, func(t *testing.T) {
				t.Parallel()

				srv, ledger := startLedgered(t, nil)

				reply := w.post(t, srv, b.body)

				assertFailureRow(t, srv, ledger, reply, failureWant{
					seq:    1,
					status: http.StatusBadRequest, kind: fakemodel.ExchangeInvalid, wire: w.wire,
					message:  requestLabel(1, http.MethodPost, w.path) + " carries no JSON object body",
					envelope: w.envelope,
				})
			})
		}
	}
}

func TestServerBodyOverSizeLimit(t *testing.T) {
	t.Parallel()

	w := testWires[1]
	const prefix, suffix = `{"pad":"`, `"}`
	body := prefix + strings.Repeat("a", maxBody+1-len(prefix)-len(suffix)) + suffix
	srv, ledger := startLedgered(t, nil)

	reply := w.post(t, srv, body)

	assertFailureRow(t, srv, ledger, reply, failureWant{
		seq:    1,
		status: http.StatusBadRequest, kind: fakemodel.ExchangeInvalid, wire: w.wire,
		message:  requestLabel(1, http.MethodPost, w.path) + " carries no JSON object body",
		envelope: w.envelope,
	})
}

func TestServerUndecodableBody(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			srv, ledger := startLedgered(t, nil)

			reply := w.post(t, srv, w.undecodable)

			assertFailureRow(t, srv, ledger, reply, failureWant{
				seq:    1,
				status: http.StatusBadRequest, kind: fakemodel.ExchangeInvalid, wire: w.wire,
				messagePrefix: requestLabel(1, http.MethodPost, w.path) + " carries a body this server cannot decode: ",
				envelope:      w.envelope,
			})
		})
	}
}

func TestServerReportsEveryNeverRequestedEntry(t *testing.T) {
	t.Parallel()

	w := testWires[2]
	script := []fakemodel.Response{{Text: "zero"}, {Text: "one"}, {Text: "two"}}
	srv, ledger := startLedgered(t, script)
	w.post(t, srv, w.turnBody)

	ledger.finish()

	want := []string{
		"scripted model: script response 1 was never requested",
		"scripted model: script response 2 was never requested",
	}
	if got := ledger.failures(); !slices.Equal(got, want) {
		t.Errorf("failures after cleanup = %q, want %q", got, want)
	}
}

func TestServerReportsNothingWhenEveryEntryIsServed(t *testing.T) {
	t.Parallel()

	w := testWires[2]
	srv, ledger := startLedgered(t, []fakemodel.Response{{Text: "zero"}, {Text: "one"}})
	w.post(t, srv, w.turnBody)
	w.post(t, srv, w.turnBody)

	ledger.finish()

	if got := ledger.failures(); len(got) != 0 {
		t.Errorf("failures after cleanup = %q, want none", got)
	}
}

func TestStartInvalidScript(t *testing.T) {
	t.Parallel()

	valid := fakemodel.Response{Text: "ok", Usage: fakemodel.Usage{Prompt: 10}}
	choice := fakemodel.Named("read_file", json.RawMessage(`{}`))

	tests := []struct {
		name      string
		script    []fakemodel.Response
		wantIndex int
	}{
		{"text and call", []fakemodel.Response{{Text: "ok", Call: choice}}, 0},
		{"neither text nor call", []fakemodel.Response{{}}, 0},
		{"hold and text", []fakemodel.Response{{Hold: true, Text: "ok"}}, 0},
		{"hold and call", []fakemodel.Response{{Hold: true, Call: choice}}, 0},
		{"hold with usage", []fakemodel.Response{{Hold: true, Usage: fakemodel.Usage{Prompt: 1}}}, 0},
		{"negative Prompt", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: -1}}}, 0},
		{"negative Candidates", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: 10, Candidates: -1}}}, 0},
		{"negative Thoughts", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: 10, Thoughts: -1}}}, 0},
		{"negative CachedContent", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: 10, CachedContent: -1}}}, 0},
		{"negative CacheWrite", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: 10, CacheWrite: -1}}}, 0},
		{"cache above Prompt", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: 10, CachedContent: 6, CacheWrite: 5}}}, 0},
		{"first invalid index is named", []fakemodel.Response{valid, {}, {Text: "ok", Call: choice}}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ledger := startInvalid(tt.script)

			fatals := ledger.fatalities()
			if len(fatals) != 1 {
				t.Fatalf("Start(%s) fatal reports = %q, want exactly one", tt.name, fatals)
			}
			if want := fmt.Sprintf("scripted model: script response %d ", tt.wantIndex); !strings.HasPrefix(fatals[0], want) {
				t.Errorf("Start(%s) fatal report = %q, want prefix %q", tt.name, fatals[0], want)
			}
			if failures := ledger.failures(); len(failures) != 0 {
				t.Errorf("Start(%s) reported failures = %q, want none after the fatal one", tt.name, failures)
			}
		})
	}
}

func TestStartAcceptsBoundaryScripts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script []fakemodel.Response
	}{
		{"empty script", nil},
		{"cache equal to Prompt", []fakemodel.Response{{Text: "ok", Usage: fakemodel.Usage{Prompt: 10, CachedContent: 6, CacheWrite: 4}}}},
		{"zero usage", []fakemodel.Response{{Text: "ok"}}},
		{"call entry", []fakemodel.Response{{Call: fakemodel.ReadFile("/workspace/nonce.txt")}}},
		{"held answer", []fakemodel.Response{{Hold: true}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ledger := startInvalid(tt.script)

			if fatals := ledger.fatalities(); len(fatals) != 0 {
				t.Errorf("Start(%s) fatal reports = %q, want none", tt.name, fatals)
			}
		})
	}
}

func TestServerAuxiliaryAnswersOutsideTheScript(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "turn answer"}})

			aux := w.post(t, srv, w.auxBody)
			turn := w.post(t, srv, w.turnBody)

			exchanges := srv.Exchanges()
			if len(exchanges) != 2 {
				t.Fatalf("Exchanges() holds %d entries, want 2", len(exchanges))
			}
			first, second := exchanges[0], exchanges[1]
			if first.Kind != fakemodel.ExchangeAuxiliary || first.Step != -1 || first.Answer.Status != http.StatusOK {
				t.Errorf("auxiliary Exchange (Kind, Step, Status) = (%q, %d, %d), want (%q, -1, 200)", first.Kind, first.Step, first.Answer.Status, fakemodel.ExchangeAuxiliary)
			}
			if first.Answer.Text != "scripted auxiliary answer" {
				t.Errorf("auxiliary Answer.Text = %q, want %q", first.Answer.Text, "scripted auxiliary answer")
			}
			if wantUsage := w.normalize(auxiliaryUsage); first.Answer.Usage != wantUsage {
				t.Errorf("auxiliary Answer.Usage = %+v, want %+v", first.Answer.Usage, wantUsage)
			}
			if len(first.Tools) != 0 {
				t.Errorf("auxiliary Exchange.Tools = %+v, want none", first.Tools)
			}
			if aux.status != http.StatusOK || !bytes.Contains(aux.body, []byte("scripted auxiliary answer")) {
				t.Errorf("auxiliary response = (%d, %q), want 200 carrying the auxiliary text", aux.status, aux.body)
			}
			if second.Kind != fakemodel.ExchangeTurn || second.Step != 0 || turn.status != http.StatusOK {
				t.Errorf("turn after the auxiliary Exchange (Kind, Step, status) = (%q, %d, %d), want (%q, 0, 200)", second.Kind, second.Step, turn.status, fakemodel.ExchangeTurn)
			}
		})
	}
}

func TestServerHostedToolsAreNotDeclaredTools(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			srv := fakemodel.Start(t, nil)

			reply := w.post(t, srv, w.hostedBody)

			ex := exchangeAt(t, srv, 0)
			if reply.status != http.StatusOK || ex.Kind != fakemodel.ExchangeAuxiliary || len(ex.Tools) != 0 {
				t.Errorf("request with hosted tools only = (status %d, Kind %q, Tools %+v), want (200, %q, none)", reply.status, ex.Kind, ex.Tools, fakemodel.ExchangeAuxiliary)
			}
		})
	}
}

func TestServerAuxiliaryWithResponseSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
		want map[string]any
	}{
		{"upper case types", loadFixture(t, "requests/generateContent/gemini-auxiliary.json"), map[string]any{"complexity_reasoning": "scripted", "complexity_score": float64(1)}},
		{
			"lower case types",
			[]byte(`{"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"label":{"type":"string"},"count":{"type":"integer"}}}}}`),
			map[string]any{"label": "scripted", "count": float64(1)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := fakemodel.Start(t, nil)

			reply := send(t, srv.URL(), http.MethodPost, "/v1beta/models/"+testModel+":generateContent", nil, tt.body)

			ex := exchangeAt(t, srv, 0)
			if reply.status != http.StatusOK || ex.Kind != fakemodel.ExchangeAuxiliary || ex.Step != -1 {
				t.Fatalf("response = (status %d, Kind %q, Step %d), want (200, %q, -1)", reply.status, ex.Kind, ex.Step, fakemodel.ExchangeAuxiliary)
			}
			if got := decodeObject(t, []byte(ex.Answer.Text)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("auxiliary Answer.Text = %v, want the object %v", got, tt.want)
			}
			var chunk generateContentChunk
			unmarshal(t, reply.body, &chunk)
			if len(chunk.Candidates) != 1 || len(chunk.Candidates[0].Content.Parts) != 1 || chunk.Candidates[0].Content.Parts[0].Text != ex.Answer.Text {
				t.Errorf("response chunk = %s, want one part carrying %q", reply.body, ex.Answer.Text)
			}
			if got := chunk.UsageMetadata; got.PromptTokenCount != 7 || got.CandidatesTokenCount != 3 || got.ThoughtsTokenCount != 2 || got.TotalTokenCount != 12 {
				t.Errorf("response usageMetadata = %+v, want prompt 7, candidates 3, thoughts 2, total 12", got)
			}
		})
	}
}

func TestExchangesReturnsCopy(t *testing.T) {
	t.Parallel()

	const body = `{"model":"` + testModel + `","stream":true,` +
		`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"out"}]}],` +
		`"tools":[{"name":"read_file","input_schema":` + readSchema + `}]}`

	newServer := func(t *testing.T) *fakemodel.Server {
		t.Helper()
		srv := fakemodel.Start(t, []fakemodel.Response{{Call: fakemodel.ReadFile("/workspace/nonce.txt")}})
		send(t, srv.URL(), http.MethodPost, "/v1/messages", map[string]string{"x-api-key": sentinel}, []byte(body))
		return srv
	}

	t.Run("later request does not change a returned slice", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t)
		snapshot := srv.Exchanges()
		want := srv.Exchanges()

		send(t, srv.URL(), http.MethodPost, "/v1/messages", nil, []byte(testWires[1].auxBody))

		if len(snapshot) != 1 || !reflect.DeepEqual(snapshot, want) {
			t.Errorf("returned slice after a later request = %d entries, want the same single entry as before", len(snapshot))
		}
		if got := len(srv.Exchanges()); got != 2 {
			t.Errorf("Exchanges() holds %d entries after a second request, want 2", got)
		}
	})

	t.Run("mutating a returned entry does not change the log", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t)
		want := srv.Exchanges()
		got := srv.Exchanges()
		ex := &got[0]
		if len(ex.Header) == 0 || len(ex.Body) == 0 || len(ex.Tools) == 0 || len(ex.ToolResults) == 0 || len(ex.Credentials) == 0 || len(ex.Answer.Body) == 0 {
			t.Fatalf("Exchange = %+v, want every collection populated for the mutation", *ex)
		}

		ex.Header.Set("x-api-key", "changed")
		ex.Body[0] = 'X'
		ex.Tools[0].Name = "changed"
		ex.ToolResults[0].Output = "changed"
		ex.Credentials[0] = "changed"
		ex.Answer.Body[0] = 'X'
		ex.Answer.Text = "changed"

		if again := srv.Exchanges(); !reflect.DeepEqual(again, want) {
			t.Errorf("Exchanges() after mutating a returned entry = %+v, want %+v", again, want)
		}
	})

	t.Run("mutating nested byte slices does not change the log", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t)
		got := srv.Exchanges()
		ex := &got[0]
		if len(ex.Tools[0].Parameters) == 0 || ex.Answer.Call == nil || len(ex.Answer.Call.Arguments) == 0 {
			t.Fatalf("Exchange = %+v, want Tools[0].Parameters and Answer.Call.Arguments populated", *ex)
		}
		wantParameters, wantArguments := string(ex.Tools[0].Parameters), string(ex.Answer.Call.Arguments)

		ex.Tools[0].Parameters[0] = 'X'
		ex.Answer.Call.Arguments[0] = 'X'

		again := srv.Exchanges()[0]
		if string(again.Tools[0].Parameters) != wantParameters {
			t.Errorf("Exchanges()[0].Tools[0].Parameters after a mutation = %q, want %q", again.Tools[0].Parameters, wantParameters)
		}
		if string(again.Answer.Call.Arguments) != wantArguments {
			t.Errorf("Exchanges()[0].Answer.Call.Arguments after a mutation = %q, want %q", again.Answer.Call.Arguments, wantArguments)
		}
	})
}

func TestServerConcurrentTurnsClaimDistinctSteps(t *testing.T) {
	t.Parallel()

	const turns = 12

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			script := make([]fakemodel.Response, turns)
			for i := range script {
				script[i] = fakemodel.Response{Text: fmt.Sprintf("answer %d", i)}
			}
			srv := fakemodel.Start(t, script)

			var wg sync.WaitGroup
			errs := make(chan error, turns)
			for range turns {
				wg.Go(func() {
					reply, err := trySend(t, srv.URL(), http.MethodPost, w.path, nil, []byte(w.turnBody))
					if err == nil && reply.status != http.StatusOK {
						err = fmt.Errorf("status = %d, want 200", reply.status)
					}
					errs <- err
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent turn request: %v", err)
				}
			}

			exchanges := srv.Exchanges()
			if len(exchanges) != turns {
				t.Fatalf("Exchanges() holds %d entries, want %d", len(exchanges), turns)
			}
			steps := make(map[int]bool)
			for i, ex := range exchanges {
				if ex.Seq != i+1 {
					t.Errorf("Exchanges()[%d].Seq = %d, want %d", i, ex.Seq, i+1)
				}
				if ex.Step < 0 || ex.Step >= turns || steps[ex.Step] {
					t.Errorf("Exchanges()[%d].Step = %d, want an index in [0, %d) no other request claimed", i, ex.Step, turns)
					continue
				}
				steps[ex.Step] = true
				if want := script[ex.Step].Text; ex.Answer.Text != want {
					t.Errorf("Exchanges()[%d].Answer.Text = %q, want %q for step %d", i, ex.Answer.Text, want, ex.Step)
				}
			}
		})
	}
}

func TestServerIdenticalBytesAcrossRuns(t *testing.T) {
	t.Parallel()

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			run := func() [][]byte {
				srv := fakemodel.Start(t, []fakemodel.Response{
					{Text: "first answer", Usage: fullUsage},
					{Call: fakemodel.ReadFile("/workspace/nonce.txt"), Usage: fullUsage},
				})
				var bodies [][]byte
				for _, body := range []string{w.turnBody, w.turnBody, w.auxBody} {
					bodies = append(bodies, w.post(t, srv, body).body)
				}
				for i, ex := range srv.Exchanges() {
					if !bytes.Equal(ex.Answer.Body, bodies[i]) {
						t.Errorf("Exchanges()[%d].Answer.Body = %q, want the response body %q", i, ex.Answer.Body, bodies[i])
					}
				}
				return bodies
			}

			first, second := run(), run()

			for i := range first {
				if len(first[i]) == 0 || !bytes.Equal(first[i], second[i]) {
					t.Errorf("response %d differs between runs: %q vs %q", i, first[i], second[i])
				}
			}
		})
	}
}

func TestServerCredentialHeaders(t *testing.T) {
	t.Parallel()

	headers := map[string]string{"x-goog-api-key": "goog-key", "x-api-key": "api-key", "Authorization": "Bearer bearer-token"}

	for _, w := range testWires {
		t.Run(w.name(), func(t *testing.T) {
			t.Parallel()

			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

			send(t, srv.URL(), http.MethodPost, w.path, headers, []byte(w.turnBody))

			got := slices.Sorted(slices.Values(exchangeAt(t, srv, 0).Credentials))
			want := slices.Sorted(slices.Values(w.credentialsOf("goog-key", "api-key", "bearer-token")))
			if !slices.Equal(got, want) {
				t.Errorf("Exchange.Credentials = %q, want %q", got, want)
			}
		})
	}
}

func TestServerCredentialValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		authorization string
		want          []string
	}{
		{"bearer scheme stripped", "Bearer token-a", []string{"token-a"}},
		{"lower case scheme stripped", "bearer token-a", []string{"token-a"}},
		{"no scheme kept", "token-a", []string{"token-a"}},
		{"empty value omitted", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := testWires[2]
			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

			send(t, srv.URL(), http.MethodPost, w.path, map[string]string{"Authorization": tt.authorization}, []byte(w.turnBody))

			if got := exchangeAt(t, srv, 0).Credentials; !slices.Equal(got, tt.want) {
				t.Errorf("Exchange.Credentials for Authorization %q = %q, want %q", tt.authorization, got, tt.want)
			}
		})
	}
}

type toolResultWant struct {
	callID string
	name   string
	nonce  string
	output string
}

type fixtureCase struct {
	file        string
	path        string
	headers     map[string]string
	choice      fakemodel.ToolChoice
	auxiliary   bool
	wantModel   string
	wantTools   int
	wantFirst   string
	wantResult  *toolResultWant
	wantCreds   []string
	wantCall    string
	wantCallArg string
}

func fixtureModel(t testing.TB, body []byte) string {
	t.Helper()

	var request struct {
		Model string `json:"model"`
	}
	unmarshal(t, body, &request)
	if request.Model == "" {
		t.Fatalf("fixture carries no model member")
	}
	return request.Model
}

func replayFixture(t *testing.T, wire fakemodel.Wire, c fixtureCase) {
	t.Helper()

	body := loadFixture(t, c.file)
	var script []fakemodel.Response
	if !c.auxiliary {
		script = []fakemodel.Response{{Call: c.choice}}
	}
	srv := fakemodel.Start(t, script)

	reply := send(t, srv.URL(), http.MethodPost, c.path, c.headers, body)

	ex := exchangeAt(t, srv, 0)
	wantModel := c.wantModel
	if wantModel == "" {
		wantModel = fixtureModel(t, body)
	}
	wantKind, wantStep := fakemodel.ExchangeTurn, 0
	if c.auxiliary {
		wantKind, wantStep = fakemodel.ExchangeAuxiliary, -1
	}
	if reply.status != http.StatusOK || ex.Wire != wire || ex.Kind != wantKind || ex.Step != wantStep {
		t.Errorf("Exchange for %s (status, Wire, Kind, Step) = (%d, %q, %q, %d), want (200, %q, %q, %d)", c.file, reply.status, ex.Wire, ex.Kind, ex.Step, wire, wantKind, wantStep)
	}
	if ex.Path != c.path || ex.Model != wantModel {
		t.Errorf("Exchange for %s (Path, Model) = (%q, %q), want (%q, %q)", c.file, ex.Path, ex.Model, c.path, wantModel)
	}
	if wantStreaming := !c.auxiliary; ex.Streaming != wantStreaming {
		t.Errorf("Exchange.Streaming for %s = %t, want %t", c.file, ex.Streaming, wantStreaming)
	}
	if len(ex.Tools) != c.wantTools {
		t.Fatalf("Exchange.Tools for %s holds %d tools, want %d", c.file, len(ex.Tools), c.wantTools)
	}
	if c.wantTools > 0 && ex.Tools[0].Name != c.wantFirst {
		t.Errorf("Exchange.Tools[0].Name for %s = %q, want %q", c.file, ex.Tools[0].Name, c.wantFirst)
	}
	if !slices.Equal(ex.Credentials, c.wantCreds) {
		t.Errorf("Exchange.Credentials for %s = %q, want %q", c.file, ex.Credentials, c.wantCreds)
	}
	assertFixtureToolResults(t, c, ex.ToolResults)
	if !c.auxiliary {
		assertFixtureCall(t, c, ex.Answer.Call)
	}
}

func assertFixtureToolResults(t *testing.T, c fixtureCase, got []fakemodel.ToolResult) {
	t.Helper()

	if c.wantResult == nil {
		if len(got) != 0 {
			t.Errorf("Exchange.ToolResults for %s = %+v, want none", c.file, got)
		}
		return
	}
	if len(got) != 1 {
		t.Fatalf("Exchange.ToolResults for %s holds %d results, want 1", c.file, len(got))
	}
	want := c.wantResult
	if got[0].CallID != want.callID || got[0].Name != want.name {
		t.Errorf("Exchange.ToolResults[0] for %s (CallID, Name) = (%q, %q), want (%q, %q)", c.file, got[0].CallID, got[0].Name, want.callID, want.name)
	}
	if want.nonce != "" && !strings.Contains(got[0].Output, want.nonce) {
		t.Errorf("Exchange.ToolResults[0].Output for %s = %q, want it to contain %q", c.file, got[0].Output, want.nonce)
	}
	if want.output != "" && got[0].Output != want.output {
		t.Errorf("Exchange.ToolResults[0].Output for %s = %q, want %q", c.file, got[0].Output, want.output)
	}
}

func assertFixtureCall(t *testing.T, c fixtureCase, call *fakemodel.FunctionCall) {
	t.Helper()

	if call == nil {
		t.Fatalf("Exchange.Answer.Call for %s = nil, want a call to %q", c.file, c.wantCall)
	}
	if call.Name != c.wantCall {
		t.Errorf("Exchange.Answer.Call.Name for %s = %q, want %q", c.file, call.Name, c.wantCall)
	}
	var arguments map[string]string
	unmarshal(t, call.Arguments, &arguments)
	if _, ok := arguments[c.wantCallArg]; !ok || len(arguments) != 1 {
		t.Errorf("Exchange.Answer.Call.Arguments for %s = %s, want the single property %q", c.file, call.Arguments, c.wantCallArg)
	}
}

func hasMember(node any, path []string) bool {
	if len(path) == 0 {
		return true
	}
	name, isList := strings.CutSuffix(path[0], "[]")
	object, ok := node.(map[string]any)
	if !ok {
		return false
	}
	child, ok := object[name]
	if !ok {
		return false
	}
	if !isList {
		return hasMember(child, path[1:])
	}
	items, ok := child.([]any)
	if !ok {
		return false
	}
	return slices.ContainsFunc(items, func(item any) bool { return hasMember(item, path[1:]) })
}

func assertMembersDropped(t *testing.T, files []string, dropped []string) {
	t.Helper()

	for _, file := range files {
		var root any
		unmarshal(t, loadFixture(t, file), &root)
		for _, member := range dropped {
			if hasMember(root, strings.Split(member, ".")) {
				t.Errorf("fixture %s carries %q, want it dropped", file, member)
			}
		}
	}
}

const (
	generateContentDir = "requests/generateContent/"
	messagesDir        = "requests/messages/"
	responsesDir       = "requests/responses/"
	streamTarget       = "/v1beta/models/" + testModel + ":streamGenerateContent?alt=sse"
	plainTarget        = "/v1beta/models/" + testModel + ":generateContent"
)

func TestGenerateContentFixtures(t *testing.T) {
	t.Parallel()

	googleKey := map[string]string{"x-goog-api-key": sentinel}
	readTarget := fakemodel.ReadFile("/workspace/nonce.txt")

	tests := []fixtureCase{
		{
			file: "gemini-first-turn.json", path: streamTarget, headers: googleKey, choice: readTarget,
			wantModel: testModel, wantTools: 15, wantFirst: "update_topic", wantCreds: []string{sentinel},
			wantCall: "read_file", wantCallArg: "file_path",
		},
		{
			file: "gemini-second-turn.json", path: streamTarget, headers: googleKey, choice: readTarget,
			wantModel: testModel, wantTools: 15, wantFirst: "update_topic", wantCreds: []string{sentinel},
			wantResult: &toolResultWant{name: "read_file", nonce: "nonce-12d198c2596b0a12", output: `{"output":"nonce-12d198c2596b0a12\n"}`},
			wantCall:   "read_file", wantCallArg: "file_path",
		},
		{
			file: "gemini-auxiliary.json", path: plainTarget, headers: googleKey, auxiliary: true,
			wantModel: testModel, wantCreds: []string{sentinel},
		},
		{
			file: "opencode1-first-turn.json", path: streamTarget, headers: googleKey, choice: readTarget,
			wantModel: testModel, wantTools: 10, wantFirst: "bash", wantCreds: []string{sentinel},
			wantCall: "read", wantCallArg: "filePath",
		},
		{
			file: "opencode1-second-turn.json", path: streamTarget, headers: googleKey, choice: readTarget,
			wantModel: testModel, wantTools: 10, wantFirst: "bash", wantCreds: []string{sentinel},
			wantResult: &toolResultWant{name: "read", nonce: "nonce-091507c2df524f73"},
			wantCall:   "read", wantCallArg: "filePath",
		},
		{
			file: "opencode2-first-turn.json", path: streamTarget, headers: googleKey, choice: readTarget,
			wantModel: testModel, wantTools: 12, wantFirst: "edit", wantCreds: []string{sentinel},
			wantCall: "read", wantCallArg: "path",
		},
		{
			file: "opencode2-second-turn.json", path: streamTarget, headers: googleKey, choice: readTarget,
			wantModel: testModel, wantTools: 12, wantFirst: "edit", wantCreds: []string{sentinel},
			wantResult: &toolResultWant{name: "read", nonce: "nonce-3fa0a6ef9d4e9990"},
			wantCall:   "read", wantCallArg: "path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()

			tt.file = generateContentDir + tt.file
			replayFixture(t, fakemodel.WireGenerateContent, tt)
		})
	}
}

func TestGenerateContentToolResultKeepsWorkspacePrefix(t *testing.T) {
	t.Parallel()

	for _, file := range []string{"opencode1-second-turn.json", "opencode2-second-turn.json"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			srv := fakemodel.Start(t, []fakemodel.Response{{Call: fakemodel.ReadFile("/workspace/nonce.txt")}})

			send(t, srv.URL(), http.MethodPost, streamTarget, nil, loadFixture(t, generateContentDir+file))

			results := exchangeAt(t, srv, 0).ToolResults
			if len(results) != 1 || !strings.Contains(results[0].Output, "/workspace/scripted-nonce-") {
				t.Errorf("Exchange.ToolResults for %s = %+v, want one result whose output names /workspace/scripted-nonce-", file, results)
			}
		})
	}
}

func TestGenerateContentDroppedMembers(t *testing.T) {
	t.Parallel()

	files := []string{
		generateContentDir + "gemini-first-turn.json",
		generateContentDir + "gemini-second-turn.json",
		generateContentDir + "gemini-auxiliary.json",
		generateContentDir + "opencode1-first-turn.json",
		generateContentDir + "opencode1-second-turn.json",
		generateContentDir + "opencode2-first-turn.json",
		generateContentDir + "opencode2-second-turn.json",
	}
	dropped := []string{
		"systemInstruction",
		"toolConfig",
		"contents[].parts[].text",
		"contents[].parts[].functionCall",
		"contents[].parts[].thoughtSignature",
		"tools[].functionDeclarations[].description",
		"generationConfig.temperature",
		"generationConfig.topP",
		"generationConfig.topK",
		"generationConfig.thinkingConfig",
		"generationConfig.responseMimeType",
		"generationConfig.maxOutputTokens",
	}

	assertMembersDropped(t, files, dropped)

	var root any
	unmarshal(t, loadFixture(t, files[0]), &root)
	if !hasMember(root, strings.Split("tools[].functionDeclarations[].name", ".")) {
		t.Error("hasMember(tools[].functionDeclarations[].name) = false on a fixture that declares tools, want true")
	}
}

func TestGenerateContentParametersJSONSchemaWinsOverParameters(t *testing.T) {
	t.Parallel()

	const preferred = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
	const shadowed = `{"type":"object","properties":{"other":{"type":"string"}},"required":["other"]}`
	body := `{"tools":[{"functionDeclarations":[{"name":"read","parameters":` + shadowed + `,"parametersJsonSchema":` + preferred + `}]}]}`
	srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

	send(t, srv.URL(), http.MethodPost, streamTarget, nil, []byte(body))

	tools := exchangeAt(t, srv, 0).Tools
	if len(tools) != 1 || string(tools[0].Parameters) != preferred {
		t.Errorf("Exchange.Tools = %+v, want read declared with %s", tools, preferred)
	}
}

func TestGenerateContentStreamingComesFromTheMethodName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target string
		want   bool
	}{
		{plainTarget, false},
		{plainTarget + "?key=abc", false},
		{streamTarget, true},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()

			w := testWires[0]
			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

			reply := send(t, srv.URL(), http.MethodPost, tt.target, nil, []byte(w.turnBody))

			ex := exchangeAt(t, srv, 0)
			if reply.status != http.StatusOK || ex.Streaming != tt.want || ex.Model != testModel {
				t.Errorf("POST %s = (status %d, Streaming %t, Model %q), want (200, %t, %q)", tt.target, reply.status, ex.Streaming, ex.Model, tt.want, testModel)
			}
			if streamed := bytes.HasPrefix(reply.body, []byte("data: ")); streamed != tt.want {
				t.Errorf("POST %s body streamed = %t, want %t", tt.target, streamed, tt.want)
			}
		})
	}
}

func TestGenerateContentUnsupportedShapes(t *testing.T) {
	t.Parallel()

	w := testWires[0]
	tests := []struct {
		name string
		body string
	}{
		{"responseSchema", `{"contents":[],"generationConfig":{"responseSchema":{"type":"OBJECT"}}}`},
		{"responseSchema beside a tool", `{"generationConfig":{"responseSchema":{"type":"OBJECT"}},"tools":[{"functionDeclarations":[{"name":"read_file","parametersJsonSchema":` + readSchema + `}]}]}`},
		{"non-scalar property", `{"generationConfig":{"responseJsonSchema":{"type":"OBJECT","properties":{"tags":{"type":"ARRAY"}}}}}`},
		{"number property", `{"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"score":{"type":"number"}}}}}`},
		{"boolean property", `{"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}`},
		{"schema that is not an object", `{"generationConfig":{"responseJsonSchema":{"type":"ARRAY"}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, ledger := startLedgered(t, nil)

			reply := w.post(t, srv, tt.body)

			assertFailureRow(t, srv, ledger, reply, failureWant{
				seq:    1,
				status: http.StatusBadRequest, kind: fakemodel.ExchangeUnsupported, wire: w.wire,
				message:  requestLabel(1, http.MethodPost, w.path) + " asks for a response schema this server does not answer",
				envelope: w.envelope,
			})
		})
	}
}

type generateContentChunk struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

func runOnce(t *testing.T, w testWire, target string, response fakemodel.Response) (httpReply, fakemodel.Exchange) {
	t.Helper()

	srv := fakemodel.Start(t, []fakemodel.Response{response})
	reply := send(t, srv.URL(), http.MethodPost, target, nil, []byte(w.turnBody))
	return reply, exchangeAt(t, srv, 0)
}

func TestGenerateContentAnswerChunk(t *testing.T) {
	t.Parallel()

	w := testWires[0]
	call := fakemodel.ReadFile("/workspace/nonce.txt")

	tests := []struct {
		name      string
		target    string
		response  fakemodel.Response
		wantUsage fakemodel.Usage
	}{
		{"text, streaming", streamTarget, fakemodel.Response{Text: "answer", Usage: fullUsage}, fakemodel.Usage{Prompt: 500, Candidates: 40, Thoughts: 12, CachedContent: 100}},
		{"text, plain", plainTarget, fakemodel.Response{Text: "answer", Usage: fullUsage}, fakemodel.Usage{Prompt: 500, Candidates: 40, Thoughts: 12, CachedContent: 100}},
		{"call, streaming", streamTarget, fakemodel.Response{Call: call, Usage: fullUsage}, fakemodel.Usage{Prompt: 500, Candidates: 40, Thoughts: 12, CachedContent: 100}},
		{"call, plain", plainTarget, fakemodel.Response{Call: call, Usage: fullUsage}, fakemodel.Usage{Prompt: 500, Candidates: 40, Thoughts: 12, CachedContent: 100}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reply, ex := runOnce(t, w, tt.target, tt.response)

			payload := reply.body
			if strings.HasSuffix(tt.target, "streamGenerateContent?alt=sse") {
				body, ok := strings.CutPrefix(string(reply.body), "data: ")
				body, framed := strings.CutSuffix(body, "\r\n\r\n")
				if !ok || !framed || strings.Contains(body, "\n") || strings.Contains(string(reply.body), "event:") {
					t.Fatalf("stream body = %q, want one \"data: <chunk>\" event ended by two CRLF pairs and no event line", reply.body)
				}
				payload = []byte(body)
			}
			var chunk generateContentChunk
			unmarshal(t, payload, &chunk)
			if len(chunk.Candidates) != 1 || chunk.Candidates[0].Content.Role != "model" || chunk.Candidates[0].FinishReason != "STOP" || chunk.Candidates[0].Index != 0 || len(chunk.Candidates[0].Content.Parts) != 1 {
				t.Fatalf("chunk = %s, want one model candidate with finishReason STOP holding one part", payload)
			}
			if chunk.ModelVersion != testModel {
				t.Errorf("chunk modelVersion = %q, want %q", chunk.ModelVersion, testModel)
			}
			usage := chunk.UsageMetadata
			if usage.PromptTokenCount != 500 || usage.CandidatesTokenCount != 40 || usage.ThoughtsTokenCount != 12 || usage.CachedContentTokenCount != 100 || usage.TotalTokenCount != 552 {
				t.Errorf("chunk usageMetadata = %+v, want prompt 500, candidates 40, thoughts 12, cached 100, total 552", usage)
			}
			part := chunk.Candidates[0].Content.Parts[0]
			if tt.response.Call == nil && part.Text != "answer" {
				t.Errorf("chunk part text = %q, want %q", part.Text, "answer")
			}
			if tt.response.Call != nil {
				if part.FunctionCall == nil || part.FunctionCall.Name != "read_file" || string(part.FunctionCall.Args) != `{"path":"/workspace/nonce.txt"}` {
					t.Errorf("chunk part = %+v, want a functionCall to read_file with the path argument", part)
				}
			}
			if ex.Answer.Usage != tt.wantUsage {
				t.Errorf("Exchange.Answer.Usage = %+v, want %+v", ex.Answer.Usage, tt.wantUsage)
			}
			if ex.Answer.CallID != "" {
				t.Errorf("Exchange.Answer.CallID = %q, want empty on a wire without correlation ids", ex.Answer.CallID)
			}
		})
	}
}

func TestGenerateContentUsageMembersAreOmittedAtZero(t *testing.T) {
	t.Parallel()

	_, ex := runOnce(t, testWires[0], plainTarget, fakemodel.Response{Text: "answer", Usage: fakemodel.Usage{Prompt: 200, Candidates: 10}})

	var chunk struct {
		UsageMetadata map[string]int64 `json:"usageMetadata"`
	}
	unmarshal(t, ex.Answer.Body, &chunk)
	want := map[string]int64{"promptTokenCount": 200, "candidatesTokenCount": 10, "totalTokenCount": 210}
	if !reflect.DeepEqual(chunk.UsageMetadata, want) {
		t.Errorf("usageMetadata = %v, want %v with the thoughts and cached members omitted", chunk.UsageMetadata, want)
	}
}

type sseEvent struct {
	name string
	data json.RawMessage
}

func parseSSE(t testing.TB, body []byte) []sseEvent {
	t.Helper()

	text, ok := strings.CutSuffix(string(body), "\n\n")
	if !ok || strings.Contains(text, "\r") {
		t.Fatalf("stream body = %q, want events ended by a blank line with LF line endings", body)
	}
	var events []sseEvent
	for block := range strings.SplitSeq(text, "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("event block = %q, want an event line then a data line", block)
		}
		events = append(events, sseEvent{name: strings.TrimPrefix(lines[0], "event: "), data: json.RawMessage(strings.TrimPrefix(lines[1], "data: "))})
	}
	return events
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, event := range events {
		names[i] = event.name
	}
	return names
}

func eventData(t testing.TB, events []sseEvent, name string) json.RawMessage {
	t.Helper()

	for _, event := range events {
		if event.name == name {
			return event.data
		}
	}
	t.Fatalf("stream carries no %q event, want one; events: %q", name, eventNames(events))
	return nil
}

func TestMessagesFixtures(t *testing.T) {
	t.Parallel()

	twoCredentialHeaders := map[string]string{"x-api-key": sentinel, "Authorization": "Bearer " + sentinel}
	oneCredentialHeader := map[string]string{"x-api-key": sentinel}
	readTool := fakemodel.ReadFile("/workspace/nonce.txt")

	tests := []fixtureCase{
		{
			file: "claude-first-turn.json", path: "/v1/messages?beta=true", headers: twoCredentialHeaders, choice: readTool,
			wantTools: 24, wantFirst: "Agent", wantCreds: []string{sentinel, sentinel},
			wantCall: "Read", wantCallArg: "file_path",
		},
		{
			file: "claude-second-turn.json", path: "/v1/messages?beta=true", headers: twoCredentialHeaders, choice: readTool,
			wantTools: 24, wantFirst: "Agent", wantCreds: []string{sentinel, sentinel},
			wantResult: &toolResultWant{callID: "toolu_scripted_1", nonce: "nonce-9f68d9642a82d8aa"},
			wantCall:   "Read", wantCallArg: "file_path",
		},
		{
			file: "copilot-first-turn.json", path: "/v1/messages", headers: oneCredentialHeader, choice: readTool,
			wantModel: testModel, wantTools: 19, wantFirst: "bash", wantCreds: []string{sentinel},
			wantCall: "view", wantCallArg: "path",
		},
		{
			file: "copilot-second-turn.json", path: "/v1/messages", headers: oneCredentialHeader, choice: readTool,
			wantModel: testModel, wantTools: 19, wantFirst: "bash", wantCreds: []string{sentinel},
			wantResult: &toolResultWant{callID: "toolu_scripted_1", output: "nonce-0fd2d815f4006fd8\n"},
			wantCall:   "view", wantCallArg: "path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()

			tt.file = messagesDir + tt.file
			replayFixture(t, fakemodel.WireMessages, tt)
		})
	}
}

func TestMessagesDroppedMembers(t *testing.T) {
	t.Parallel()

	files := []string{
		messagesDir + "claude-first-turn.json",
		messagesDir + "claude-second-turn.json",
		messagesDir + "copilot-first-turn.json",
		messagesDir + "copilot-second-turn.json",
	}
	dropped := []string{
		"system",
		"metadata",
		"max_tokens",
		"temperature",
		"thinking",
		"context_management",
		"tools[].description",
		"messages[].content[].text",
		"messages[].content[].cache_control",
		"messages[].content[].input",
	}

	assertMembersDropped(t, files, dropped)

	var root any
	unmarshal(t, loadFixture(t, files[1]), &root)
	if !hasMember(root, strings.Split("messages[].content[].tool_use_id", ".")) {
		t.Error("hasMember(messages[].content[].tool_use_id) = false on a fixture that carries a tool result, want true")
	}
}

func TestMessagesToolResultContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"string", `"plain output"`, "plain output"},
		{"text blocks joined in order", `[{"type":"text","text":"first "},{"type":"text","text":"second"}]`, "first second"},
		{"non-text blocks ignored", `[{"type":"text","text":"kept"},{"type":"image","source":{}}]`, "kept"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := `{"model":"` + testModel + `","stream":true,"messages":[` +
				`{"role":"user","content":"a plain string message"},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":` + tt.content + `}]}],` +
				`"tools":[{"name":"read_file","input_schema":` + readSchema + `}]}`
			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

			send(t, srv.URL(), http.MethodPost, "/v1/messages", nil, []byte(body))

			want := []fakemodel.ToolResult{{CallID: "toolu_x", Output: tt.want}}
			if got := exchangeAt(t, srv, 0).ToolResults; !slices.Equal(got, want) {
				t.Errorf("Exchange.ToolResults for content %s = %+v, want %+v", tt.content, got, want)
			}
		})
	}
}

func TestMessagesUnsupportedShapes(t *testing.T) {
	t.Parallel()

	w := testWires[1]
	tests := []struct {
		name string
		body string
	}{
		{"stream false", `{"model":"` + testModel + `","stream":false,"messages":[],"tools":[{"name":"read_file","input_schema":` + readSchema + `}]}`},
		{"stream absent", `{"model":"` + testModel + `","messages":[],"tools":[{"name":"read_file","input_schema":` + readSchema + `}]}`},
		{"stream absent without tools", `{"model":"` + testModel + `","messages":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, ledger := startLedgered(t, nil)

			reply := w.post(t, srv, tt.body)

			assertFailureRow(t, srv, ledger, reply, failureWant{
				seq:    1,
				status: http.StatusBadRequest, kind: fakemodel.ExchangeUnsupported, wire: w.wire,
				message:  requestLabel(1, http.MethodPost, w.path) + " asks for a non-streaming answer this server does not answer",
				envelope: w.envelope,
			})
			if ex := exchangeAt(t, srv, 0); ex.Streaming {
				t.Errorf("Exchange.Streaming = true, want false")
			}
		})
	}
}

func TestMessagesAnswerStream(t *testing.T) {
	t.Parallel()

	w := testWires[1]
	readFile := fakemodel.ReadFile("/workspace/nonce.txt")

	tests := []struct {
		name      string
		response  fakemodel.Response
		wantInput int64
		wantOut   int64
		wantUsage fakemodel.Usage
	}{
		{
			"input excludes cache, output folds thoughts",
			fakemodel.Response{Text: "answer", Usage: fullUsage},
			370, 52, fakemodel.Usage{Prompt: 500, Candidates: 52, CachedContent: 100, CacheWrite: 30},
		},
		{
			"no cache and no thoughts",
			fakemodel.Response{Text: "answer", Usage: fakemodel.Usage{Prompt: 200, Candidates: 10}},
			200, 10, fakemodel.Usage{Prompt: 200, Candidates: 10},
		},
		{
			"call answer",
			fakemodel.Response{Call: readFile, Usage: fullUsage},
			370, 52, fakemodel.Usage{Prompt: 500, Candidates: 52, CachedContent: 100, CacheWrite: 30},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, ex := runOnce(t, w, w.path, tt.response)

			events := parseSSE(t, ex.Answer.Body)
			wantNames := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
			if got := eventNames(events); !slices.Equal(got, wantNames) {
				t.Fatalf("event names = %q, want %q", got, wantNames)
			}
			var start struct {
				Message struct {
					Usage struct {
						InputTokens              int64 `json:"input_tokens"`
						CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
						OutputTokens             int64 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			unmarshal(t, eventData(t, events, "message_start"), &start)
			usage := start.Message.Usage
			if usage.InputTokens != tt.wantInput || usage.CacheReadInputTokens != tt.wantUsage.CachedContent || usage.CacheCreationInputTokens != tt.wantUsage.CacheWrite || usage.OutputTokens != 1 {
				t.Errorf("message_start usage = %+v, want input %d, cache read %d, cache creation %d, output 1", usage, tt.wantInput, tt.wantUsage.CachedContent, tt.wantUsage.CacheWrite)
			}
			var stop struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			unmarshal(t, eventData(t, events, "message_delta"), &stop)
			wantStop := "end_turn"
			if tt.response.Call != nil {
				wantStop = "tool_use"
			}
			if stop.Usage.OutputTokens != tt.wantOut || stop.Delta.StopReason != wantStop {
				t.Errorf("message_delta = (output_tokens %d, stop_reason %q), want (%d, %q)", stop.Usage.OutputTokens, stop.Delta.StopReason, tt.wantOut, wantStop)
			}
			if ex.Answer.Usage != tt.wantUsage {
				t.Errorf("Exchange.Answer.Usage = %+v, want %+v", ex.Answer.Usage, tt.wantUsage)
			}
			assertMessagesContent(t, events, ex, tt.response.Call != nil)
		})
	}
}

func assertMessagesContent(t *testing.T, events []sseEvent, ex fakemodel.Exchange, isCall bool) {
	t.Helper()

	var block struct {
		ContentBlock struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Text  *string         `json:"text"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
	}
	var delta struct {
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	unmarshal(t, eventData(t, events, "content_block_start"), &block)
	unmarshal(t, eventData(t, events, "content_block_delta"), &delta)

	if !isCall {
		if block.ContentBlock.Type != "text" || delta.Delta.Type != "text_delta" || delta.Delta.Text != "answer" || ex.Answer.CallID != "" {
			t.Errorf("text answer (block %+v, delta %+v, CallID %q), want a text block filled by one text_delta and no call id", block.ContentBlock, delta.Delta, ex.Answer.CallID)
		}
		return
	}
	const wantArguments = `{"path":"/workspace/nonce.txt"}`
	if block.ContentBlock.Type != "tool_use" || block.ContentBlock.ID != "toolu_scripted_1" || block.ContentBlock.Name != "read_file" || string(block.ContentBlock.Input) != "{}" {
		t.Errorf("content_block_start block = %+v, want tool_use toolu_scripted_1 read_file with input {}", block.ContentBlock)
	}
	if delta.Delta.Type != "input_json_delta" || delta.Delta.PartialJSON != wantArguments {
		t.Errorf("content_block_delta delta = %+v, want one input_json_delta holding %s", delta.Delta, wantArguments)
	}
	if ex.Answer.CallID != "toolu_scripted_1" || ex.Answer.Call == nil || string(ex.Answer.Call.Arguments) != wantArguments {
		t.Errorf("Exchange.Answer (CallID, Call) = (%q, %+v), want (%q, arguments %s)", ex.Answer.CallID, ex.Answer.Call, "toolu_scripted_1", wantArguments)
	}
}

func TestMessagesCredentialHeaders(t *testing.T) {
	t.Parallel()

	w := testWires[1]
	srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})
	headers := map[string]string{"x-goog-api-key": "goog-key", "x-api-key": "api-key", "Authorization": "Bearer bearer-token"}

	send(t, srv.URL(), http.MethodPost, w.path, headers, []byte(w.turnBody))

	got := slices.Sorted(slices.Values(exchangeAt(t, srv, 0).Credentials))
	if want := []string{"api-key", "bearer-token"}; !slices.Equal(got, want) {
		t.Errorf("Exchange.Credentials = %q, want %q with x-goog-api-key not read", got, want)
	}
}

func TestResponsesFixtures(t *testing.T) {
	t.Parallel()

	bearer := map[string]string{"Authorization": "Bearer " + sentinel}
	catTool := fakemodel.CatFile("/workspace/nonce.txt")

	tests := []fixtureCase{
		{
			file: "codex-first-turn.json", path: "/v1/responses", headers: bearer, choice: catTool,
			wantModel: testModel, wantTools: 7, wantFirst: "exec_command", wantCreds: []string{sentinel},
			wantCall: "exec_command", wantCallArg: "cmd",
		},
		{
			file: "codex-second-turn.json", path: "/v1/responses", headers: bearer, choice: catTool,
			wantModel: testModel, wantTools: 7, wantFirst: "exec_command", wantCreds: []string{sentinel},
			wantResult: &toolResultWant{
				callID: "call_scripted_1",
				nonce:  "nonce-e4b641abb1af099f",
				output: "Chunk ID: 415843\nWall time: 0.0000 seconds\nProcess exited with code 0\nOriginal token count: 6\nOutput:\nnonce-e4b641abb1af099f\n",
			},
			wantCall: "exec_command", wantCallArg: "cmd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()

			tt.file = responsesDir + tt.file
			replayFixture(t, fakemodel.WireResponses, tt)
		})
	}
}

func TestResponsesDeclaresFunctionsAndNamespaceMembersAsTools(t *testing.T) {
	t.Parallel()

	member := func(declared []fakemodel.Tool) (fakemodel.FunctionCall, error) {
		for _, tool := range declared {
			if tool.Namespace != "" {
				return fakemodel.FunctionCall{Name: tool.Name, Namespace: tool.Namespace, Arguments: json.RawMessage(`{}`)}, nil
			}
		}
		return fakemodel.FunctionCall{}, fmt.Errorf("no namespace member among %d declared tools", len(declared))
	}
	srv := fakemodel.Start(t, []fakemodel.Response{{Call: member}})
	body := `{"model":"` + testModel + `","stream":true,"input":[],"tools":[` +
		`{"type":"function","name":"read_file","parameters":` + readSchema + `},` +
		`{"type":"namespace","name":"mcp__sortie_tools","tools":[{"type":"function","name":"sortie_status","parameters":{"type":"object"}}]},` +
		`{"type":"web_search"}]}`

	reply := send(t, srv.URL(), http.MethodPost, "/v1/responses", nil, []byte(body))

	got := exchangeAt(t, srv, 0).Tools
	want := []fakemodel.Tool{{Name: "read_file"}, {Name: "sortie_status", Namespace: "mcp__sortie_tools"}}
	if len(got) != len(want) || got[0].Name != want[0].Name || got[0].Namespace != "" || got[1].Name != want[1].Name || got[1].Namespace != want[1].Namespace {
		t.Errorf("Exchange.Tools = %+v, want names and namespaces %+v with web_search left out", got, want)
	}
	if !strings.Contains(string(reply.body), `"name":"sortie_status","namespace":"mcp__sortie_tools"`) {
		t.Errorf("answer body = %s, want a function_call carrying the member's namespace", reply.body)
	}
}

func TestResponsesDroppedMembers(t *testing.T) {
	t.Parallel()

	files := []string{responsesDir + "codex-first-turn.json", responsesDir + "codex-second-turn.json"}
	dropped := []string{
		"instructions",
		"tool_choice",
		"parallel_tool_calls",
		"reasoning",
		"store",
		"include",
		"prompt_cache_key",
		"client_metadata",
		"tools[].description",
		"tools[].strict",
		"tools[].tools",
		"tools[].external_web_access",
		"input[].content",
		"input[].arguments",
	}

	assertMembersDropped(t, files, dropped)

	var root any
	unmarshal(t, loadFixture(t, files[1]), &root)
	if !hasMember(root, strings.Split("input[].call_id", ".")) {
		t.Error("hasMember(input[].call_id) = false on a fixture that carries a tool result, want true")
	}
}

func TestResponsesFunctionCallOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []fakemodel.ToolResult
	}{
		{"string output", `[{"type":"function_call_output","call_id":"call_x","output":"plain output"}]`, []fakemodel.ToolResult{{CallID: "call_x", Output: "plain output"}}},
		{
			"parts joined in order",
			`[{"type":"function_call_output","call_id":"call_x","output":[{"type":"input_text","text":"first "},{"type":"input_text","text":"second"}]}]`,
			[]fakemodel.ToolResult{{CallID: "call_x", Output: "first second"}},
		},
		{"other items ignored", `[{"type":"message","role":"user"},{"type":"function_call","call_id":"call_y","name":"read_file"}]`, nil},
		{"input that is not an array", `"just a prompt"`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := `{"model":"` + testModel + `","stream":true,"input":` + tt.input + `,` +
				`"tools":[{"type":"function","name":"read_file","parameters":` + readSchema + `}]}`
			srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

			reply := send(t, srv.URL(), http.MethodPost, "/v1/responses", nil, []byte(body))

			ex := exchangeAt(t, srv, 0)
			if reply.status != http.StatusOK || ex.Kind != fakemodel.ExchangeTurn {
				t.Errorf("request with input %s = (status %d, Kind %q), want (200, %q)", tt.input, reply.status, ex.Kind, fakemodel.ExchangeTurn)
			}
			if !slices.Equal(ex.ToolResults, tt.want) {
				t.Errorf("Exchange.ToolResults for input %s = %+v, want %+v", tt.input, ex.ToolResults, tt.want)
			}
		})
	}
}

func TestResponsesUnsupportedShapes(t *testing.T) {
	t.Parallel()

	w := testWires[2]
	tool := `{"type":"function","name":"read_file","parameters":` + readSchema + `}`
	tests := []struct {
		name  string
		body  string
		shape string
	}{
		{"stream false", `{"model":"` + testModel + `","stream":false,"input":[],"tools":[` + tool + `]}`, "non-streaming answer"},
		{"stream absent", `{"model":"` + testModel + `","input":[],"tools":[` + tool + `]}`, "non-streaming answer"},
		{"json_schema text format", `{"model":"` + testModel + `","stream":true,"input":[],"text":{"format":{"type":"json_schema","name":"out","schema":{}}},"tools":[` + tool + `]}`, "response schema"},
		{"json_schema text format without tools", `{"model":"` + testModel + `","stream":true,"input":[],"text":{"format":{"type":"json_schema"}}}`, "response schema"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, ledger := startLedgered(t, nil)

			reply := w.post(t, srv, tt.body)

			assertFailureRow(t, srv, ledger, reply, failureWant{
				seq:    1,
				status: http.StatusBadRequest, kind: fakemodel.ExchangeUnsupported, wire: w.wire,
				message:  requestLabel(1, http.MethodPost, w.path) + " asks for a " + tt.shape + " this server does not answer",
				envelope: w.envelope,
			})
		})
	}
}

func TestResponsesPlainTextFormatIsAnswered(t *testing.T) {
	t.Parallel()

	w := testWires[2]
	body := `{"model":"` + testModel + `","stream":true,"input":[],"text":{"format":{"type":"text"}},"tools":[{"type":"function","name":"read_file","parameters":` + readSchema + `}]}`
	srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})

	reply := w.post(t, srv, body)

	if reply.status != http.StatusOK {
		t.Errorf("status with text.format.type text = %d, want 200", reply.status)
	}
}

func TestResponsesCredentialHeader(t *testing.T) {
	t.Parallel()

	w := testWires[2]
	srv := fakemodel.Start(t, []fakemodel.Response{{Text: "answer"}})
	headers := map[string]string{"x-goog-api-key": "goog-key", "x-api-key": "api-key", "Authorization": "Bearer bearer-token"}

	send(t, srv.URL(), http.MethodPost, w.path, headers, []byte(w.turnBody))

	if got, want := exchangeAt(t, srv, 0).Credentials, []string{"bearer-token"}; !slices.Equal(got, want) {
		t.Errorf("Exchange.Credentials = %q, want %q with only Authorization read", got, want)
	}
}

func TestResponsesAnswerStream(t *testing.T) {
	t.Parallel()

	w := testWires[2]
	readFile := fakemodel.ReadFile("/workspace/nonce.txt")

	textEvents := []string{
		"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed",
	}
	callEvents := []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.output_item.done", "response.completed",
	}
	tests := []struct {
		name       string
		response   fakemodel.Response
		wantEvents []string
	}{
		{"text", fakemodel.Response{Text: "answer", Usage: fullUsage}, textEvents},
		{"call", fakemodel.Response{Call: readFile, Usage: fullUsage}, callEvents},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, ex := runOnce(t, w, w.path, tt.response)

			events := parseSSE(t, ex.Answer.Body)
			if got := eventNames(events); !slices.Equal(got, tt.wantEvents) {
				t.Fatalf("event names = %q, want %q", got, tt.wantEvents)
			}
			var completed struct {
				Response struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Usage  struct {
						InputTokens        int64 `json:"input_tokens"`
						InputTokensDetails struct {
							CachedTokens int64 `json:"cached_tokens"`
						} `json:"input_tokens_details"`
						OutputTokens        int64 `json:"output_tokens"`
						OutputTokensDetails struct {
							ReasoningTokens int64 `json:"reasoning_tokens"`
						} `json:"output_tokens_details"`
						TotalTokens int64 `json:"total_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			unmarshal(t, eventData(t, events, "response.completed"), &completed)
			usage := completed.Response.Usage
			if usage.InputTokens != 500 || usage.InputTokensDetails.CachedTokens != 100 || usage.OutputTokens != 52 || usage.OutputTokensDetails.ReasoningTokens != 12 || usage.TotalTokens != 552 {
				t.Errorf("response.completed usage = %+v, want input 500, cached 100, output 52, reasoning 12, total 552", usage)
			}
			if completed.Response.ID != "resp_scripted_1" || completed.Response.Status != "completed" {
				t.Errorf("response.completed (id, status) = (%q, %q), want (%q, %q)", completed.Response.ID, completed.Response.Status, "resp_scripted_1", "completed")
			}
			wantUsage := fakemodel.Usage{Prompt: 500, Candidates: 40, Thoughts: 12, CachedContent: 100}
			if ex.Answer.Usage != wantUsage {
				t.Errorf("Exchange.Answer.Usage = %+v, want %+v", ex.Answer.Usage, wantUsage)
			}
			assertResponsesItems(t, events, ex, tt.response.Call != nil)
		})
	}
}

func assertResponsesItems(t *testing.T, events []sseEvent, ex fakemodel.Exchange, isCall bool) {
	t.Helper()

	var added, done struct {
		Item struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	unmarshal(t, eventData(t, events, "response.output_item.added"), &added)
	unmarshal(t, eventData(t, events, "response.output_item.done"), &done)

	if !isCall {
		var text struct {
			Delta  string `json:"delta"`
			ItemID string `json:"item_id"`
		}
		unmarshal(t, eventData(t, events, "response.output_text.delta"), &text)
		if added.Item.ID != "msg_scripted_1" || added.Item.Type != "message" || text.Delta != "answer" || text.ItemID != "msg_scripted_1" || ex.Answer.CallID != "" {
			t.Errorf("text answer (item %+v, delta %+v, CallID %q), want message msg_scripted_1 carrying the text and no call id", added.Item, text, ex.Answer.CallID)
		}
		return
	}
	const wantArguments = `{"path":"/workspace/nonce.txt"}`
	var argDelta, argDone struct {
		Delta     string `json:"delta"`
		Arguments string `json:"arguments"`
	}
	unmarshal(t, eventData(t, events, "response.function_call_arguments.delta"), &argDelta)
	unmarshal(t, eventData(t, events, "response.function_call_arguments.done"), &argDone)
	if added.Item.ID != "fc_scripted_1" || added.Item.Type != "function_call" || added.Item.CallID != "call_scripted_1" || added.Item.Name != "read_file" || added.Item.Arguments != "" {
		t.Errorf("response.output_item.added item = %+v, want function_call fc_scripted_1 call_scripted_1 read_file with empty arguments", added.Item)
	}
	if done.Item.Status != "completed" || done.Item.Arguments != wantArguments {
		t.Errorf("response.output_item.done item = %+v, want completed with arguments %s", done.Item, wantArguments)
	}
	if argDelta.Delta != wantArguments || argDone.Arguments != wantArguments {
		t.Errorf("argument events (delta, done) = (%q, %q), want both %s", argDelta.Delta, argDone.Arguments, wantArguments)
	}
	if ex.Answer.CallID != "call_scripted_1" || ex.Answer.Call == nil || string(ex.Answer.Call.Arguments) != wantArguments {
		t.Errorf("Exchange.Answer (CallID, Call) = (%q, %+v), want (%q, arguments %s)", ex.Answer.CallID, ex.Answer.Call, "call_scripted_1", wantArguments)
	}
}

type goldenCase struct {
	file     string
	wire     testWire
	target   string
	response fakemodel.Response
}

func goldenCases() []goldenCase {
	text := fakemodel.Response{Text: "He said \"ok\".\nDone.", Usage: fullUsage}
	call := fakemodel.Response{Call: fakemodel.ReadFile("/workspace/nonce.txt"), Usage: fullUsage}
	return []goldenCase{
		{"generateContent/text-stream.sse", testWires[0], streamTarget, text},
		{"generateContent/call-stream.sse", testWires[0], streamTarget, call},
		{"generateContent/text-plain.json", testWires[0], plainTarget, text},
		{"generateContent/call-plain.json", testWires[0], plainTarget, call},
		{"messages/text-stream.sse", testWires[1], testWires[1].path, text},
		{"messages/call-stream.sse", testWires[1], testWires[1].path, call},
		{"responses/text-stream.sse", testWires[2], testWires[2].path, text},
		{"responses/call-stream.sse", testWires[2], testWires[2].path, call},
	}
}

// git normalizes line endings in testdata, so a golden holds LF where the
// generateContent stream writes CRLF; the CRLF framing is asserted apart.
func goldenForm(body []byte) []byte {
	return bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
}

func assertGoldens(t *testing.T, wire fakemodel.Wire) {
	t.Helper()

	for _, c := range goldenCases() {
		if c.wire.wire != wire {
			continue
		}
		t.Run(c.file, func(t *testing.T) {
			t.Parallel()

			want := loadFixture(t, "answers/"+c.file)

			reply, ex := runOnce(t, c.wire, c.target, c.response)

			if got := goldenForm(reply.body); !bytes.Equal(got, want) {
				t.Errorf("response body for %s =\n%s\nwant\n%s", c.file, got, want)
			}
			if !bytes.Equal(ex.Answer.Body, reply.body) {
				t.Errorf("Exchange.Answer.Body for %s = %q, want the response body %q", c.file, ex.Answer.Body, reply.body)
			}
		})
	}
}

func TestGenerateContentGoldens(t *testing.T) {
	t.Parallel()

	assertGoldens(t, fakemodel.WireGenerateContent)
}

func TestGenerateContentStreamUsesCRLFFraming(t *testing.T) {
	t.Parallel()

	reply, _ := runOnce(t, testWires[0], streamTarget, fakemodel.Response{Text: "answer"})

	if !bytes.HasSuffix(reply.body, []byte("\r\n\r\n")) || bytes.Count(reply.body, []byte("\n")) != bytes.Count(reply.body, []byte("\r\n")) {
		t.Errorf("stream body = %q, want every line ended by CRLF, closing with two CRLF pairs", reply.body)
	}
}

func TestMessagesGoldens(t *testing.T) {
	t.Parallel()

	assertGoldens(t, fakemodel.WireMessages)
}

func TestResponsesGoldens(t *testing.T) {
	t.Parallel()

	assertGoldens(t, fakemodel.WireResponses)
}
