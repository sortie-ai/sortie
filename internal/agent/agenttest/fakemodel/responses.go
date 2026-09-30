package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// responsesCodec serves the responses wire: POST /v1/responses, whose request
// body carries the model and asks for a server-sent stream. A body without
// "stream": true, or one asking for a json_schema text format, is a shape the
// codec does not answer.
//
// A request that declares no function tool is auxiliary: it is answered
// outside the script and its usage still counts, because a runtime keeps the
// figure of every request it makes. Tools of any other type, such as a hosted
// web search, do not make a request a turn request.
//
// A stream is a run of events, each "event: <type>", "data: <json>" and a
// blank line. The usage rides on the closing response.completed event.
type responsesCodec struct{}

func (responsesCodec) wire() Wire { return WireResponses }

func (responsesCodec) credentialHeaders() []string {
	return []string{"Authorization"}
}

func (responsesCodec) route(method, path string) (route, bool) {
	return route{}, method == http.MethodPost && path == "/v1/responses"
}

type responsesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	Tools  []struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"tools"`
	Input json.RawMessage `json:"input"`
	Text  struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"text"`
}

type responsesInputItem struct {
	Type   string          `json:"type"`
	CallID string          `json:"call_id"`
	Output json.RawMessage `json:"output"`
}

func (responsesCodec) decode(_ route, body []byte) (decoded, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return decoded{}, err
	}

	dec := decoded{model: req.Model, streaming: req.Stream}
	switch {
	case !req.Stream:
		dec.unsupported = "non-streaming answer"
	case req.Text.Format.Type == "json_schema":
		dec.unsupported = "response schema"
	}
	for _, tool := range req.Tools {
		if tool.Type == "function" {
			dec.tools = append(dec.tools, Tool{Name: tool.Name, Parameters: tool.Parameters})
		}
	}

	var items []responsesInputItem
	if json.Unmarshal(req.Input, &items) != nil {
		return dec, nil
	}
	for _, item := range items {
		if item.Type == "function_call_output" {
			dec.toolResults = append(dec.toolResults, ToolResult{CallID: item.CallID, Output: functionOutputText(item.Output)})
		}
	}
	return dec, nil
}

// functionOutputText reads a function_call_output member, which is either a
// string or a list of parts whose text is joined in order.
func functionOutputText(output json.RawMessage) string {
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(output, &parts) != nil {
		return ""
	}
	var joined strings.Builder
	for _, part := range parts {
		joined.WriteString(part.Text)
	}
	return joined.String()
}

type responsesEvent struct {
	Type        string          `json:"type"`
	Response    *responsesBody  `json:"response,omitempty"`
	OutputIndex *int            `json:"output_index,omitempty"`
	ContentIdx  *int            `json:"content_index,omitempty"`
	Item        json.RawMessage `json:"item,omitempty"`
	ItemID      string          `json:"item_id,omitempty"`
	Part        *responsesPart  `json:"part,omitempty"`
	Delta       string          `json:"delta,omitempty"`
	Text        string          `json:"text,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type responsesBody struct {
	ID     string            `json:"id"`
	Object string            `json:"object"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Model  string            `json:"model"`
	Usage  *responsesUsage   `json:"usage,omitempty"`
}

type responsesMessageItem struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Status  string          `json:"status"`
	Content []responsesPart `json:"content"`
}

type responsesCallItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesPart struct {
	Type        string   `json:"type"`
	Text        string   `json:"text"`
	Annotations []string `json:"annotations"`
}

type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`
}

func (responsesCodec) render(r reply) rendered {
	output := r.usage.Candidates + r.usage.Thoughts
	usage := &responsesUsage{
		InputTokens:  r.usage.Prompt,
		OutputTokens: output,
		TotalTokens:  r.usage.Prompt + output,
	}
	usage.InputTokensDetails.CachedTokens = r.usage.CachedContent
	usage.OutputTokensDetails.ReasoningTokens = r.usage.Thoughts

	responseID := fmt.Sprintf("resp_scripted_%d", r.seq)
	inProgress := responsesBody{ID: responseID, Object: "response", Status: "in_progress", Output: []json.RawMessage{}, Model: r.model}
	index, contentIndex := 0, 0

	events := []responsesEvent{
		{Type: "response.created", Response: &inProgress},
		{Type: "response.in_progress", Response: &inProgress},
	}
	var final json.RawMessage
	callID := ""
	if r.call != nil {
		callID = fmt.Sprintf("call_scripted_%d", r.seq)
		itemID := fmt.Sprintf("fc_scripted_%d", r.seq)
		arguments := string(r.call.Arguments)
		started := responsesCallItem{ID: itemID, Type: "function_call", Status: "in_progress", CallID: callID, Name: r.call.Name}
		finished := started
		finished.Status, finished.Arguments = "completed", arguments
		final = encodeJSON(finished)
		events = append(events,
			responsesEvent{Type: "response.output_item.added", OutputIndex: &index, Item: encodeJSON(started)},
			responsesEvent{Type: "response.function_call_arguments.delta", OutputIndex: &index, ItemID: itemID, Delta: arguments},
			responsesEvent{Type: "response.function_call_arguments.done", OutputIndex: &index, ItemID: itemID, Arguments: arguments},
			responsesEvent{Type: "response.output_item.done", OutputIndex: &index, Item: final},
		)
	} else {
		itemID := fmt.Sprintf("msg_scripted_%d", r.seq)
		empty := responsesPart{Type: "output_text", Annotations: []string{}}
		full := empty
		full.Text = r.text
		started := responsesMessageItem{ID: itemID, Type: "message", Role: "assistant", Status: "in_progress", Content: []responsesPart{}}
		finished := started
		finished.Status, finished.Content = "completed", []responsesPart{full}
		final = encodeJSON(finished)
		events = append(events,
			responsesEvent{Type: "response.output_item.added", OutputIndex: &index, Item: encodeJSON(started)},
			responsesEvent{Type: "response.content_part.added", OutputIndex: &index, ContentIdx: &contentIndex, ItemID: itemID, Part: &empty},
			responsesEvent{Type: "response.output_text.delta", OutputIndex: &index, ContentIdx: &contentIndex, ItemID: itemID, Delta: r.text},
			responsesEvent{Type: "response.output_text.done", OutputIndex: &index, ContentIdx: &contentIndex, ItemID: itemID, Text: r.text},
			responsesEvent{Type: "response.content_part.done", OutputIndex: &index, ContentIdx: &contentIndex, ItemID: itemID, Part: &full},
			responsesEvent{Type: "response.output_item.done", OutputIndex: &index, Item: final},
		)
	}
	completed := responsesBody{ID: responseID, Object: "response", Status: "completed", Output: []json.RawMessage{final}, Model: r.model, Usage: usage}
	events = append(events, responsesEvent{Type: "response.completed", Response: &completed})

	var stream strings.Builder
	for _, event := range events {
		fmt.Fprintf(&stream, "event: %s\ndata: %s\n\n", event.Type, encodeJSON(event))
	}

	reported := r.usage
	reported.CacheWrite = 0
	return rendered{contentType: "text/event-stream", body: []byte(stream.String()), callID: callID, usage: reported}
}

type responsesError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func (responsesCodec) errorBody(_ int, message string) []byte {
	var envelope responsesError
	envelope.Error.Message, envelope.Error.Type = message, "invalid_request_error"
	return encodeJSON(envelope)
}
