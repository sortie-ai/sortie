package fakemodel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

const generateContentModelsPrefix = "/v1beta/models/"

// generateContentCodec serves the generateContent wire: POST
// /v1beta/models/{model}:generateContent for a whole answer and
// :streamGenerateContent for a server-sent stream.
//
// A request that declares no function tool is auxiliary: it is answered
// outside the script and its usage still counts, because a runtime keeps the
// figure of every request it makes.
//
// A stream is one event, "data: <chunk>" ended by two CRLF pairs, with no
// "event:" line because the runtime's parser is anchored on "data: ". The
// runtime keeps the last chunk's usage, so the single chunk carries it.
type generateContentCodec struct{}

func (generateContentCodec) wire() Wire { return WireGenerateContent }

func (generateContentCodec) credentialHeaders() []string {
	return []string{"x-goog-api-key", "Authorization"}
}

func (generateContentCodec) route(method, path string) (route, bool) {
	if method != http.MethodPost {
		return route{}, false
	}
	rest, ok := strings.CutPrefix(path, generateContentModelsPrefix)
	if !ok {
		return route{}, false
	}
	colon := strings.LastIndex(rest, ":")
	if colon <= 0 || strings.Contains(rest[:colon], "/") {
		return route{}, false
	}
	switch rest[colon+1:] {
	case "generateContent":
		return route{model: rest[:colon]}, true
	case "streamGenerateContent":
		return route{model: rest[:colon], streaming: true}, true
	}
	return route{}, false
}

type generateContentRequest struct {
	Tools []struct {
		FunctionDeclarations []struct {
			Name                 string          `json:"name"`
			ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
			Parameters           json.RawMessage `json:"parameters"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	Contents []struct {
		Parts []struct {
			FunctionResponse *struct {
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"contents"`
	GenerationConfig struct {
		ResponseSchema     json.RawMessage `json:"responseSchema"`
		ResponseJSONSchema json.RawMessage `json:"responseJsonSchema"`
	} `json:"generationConfig"`
}

func (generateContentCodec) decode(rt route, body []byte) (decoded, error) {
	var req generateContentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return decoded{}, err
	}

	dec := decoded{model: rt.model, streaming: rt.streaming}
	for _, tool := range req.Tools {
		for _, declaration := range tool.FunctionDeclarations {
			parameters := declaration.ParametersJSONSchema
			if !present(parameters) {
				parameters = declaration.Parameters
			}
			dec.tools = append(dec.tools, Tool{Name: declaration.Name, Parameters: parameters})
		}
	}
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.FunctionResponse == nil {
				continue
			}
			dec.toolResults = append(dec.toolResults, ToolResult{
				Name:   part.FunctionResponse.Name,
				Output: compactJSON(part.FunctionResponse.Response),
			})
		}
	}

	switch config := req.GenerationConfig; {
	case present(config.ResponseSchema):
		dec.unsupported = "response schema"
	case present(config.ResponseJSONSchema):
		schema, ok := parseResponseSchema(config.ResponseJSONSchema)
		if !ok {
			dec.unsupported = "response schema"
			break
		}
		dec.schema = &schema
	}
	return dec, nil
}

// parseResponseSchema reads a top-level object schema whose properties are
// all strings or integers, the only schemas the auxiliary answer can satisfy.
func parseResponseSchema(raw json.RawMessage) (responseSchema, bool) {
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil || !strings.EqualFold(schema.Type, "object") {
		return responseSchema{}, false
	}
	properties := make(map[string]string, len(schema.Properties))
	for name, property := range schema.Properties {
		typ := strings.ToLower(property.Type)
		if typ != "string" && typ != "integer" {
			return responseSchema{}, false
		}
		properties[name] = typ
	}
	return responseSchema{properties: properties}, true
}

type generateContentChunk struct {
	Candidates    []generateContentCandidate `json:"candidates"`
	UsageMetadata generateContentUsage       `json:"usageMetadata"`
	ModelVersion  string                     `json:"modelVersion"`
}

type generateContentCandidate struct {
	Content      generateContentContent `json:"content"`
	FinishReason string                 `json:"finishReason"`
	Index        int                    `json:"index"`
}

type generateContentContent struct {
	Role  string                `json:"role"`
	Parts []generateContentPart `json:"parts"`
}

type generateContentPart struct {
	Text         string                       `json:"text,omitempty"`
	FunctionCall *generateContentFunctionCall `json:"functionCall,omitempty"`
}

type generateContentFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type generateContentUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount,omitempty"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
}

func (generateContentCodec) render(r reply) rendered {
	part := generateContentPart{Text: r.text}
	if r.call != nil {
		part = generateContentPart{FunctionCall: &generateContentFunctionCall{Name: r.call.Name, Args: r.call.Arguments}}
	}
	chunk := encodeJSON(generateContentChunk{
		Candidates: []generateContentCandidate{{
			Content:      generateContentContent{Role: "model", Parts: []generateContentPart{part}},
			FinishReason: "STOP",
		}},
		UsageMetadata: generateContentUsage{
			PromptTokenCount:        r.usage.Prompt,
			CandidatesTokenCount:    r.usage.Candidates,
			ThoughtsTokenCount:      r.usage.Thoughts,
			CachedContentTokenCount: r.usage.CachedContent,
			TotalTokenCount:         r.usage.Prompt + r.usage.Candidates + r.usage.Thoughts,
		},
		ModelVersion: r.model,
	})

	reported := r.usage
	reported.CacheWrite = 0
	if !r.streaming {
		return rendered{contentType: jsonContentType, body: chunk, usage: reported}
	}
	return rendered{
		contentType: "text/event-stream",
		body:        []byte("data: " + string(chunk) + "\r\n\r\n"),
		usage:       reported,
	}
}

type generateContentError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (generateContentCodec) errorBody(status int, message string) []byte {
	var envelope generateContentError
	envelope.Error.Code, envelope.Error.Message, envelope.Error.Status = status, message, "INVALID_ARGUMENT"
	return encodeJSON(envelope)
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func compactJSON(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}
