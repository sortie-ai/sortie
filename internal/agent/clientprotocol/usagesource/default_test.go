package usagesource

import (
	"encoding/json"
	"testing"
)

func usageUpdateWith(cost string) string {
	return `{"sessionUpdate":"usage_update","used":12000,"size":200000` + cost + `}`
}

func TestDefaultSessionUpdateAmounts(t *testing.T) {
	t.Parallel()

	if Default.SessionUpdateAmounts == nil {
		t.Fatal("Default.SessionUpdateAmounts = nil, want a reader of the standard cost member")
	}
	one := 1.0
	tests := []struct {
		name       string
		update     string
		wantUnit   string
		wantAmount *float64
	}{
		{"an amount written 1.0", usageUpdateWith(`,"cost":{"amount":1.0,"currency":"USD"}`), "USD", &one},
		{"a lowercase currency", usageUpdateWith(`,"cost":{"amount":1,"currency":"eur"}`), "EUR", &one},
		{"a four-letter currency", usageUpdateWith(`,"cost":{"amount":1,"currency":"USDT"}`), "", nil},
		{"a null amount", usageUpdateWith(`,"cost":{"amount":null,"currency":"USD"}`), "USD", nil},
		{"a negative amount", usageUpdateWith(`,"cost":{"amount":-1,"currency":"USD"}`), "USD", nil},
		{"no cost member", usageUpdateWith(""), "", nil},
		{"another variant carrying a cost", `{"sessionUpdate":"plan","entries":[],"cost":{"amount":1,"currency":"USD"}}`, "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Default.SessionUpdateAmounts(json.RawMessage(tt.update))

			if tt.wantUnit == "" {
				if len(got) != 0 {
					t.Fatalf("SessionUpdateAmounts(%s) = %+v, want no reading", tt.update, got)
				}
				return
			}
			if len(got) != 1 || got[0].Unit != tt.wantUnit {
				t.Fatalf("SessionUpdateAmounts(%s) = %+v, want one reading in %q", tt.update, got, tt.wantUnit)
			}
			if (got[0].Amount == nil) != (tt.wantAmount == nil) || (tt.wantAmount != nil && *got[0].Amount != *tt.wantAmount) {
				t.Errorf("SessionUpdateAmounts(%s) amount = %v, want %v", tt.update, got[0].Amount, tt.wantAmount)
			}
		})
	}
}
