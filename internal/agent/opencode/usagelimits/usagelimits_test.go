package usagelimits_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/opencode/usagelimits"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestPull(t *testing.T) {
	t.Parallel()

	signed, err := os.ReadFile("testdata/api-limits-signed.json")
	if err != nil {
		t.Fatalf("read signed-in fixture: %v", err)
	}
	signedIn := []domain.UsageWindow{
		{Key: "rolling", Label: "Rolling limit", UsedPercent: new(37.5), ResetsAt: time.UnixMilli(1793199600000).UTC()},
		{Key: "weekly", Label: "Weekly limit", Minutes: 10080, UsedPercent: new(12.0), ResetsAt: time.UnixMilli(1793577600000).UTC()},
		{Key: "monthly", Label: "Monthly limit", UsedPercent: new(100.0), Status: domain.UsageWindowExhausted, ResetsAt: time.UnixMilli(1794700800000).UTC()},
	}
	tests := []struct {
		name    string
		out     string
		runErr  error
		wantErr error
	}{
		{name: "signed in", out: string(signed)},
		{name: "no console sign-in", out: `{"data":null}`, wantErr: usage.ErrPullUnsupported},
		{name: "no go member", out: `{"data":{}}`, wantErr: usage.ErrPullUnsupported},
		{name: "not an object", out: "unknown command\n", wantErr: usage.ErrPullUnsupported},
		{name: "run fails", runErr: errors.New("exit status 1"), wantErr: usage.ErrPullUnsupported},
		{name: "connection failure", runErr: sshutil.ErrConnectionFailed, wantErr: sshutil.ErrConnectionFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			readings, err := usagelimits.Pull(t.Context(), func(context.Context, []string) ([]byte, error) { return []byte(tt.out), tt.runErr })

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || errors.Is(err, usage.ErrPullUnsupported) != (tt.wantErr == usage.ErrPullUnsupported) {
					t.Errorf("Pull() error = %v, want it to wrap %v (and ErrPullUnsupported only if that is the sentinel)", err, tt.wantErr)
				}
				return
			}
			got := usage.Windows(usagelimits.Source, readings, time.Time{})
			if err != nil || !reflect.DeepEqual(got, signedIn) {
				t.Errorf("Pull() = %+v, %v, want %+v, nil", got, err, signedIn)
			}
		})
	}
}

func TestRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		wantRefusal bool
		wantMetered bool
	}{
		{"provider.free-limit", true, true},
		{"provider.budget-limit", true, true},
		{"provider.model-budget-limit", true, true},
		{"provider.quota", true, false},
		{"provider.rate-limit", false, false},
		{"Provider.Free-Limit", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			limit, ok := usagelimits.Refusal(tt.name)
			metered := usagelimits.ConsoleMetered(tt.name)

			if ok != tt.wantRefusal || limit.ModelOnly != tt.wantRefusal || metered != tt.wantMetered {
				t.Errorf("Refusal(%q) = %+v, %v, ConsoleMetered = %v, want refusal and ModelOnly %v, ConsoleMetered %v", tt.name, limit, ok, metered, tt.wantRefusal, tt.wantMetered)
			}
		})
	}
}

func TestRetryAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		failedAtMS int64
		delayMS    *int64
		want       time.Time
	}{
		{"delay after the failure", 1793288718816, new(int64(3600000)), time.UnixMilli(1793292318816).UTC()},
		{"no failure time", 0, new(int64(3600000)), time.Time{}},
		{"no delay", 1793288718816, nil, time.Time{}},
		{"zero delay", 1793288718816, new(int64(0)), time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := usagelimits.RetryAt(tt.failedAtMS, tt.delayMS)

			if !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Errorf("RetryAt(%s) = %v, want %v (UTC)", tt.name, got, tt.want)
			}
		})
	}
}
