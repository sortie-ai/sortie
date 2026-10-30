package fakemodel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
)

const (
	assistantTargetPrefix  = "AmazonCodeWhisperer"
	controlPlanePrefix     = "KiroControlPlaneBearerService."
	operationGenerate      = "GenerateAssistantResponse"
	operationListModels    = "ListAvailableModels"
	scriptedModelID        = "scripted-model"
	eventStreamContentType = "application/vnd.amazon.eventstream"
	amzJSONContentType     = "application/x-amz-json-1.0"

	// eventStreamStringHeader is the event-stream header value type for a string.
	eventStreamStringHeader = 7
	eventStreamPreludeBytes = 12
	eventStreamTrailerBytes = 4
)

// generateAssistantCodec serves the generate-assistant-response wire: every
// operation is a POST to the root path with the operation named in the
// x-amz-target header, so the header, not the path, routes it. A runtime
// addresses the operation to either the assistant service or the control-plane
// service, and both are served alike.
//
// GenerateAssistantResponse is answered as an event stream. A request that
// declares no tool is auxiliary and answered outside the script. The
// ListAvailableModels operation lists the one scripted model, and every other
// operation the runtime makes (profile, usage limits, telemetry) is answered
// with an empty object. The wire reports no token usage.
type generateAssistantCodec struct{}

func (generateAssistantCodec) wire() Wire { return WireGenerateAssistantResponse }

func (generateAssistantCodec) credentialHeaders() []string {
	return []string{"Authorization"}
}

func (generateAssistantCodec) route(method, path string, header http.Header) (route, bool) {
	if method != http.MethodPost || path != "/" {
		return route{}, false
	}
	target := header.Get("X-Amz-Target")
	if !strings.HasPrefix(target, assistantTargetPrefix) && !strings.HasPrefix(target, controlPlanePrefix) {
		return route{}, false
	}
	_, operation, _ := strings.Cut(target, ".")
	return route{operation: operation, streaming: operation == operationGenerate}, true
}

type generateAssistantRequest struct {
	ConversationState struct {
		History []struct {
			UserInputMessage assistantUserInput `json:"userInputMessage"`
		} `json:"history"`
		CurrentMessage struct {
			UserInputMessage assistantUserInput `json:"userInputMessage"`
		} `json:"currentMessage"`
	} `json:"conversationState"`
}

type assistantUserInput struct {
	ModelID string `json:"modelId"`
	Context struct {
		Tools []struct {
			ToolSpecification struct {
				Name        string `json:"name"`
				InputSchema struct {
					JSON json.RawMessage `json:"json"`
				} `json:"inputSchema"`
			} `json:"toolSpecification"`
		} `json:"tools"`
		ToolResults []struct {
			ToolUseID string `json:"toolUseId"`
			Content   []struct {
				Text string          `json:"text"`
				JSON json.RawMessage `json:"json"`
			} `json:"content"`
		} `json:"toolResults"`
	} `json:"userInputMessageContext"`
}

func (generateAssistantCodec) decode(rt route, body []byte) (decoded, error) {
	dec := decoded{operation: rt.operation, streaming: rt.streaming}
	if rt.operation != operationGenerate {
		return dec, nil
	}

	var req generateAssistantRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return decoded{}, err
	}
	current := req.ConversationState.CurrentMessage.UserInputMessage
	dec.model = current.ModelID
	for _, tool := range current.Context.Tools {
		if present(tool.ToolSpecification.InputSchema.JSON) {
			dec.tools = append(dec.tools, Tool{Name: tool.ToolSpecification.Name, Parameters: tool.ToolSpecification.InputSchema.JSON})
		}
	}

	messages := []assistantUserInput{current}
	for _, entry := range req.ConversationState.History {
		messages = append(messages, entry.UserInputMessage)
	}
	for _, message := range messages {
		for _, result := range message.Context.ToolResults {
			var output strings.Builder
			for _, block := range result.Content {
				output.WriteString(block.Text)
				if present(block.JSON) {
					output.WriteString(compactJSON(block.JSON))
				}
			}
			dec.toolResults = append(dec.toolResults, ToolResult{CallID: result.ToolUseID, Output: output.String()})
		}
	}
	return dec, nil
}

type assistantTokenLimits struct {
	MaxInputTokens  int `json:"maxInputTokens"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

type assistantModelEntry struct {
	ModelID     string                `json:"modelId"`
	ModelName   string                `json:"modelName"`
	Description string                `json:"description,omitempty"`
	TokenLimits *assistantTokenLimits `json:"tokenLimits,omitempty"`
}

type assistantModels struct {
	Models       []assistantModelEntry `json:"models"`
	DefaultModel assistantModelEntry   `json:"defaultModel"`
}

func (generateAssistantCodec) render(r reply) rendered {
	switch r.operation {
	case operationGenerate:
		body, callID := assistantAnswerFrames(r)
		return rendered{contentType: eventStreamContentType, body: body, callID: callID}
	case operationListModels:
		listed := assistantModelEntry{
			ModelID:     scriptedModelID,
			ModelName:   scriptedModelID,
			Description: "scripted",
			TokenLimits: &assistantTokenLimits{MaxInputTokens: 200000, MaxOutputTokens: 8192},
		}
		return rendered{contentType: amzJSONContentType, body: encodeJSON(assistantModels{
			Models:       []assistantModelEntry{listed},
			DefaultModel: assistantModelEntry{ModelID: scriptedModelID, ModelName: scriptedModelID},
		})}
	}
	return rendered{contentType: amzJSONContentType, body: []byte("{}")}
}

type assistantToolUse struct {
	Name      string `json:"name"`
	ToolUseID string `json:"toolUseId"`
	Input     string `json:"input,omitempty"`
	Stop      bool   `json:"stop,omitempty"`
}

func assistantAnswerFrames(r reply) (body []byte, callID string) {
	if r.call == nil {
		return eventStreamFrame("assistantResponseEvent", encodeJSON(struct {
			Content string `json:"content"`
		}{Content: r.text})), ""
	}
	callID = fmt.Sprintf("tooluse_scripted_%d", r.seq)
	body = eventStreamFrame("toolUseEvent", encodeJSON(assistantToolUse{Name: r.call.Name, ToolUseID: callID, Input: string(r.call.Arguments)}))
	body = append(body, eventStreamFrame("toolUseEvent", encodeJSON(assistantToolUse{Name: r.call.Name, ToolUseID: callID, Stop: true}))...)
	return body, callID
}

// eventStreamFrame encodes one event-stream message: a prelude of the total and
// header lengths with its own checksum, string headers, the JSON payload, and
// a checksum over everything before it.
func eventStreamFrame(eventType string, payload []byte) []byte {
	var headers []byte
	for _, header := range [][2]string{
		{":message-type", "event"},
		{":event-type", eventType},
		{":content-type", jsonContentType},
	} {
		headers = append(headers, lengthAs[byte](len(header[0])))
		headers = append(headers, header[0]...)
		headers = append(headers, eventStreamStringHeader)
		headers = binary.BigEndian.AppendUint16(headers, lengthAs[uint16](len(header[1])))
		headers = append(headers, header[1]...)
	}

	total := eventStreamPreludeBytes + len(headers) + len(payload) + eventStreamTrailerBytes
	frame := binary.BigEndian.AppendUint32(nil, lengthAs[uint32](total))
	frame = binary.BigEndian.AppendUint32(frame, lengthAs[uint32](len(headers)))
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	frame = append(frame, headers...)
	frame = append(frame, payload...)
	return binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
}

// lengthAs narrows a length the frame fields carry. Every length here is that
// of a header or a scripted answer, far below the narrowest field's range.
func lengthAs[T byte | uint16 | uint32](n int) T {
	return T(n) //nolint:gosec // G115: the lengths are of short scripted values
}

// errorBody names the failure a ValidationException for every status: a runtime
// shows a nameless body as a generic service error and drops the message. A
// scripted refusal that needs another error type carries its own body.
func (generateAssistantCodec) errorBody(_ int, message string) []byte {
	return encodeJSON(struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}{Type: "ValidationException", Message: message})
}
