package quota

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	headlessConnectID = 1
	headlessQuotaID   = 2
)

var headlessArgs = []string{"--headless", "--no-auto-update", "--stdio"}

type headlessRequest struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      int      `json:"id"`
	Method  string   `json:"method"`
	Params  struct{} `json:"params"`
}

type headlessMessage struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct{}       `json:"error"`
}

type headlessQuotaAnswer struct {
	QuotaSnapshots map[string]headlessQuotaSnapshot `json:"quotaSnapshots"`
}

type headlessQuotaSnapshot struct {
	IsUnlimitedEntitlement bool     `json:"isUnlimitedEntitlement"`
	RemainingPercentage    *float64 `json:"remainingPercentage"`
	ResetDate              string   `json:"resetDate"`
}

// askHeadless asks a one-shot copilot server for the account's quota
// windows over JSON-RPC on stdio, with the runtime's own sign-in. It returns
// one reading per limited quota; an error answer from the server wraps
// [usage.ErrPullUnsupported].
func askHeadless(ctx context.Context, rt Runtime) ([]usage.WindowReading, error) {
	var readings []usage.WindowReading
	err := rt.Exchange(ctx, headlessArgs, MaxAnswerBytes, func(stdin io.Writer, stdout io.Reader) error {
		var exchangeErr error
		readings, exchangeErr = headlessExchange(stdin, stdout)
		return exchangeErr
	})
	if err != nil {
		return nil, fmt.Errorf("query copilot quota: %w", err)
	}
	return readings, nil
}

func headlessExchange(stdin io.Writer, stdout io.Reader) ([]usage.WindowReading, error) {
	br := bufio.NewReader(stdout)
	if _, err := headlessCall(stdin, br, headlessConnectID, "connect"); err != nil {
		return nil, err
	}
	result, err := headlessCall(stdin, br, headlessQuotaID, "account.getQuota")
	if err != nil {
		return nil, err
	}
	return parseHeadlessQuota(result)
}

// headlessCall sends one request with empty params and returns the result
// of the response carrying id. Messages that are not that response, such
// as notifications, are skipped.
func headlessCall(stdin io.Writer, stdout *bufio.Reader, id int, method string) (json.RawMessage, error) {
	payload, err := json.Marshal(headlessRequest{JSONRPC: "2.0", ID: id, Method: method})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	if err := jsonrpc.WriteContentLengthFrame(stdin, payload); err != nil {
		return nil, fmt.Errorf("send %s request: %w", method, err)
	}

	for {
		body, err := jsonrpc.ReadContentLengthFrame(stdout, MaxAnswerBytes)
		if err != nil {
			return nil, fmt.Errorf("read %s answer: %w", method, err)
		}

		var msg headlessMessage
		if json.Unmarshal(body, &msg) != nil || msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s answered with an error: %w", method, usage.ErrPullUnsupported)
		}
		return msg.Result, nil
	}
}

func parseHeadlessQuota(result json.RawMessage) ([]usage.WindowReading, error) {
	var answer headlessQuotaAnswer
	if err := json.Unmarshal(result, &answer); err != nil {
		return nil, fmt.Errorf("decode account.getQuota answer: %w", err)
	}

	var readings []usage.WindowReading
	for _, key := range slices.Sorted(maps.Keys(answer.QuotaSnapshots)) {
		snapshot := answer.QuotaSnapshots[key]
		if snapshot.IsUnlimitedEntitlement {
			continue
		}
		readings = append(readings, usage.WindowReading{
			Window:     key,
			Share:      snapshot.RemainingPercentage,
			ObservedAt: headlessObservedAt(snapshot.ResetDate),
		})
	}
	return readings, nil
}

// headlessObservedAt reads resetDate as the observation time: the server
// answers from a cached snapshot and stamps it there, so the field is not
// a reset instant.
func headlessObservedAt(resetDate string) int64 {
	instant, err := time.Parse(time.RFC3339, resetDate)
	if err != nil {
		return 0
	}
	return instant.Unix()
}
