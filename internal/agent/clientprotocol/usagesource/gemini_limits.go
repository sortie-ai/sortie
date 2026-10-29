package usagesource

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	geminiEndpointVar     = "CODE_ASSIST_ENDPOINT"
	geminiDefaultEndpoint = "https://cloudcode-pa.googleapis.com"
	geminiAPIVersion      = "v1internal"
	geminiResponseLimit   = 1 << 20
)

// geminiProjectVars are the variables the runtime reads its project from, in
// its own order.
var geminiProjectVars = []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_PROJECT_ID"}

func init() {
	Declarations.Register(geminiAgentName, Declaration{
		Live: usage.Source{Category: usage.SourceProtocol},
		Pull: usage.Source{
			Strategy: usage.StrategyPull,
			Windows:  usage.WindowDeclaration{Share: usage.ShareFraction, ShareRemaining: true},
		},
		PullWindows: geminiPullWindows,
		Launches: func(target agentcore.LaunchTarget) bool {
			return commandNamesRuntime(target, geminiExecutableName, geminiPackageName)
		},
	})
}

// geminiClientMetadata is what the runtime sends with loadCodeAssist when it
// has no project.
type geminiClientMetadata struct {
	IDEType    string `json:"ideType"`
	Platform   string `json:"platform"`
	PluginType string `json:"pluginType"`
}

type geminiLoadRequest struct {
	Metadata geminiClientMetadata `json:"metadata"`
}

type geminiLoadResponse struct {
	CloudAICompanionProject string `json:"cloudaicompanionProject"`
}

type geminiQuotaRequest struct {
	Project string `json:"project"`
}

type geminiQuotaResponse struct {
	Buckets []geminiBucket `json:"buckets"`
}

type geminiBucket struct {
	ModelID           string   `json:"modelId"`
	TokenType         string   `json:"tokenType"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

// geminiPullWindows fetches the per-model quota from the endpoint the runtime
// itself calls. It returns nil, nil without a request once the stored token has
// expired: only the runtime refreshes it.
func geminiPullWindows(ctx context.Context, rt PullRuntime) ([]usage.WindowReading, error) {
	token, err := geminiAccessToken(ctx, rt)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, nil
	}

	endpoint, _ := rt.LaunchEnv(geminiEndpointVar)
	baseURL := cmp.Or(endpoint, geminiDefaultEndpoint) + "/" + geminiAPIVersion

	project := geminiLaunchProject(rt)
	if project == "" {
		var loaded geminiLoadResponse
		request := geminiLoadRequest{Metadata: geminiClientMetadata{
			IDEType:    "IDE_UNSPECIFIED",
			Platform:   "PLATFORM_UNSPECIFIED",
			PluginType: "GEMINI",
		}}
		if err := geminiPost(ctx, baseURL+":loadCodeAssist", token, request, &loaded); err != nil {
			return nil, err
		}
		project = loaded.CloudAICompanionProject
		if project == "" {
			return nil, errors.New("loadCodeAssist named no project")
		}
	}

	var quota geminiQuotaResponse
	if err := geminiPost(ctx, baseURL+":retrieveUserQuota", token, geminiQuotaRequest{Project: project}, &quota); err != nil {
		return nil, err
	}
	return geminiBucketReadings(quota.Buckets), nil
}

func geminiLaunchProject(rt PullRuntime) string {
	for _, name := range geminiProjectVars {
		if value, _ := rt.LaunchEnv(name); value != "" {
			return value
		}
	}
	return ""
}

// geminiPost sends one JSON request and decodes the answer into out. A failed
// status is reported by its code alone: neither the body nor a header can
// reach an error, and the token is sent only here.
func geminiPost(ctx context.Context, url, token string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode gemini request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build gemini request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("gemini request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("gemini request: status %d: %w", resp.StatusCode, usage.ErrPullUnsupported)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("gemini request: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, geminiResponseLimit)).Decode(out); err != nil {
		return fmt.Errorf("decode gemini response: %w", err)
	}
	return nil
}

// geminiBucketReadings skips a bucket the runtime itself skips: one without a
// model or a remaining fraction.
func geminiBucketReadings(buckets []geminiBucket) []usage.WindowReading {
	var readings []usage.WindowReading
	for _, bucket := range buckets {
		if bucket.ModelID == "" || bucket.RemainingFraction == nil {
			continue
		}
		window := bucket.ModelID
		if bucket.TokenType != "" {
			window += "/" + bucket.TokenType
		}
		readings = append(readings, usage.WindowReading{
			Window:   window,
			Span:     usage.Span{Scope: bucket.ModelID},
			Share:    bucket.RemainingFraction,
			ResetsAt: geminiResetUnix(bucket.ResetTime),
		})
	}
	return readings
}

func geminiResetUnix(resetTime string) int64 {
	reset, err := time.Parse(time.RFC3339, resetTime)
	if err != nil {
		return 0
	}
	return reset.Unix()
}
