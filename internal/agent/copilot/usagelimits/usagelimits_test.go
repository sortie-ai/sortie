package usagelimits_test

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/copilot/usagelimits"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestSessionErrorRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		data        string
		wantParse   bool
		wantMessage string
		wantRefusal bool
		wantQuota   bool
	}{
		{name: "quota reads its reset from the quota windows", data: `{"errorType":"quota"}`, wantParse: true, wantRefusal: true, wantQuota: true},
		{name: "rate_limit has no quota window", data: `{"errorType":"rate_limit"}`, wantParse: true, wantRefusal: true},
		{name: "query is not a refusal", data: `{"errorType":"query","message":"failed"}`, wantParse: true, wantMessage: "failed"},
		{name: "recognition is exact", data: `{"errorType":"Quota"}`, wantParse: true},
		{name: "non-string members read as empty", data: `{"errorType":402,"message":["x"]}`, wantParse: true},
		{name: "message is trimmed", data: `{"errorType":"quota","message":"  out \n"}`, wantParse: true, wantMessage: "out", wantRefusal: true, wantQuota: true},
		{name: "array data", data: `["quota"]`},
		{name: "null data", data: `null`},
		{name: "empty data", data: ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, err := usagelimits.ParseSessionError([]byte(tt.data))
			if (err == nil) != tt.wantParse {
				t.Fatalf("ParseSessionError(%q) error = %v, want success %v", tt.data, err, tt.wantParse)
			}
			refusal, ok := usagelimits.RefusalOf(e)

			if e.Message != tt.wantMessage || ok != tt.wantRefusal || refusal.QuotaWindow != tt.wantQuota {
				t.Errorf("ParseSessionError(%q) = message %q, refusal %v, quota window %v, want %q, %v, %v",
					tt.data, e.Message, ok, refusal.QuotaWindow, tt.wantMessage, tt.wantRefusal, tt.wantQuota)
			}
			if ok && refusal.Limit != (domain.UsageLimit{ModelOnly: true}) {
				t.Errorf("RefusalOf(%q).Limit = %+v, want model-only with no reset", tt.data, refusal.Limit)
			}
		})
	}
}
