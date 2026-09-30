package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// messagesCodec serves the messages wire: POST /v1/messages, whose request
// body carries the model and asks for a server-sent stream. A body without
// "stream": true is a shape the codec does not answer.
//
// A request that declares no function tool is auxiliary: it is answered
// outside the script and its usage still counts, because a runtime keeps the
// figure of every request it makes.
//
// A stream is a run of events, each "event: <type>", "data: <json>" and a
// blank line. The input side of the usage rides on message_start with the
// cache members apart from input_tokens, and the closing output count rides
// on message_delta.
type messagesCodec struct{}

func (messagesCodec) wire() Wire { return WireMessages }

func (messagesCodec) credentialHeaders() []string {
	return []string{"x-api-key", "Authorization"}
}

func (messagesCodec) route(method, path string) (route, bool) {
	return route{}, method == http.MethodPost && path == "/v1/messages"
}

type messagesRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Content   json.RawMessage `json:"content"`
}

func (messagesCodec) decode(_ route, body []byte) (decoded, error) {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return decoded{}, err
	}

	dec := decoded{model: req.Model, streaming: req.Stream}
	if !req.Stream {
		dec.unsupported = "non-streaming answer"
	}
	for _, tool := range req.Tools {
		if present(tool.InputSchema) {
			dec.tools = append(dec.tools, Tool{Name: tool.Name, Parameters: tool.InputSchema})
		}
	}
	for _, message := range req.Messages {
		var blocks []messagesBlock
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_result" {
				dec.toolResults = append(dec.toolResults, ToolResult{CallID: block.ToolUseID, Output: toolResultText(block.Content)})
			}
		}
	}
	return dec, nil
}

// toolResultText reads a tool_result content member, which is either a
// string or a list of blocks whose text parts are joined in order.
func toolResultText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []messagesBlock
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var joined strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			joined.WriteString(block.Text)
		}
	}
	return joined.String()
}

type messagesEvent struct {
	Type         string            `json:"type"`
	Message      *messagesMessage  `json:"message,omitempty"`
	Index        *int              `json:"index,omitempty"`
	ContentBlock *messagesContent  `json:"content_block,omitempty"`
	Delta        json.RawMessage   `json:"delta,omitempty"`
	Usage        *messagesUsageOut `json:"usage,omitempty"`
}

type messagesMessage struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []struct{}    `json:"content"`
	StopReason   *string       `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        messagesUsage `json:"usage"`
}

type messagesContent struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type messagesBlockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type messagesStopDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type messagesUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type messagesUsageOut struct {
	OutputTokens int64 `json:"output_tokens"`
}

func (messagesCodec) render(r reply) rendered {
	output := r.usage.Candidates + r.usage.Thoughts
	id := fmt.Sprintf("msg_scripted_%d", r.seq)
	stopReason := "end_turn"
	callID := ""

	var start messagesContent
	var delta messagesBlockDelta
	if r.call != nil {
		callID = fmt.Sprintf("toolu_scripted_%d", r.seq)
		stopReason = "tool_use"
		start = messagesContent{Type: "tool_use", ID: callID, Name: r.call.Name, Input: json.RawMessage("{}")}
		delta = messagesBlockDelta{Type: "input_json_delta", PartialJSON: string(r.call.Arguments)}
	} else {
		empty := ""
		start = messagesContent{Type: "text", Text: &empty}
		delta = messagesBlockDelta{Type: "text_delta", Text: r.text}
	}

	index := 0
	events := []messagesEvent{
		{Type: "message_start", Message: &messagesMessage{
			ID: id, Type: "message", Role: "assistant", Model: r.model, Content: []struct{}{},
			Usage: messagesUsage{
				InputTokens:              r.usage.Prompt - r.usage.CachedContent - r.usage.CacheWrite,
				CacheReadInputTokens:     r.usage.CachedContent,
				CacheCreationInputTokens: r.usage.CacheWrite,
				OutputTokens:             1,
			},
		}},
		{Type: "content_block_start", Index: &index, ContentBlock: &start},
		{Type: "content_block_delta", Index: &index, Delta: encodeJSON(delta)},
		{Type: "content_block_stop", Index: &index},
		{Type: "message_delta", Delta: encodeJSON(messagesStopDelta{StopReason: stopReason}), Usage: &messagesUsageOut{OutputTokens: output}},
		{Type: "message_stop"},
	}

	var stream strings.Builder
	for _, event := range events {
		fmt.Fprintf(&stream, "event: %s\ndata: %s\n\n", event.Type, encodeJSON(event))
	}

	reported := r.usage
	reported.Candidates, reported.Thoughts = output, 0
	return rendered{contentType: "text/event-stream", body: []byte(stream.String()), callID: callID, usage: reported}
}

type messagesError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (messagesCodec) errorBody(_ int, message string) []byte {
	envelope := messagesError{Type: "error"}
	envelope.Error.Type, envelope.Error.Message = "invalid_request_error", message
	return encodeJSON(envelope)
}
