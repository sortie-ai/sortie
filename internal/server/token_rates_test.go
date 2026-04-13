package server

import (
	"math"
	"testing"
)

// --- ParseTokenRates ---

func TestParseTokenRates(t *testing.T) {
	t.Parallel()

	fptr := func(v float64) *float64 { return &v }

	tests := []struct {
		name         string
		extensions   map[string]any
		wantNil      bool
		wantWarnings int
		wantRates    TokenRates // only checked when wantNil is false
	}{
		{
			name:       "absent token_rates key returns nil",
			extensions: map[string]any{"other": "value"},
			wantNil:    true,
		},
		{
			name:       "nil extensions map returns nil",
			extensions: nil,
			wantNil:    true,
		},
		{
			name:       "empty token_rates map returns nil",
			extensions: map[string]any{"token_rates": map[string]any{}},
			wantNil:    true,
		},
		{
			name: "kind with empty map is skipped, no rates returned",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{},
				},
			},
			wantNil: true,
		},
		{
			name: "single kind all three rates present",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{
						"input_per_mtok":      3.0,
						"output_per_mtok":     15.0,
						"cache_read_per_mtok": 0.3,
					},
				},
			},
			wantNil: false,
			wantRates: TokenRates{
				"claude": TokenRateConfig{
					InputPerMtok:     fptr(3.0),
					OutputPerMtok:    fptr(15.0),
					CacheReadPerMtok: fptr(0.3),
				},
			},
		},
		{
			name: "multiple kinds all parsed",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{
						"input_per_mtok":  3.0,
						"output_per_mtok": 15.0,
					},
					"gpt4": map[string]any{
						"input_per_mtok":  10.0,
						"output_per_mtok": 30.0,
					},
				},
			},
			wantNil: false,
			wantRates: TokenRates{
				"claude": TokenRateConfig{InputPerMtok: fptr(3.0), OutputPerMtok: fptr(15.0)},
				"gpt4":   TokenRateConfig{InputPerMtok: fptr(10.0), OutputPerMtok: fptr(30.0)},
			},
		},
		{
			name: "non-map token_rates value yields warning",
			extensions: map[string]any{
				"token_rates": "not-a-map",
			},
			wantNil:      true,
			wantWarnings: 1,
		},
		{
			name: "non-map kind sub-value yields warning, partial result",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{"input_per_mtok": 3.0},
					"bad":    "not-a-map",
				},
			},
			wantNil:      false,
			wantWarnings: 1,
			wantRates: TokenRates{
				"claude": TokenRateConfig{InputPerMtok: fptr(3.0)},
			},
		},
		{
			name: "negative rate yields nil pointer for that field and warning",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{
						"input_per_mtok":  -1.0,
						"output_per_mtok": 15.0,
					},
				},
			},
			wantNil:      false,
			wantWarnings: 1,
			wantRates: TokenRates{
				"claude": TokenRateConfig{InputPerMtok: nil, OutputPerMtok: fptr(15.0)},
			},
		},
		{
			name: "empty kind alongside populated kind yields only populated kind",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"empty":  map[string]any{},
					"claude": map[string]any{"input_per_mtok": 3.0},
				},
			},
			wantNil: false,
			wantRates: TokenRates{
				"claude": TokenRateConfig{InputPerMtok: fptr(3.0)},
			},
		},
		{
			name: "kind with all negative rates is skipped",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"bad": map[string]any{
						"input_per_mtok":  -1.0,
						"output_per_mtok": -2.0,
					},
				},
			},
			wantNil:      true,
			wantWarnings: 2,
		},
		{
			name: "explicit zero rate produces non-nil pointer to 0.0",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{
						"input_per_mtok": 0.0,
					},
				},
			},
			wantNil: false,
			wantRates: TokenRates{
				"claude": TokenRateConfig{InputPerMtok: fptr(0.0)},
			},
		},
		{
			name: "integer values coerced to float64",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{
						"input_per_mtok":  int(3),
						"output_per_mtok": int64(15),
					},
				},
			},
			wantNil: false,
			wantRates: TokenRates{
				"claude": TokenRateConfig{
					InputPerMtok:  fptr(3.0),
					OutputPerMtok: fptr(15.0),
				},
			},
		},
		{
			name: "missing individual field yields nil pointer for that field",
			extensions: map[string]any{
				"token_rates": map[string]any{
					"claude": map[string]any{
						"output_per_mtok": 15.0,
						// input_per_mtok absent
						// cache_read_per_mtok absent
					},
				},
			},
			wantNil: false,
			wantRates: TokenRates{
				"claude": TokenRateConfig{
					InputPerMtok:     nil,
					OutputPerMtok:    fptr(15.0),
					CacheReadPerMtok: nil,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, warnings := ParseTokenRates(tt.extensions)

			if len(warnings) < tt.wantWarnings {
				t.Errorf("ParseTokenRates warnings = %d, want >= %d: %v", len(warnings), tt.wantWarnings, warnings)
			}

			if tt.wantNil {
				if got != nil {
					t.Errorf("ParseTokenRates = %v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Fatal("ParseTokenRates = nil, want non-nil")
			}

			for kind, wantCfg := range tt.wantRates {
				gotCfg, ok := got[kind]
				if !ok {
					t.Errorf("missing kind %q in result", kind)
					continue
				}
				assertRateField(t, kind, "input_per_mtok", gotCfg.InputPerMtok, wantCfg.InputPerMtok)
				assertRateField(t, kind, "output_per_mtok", gotCfg.OutputPerMtok, wantCfg.OutputPerMtok)
				assertRateField(t, kind, "cache_read_per_mtok", gotCfg.CacheReadPerMtok, wantCfg.CacheReadPerMtok)
			}
		})
	}
}

func assertRateField(t *testing.T, kind, field string, got, want *float64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("token_rates.%s.%s = %v, want nil", kind, field, *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("token_rates.%s.%s = nil, want %v", kind, field, *want)
	}
	if *got != *want {
		t.Errorf("token_rates.%s.%s = %v, want %v", kind, field, *got, *want)
	}
}

// --- estimateCost ---

func TestEstimateCost(t *testing.T) {
	t.Parallel()

	fptr := func(v float64) *float64 { return &v }

	tests := []struct {
		name       string
		input      int64
		output     int64
		cacheRead  int64
		rates      *TokenRateConfig
		wantNil    bool
		wantResult float64
	}{
		{
			name:      "nil rates returns nil",
			input:     100,
			output:    200,
			cacheRead: 50,
			rates:     nil,
			wantNil:   true,
		},
		{
			name:    "all rate fields nil on non-nil config returns nil",
			input:   1000,
			output:  500,
			rates:   &TokenRateConfig{},
			wantNil: true,
		},
		{
			name:       "all rates configured computes correct sum",
			input:      1_000_000,
			output:     500_000,
			cacheRead:  200_000,
			rates:      &TokenRateConfig{InputPerMtok: fptr(3.0), OutputPerMtok: fptr(15.0), CacheReadPerMtok: fptr(0.3)},
			wantResult: 3.0*1.0 + 15.0*0.5 + 0.3*0.2,
		},
		{
			name:       "only output rate configured",
			input:      1_000_000,
			output:     2_000_000,
			cacheRead:  500_000,
			rates:      &TokenRateConfig{OutputPerMtok: fptr(15.0)},
			wantResult: 15.0 * 2.0,
		},
		{
			name:       "only input rate configured",
			input:      2_000_000,
			output:     500_000,
			rates:      &TokenRateConfig{InputPerMtok: fptr(5.0)},
			wantResult: 5.0 * 2.0,
		},
		{
			name:       "zero token counts with rates returns pointer to 0.0",
			input:      0,
			output:     0,
			cacheRead:  0,
			rates:      &TokenRateConfig{InputPerMtok: fptr(3.0), OutputPerMtok: fptr(15.0)},
			wantResult: 0.0,
		},
		{
			name:       "zero rates with tokens returns pointer to 0.0",
			input:      1_000_000,
			output:     500_000,
			rates:      &TokenRateConfig{InputPerMtok: fptr(0.0), OutputPerMtok: fptr(0.0)},
			wantResult: 0.0,
		},
		{
			name:       "large token counts produce finite result",
			input:      1_000_000_000,
			output:     1_000_000_000,
			cacheRead:  1_000_000_000,
			rates:      &TokenRateConfig{InputPerMtok: fptr(3.0), OutputPerMtok: fptr(15.0), CacheReadPerMtok: fptr(0.3)},
			wantResult: 3000.0 + 15000.0 + 300.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := estimateCost(tt.input, tt.output, tt.cacheRead, tt.rates)

			if tt.wantNil {
				if got != nil {
					t.Errorf("estimateCost = %v, want nil", *got)
				}
				return
			}

			if got == nil {
				t.Fatal("estimateCost = nil, want non-nil")
			}
			if math.IsInf(*got, 0) || math.IsNaN(*got) {
				t.Fatalf("estimateCost = %v, want finite number", *got)
			}
			if diff := *got - tt.wantResult; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("estimateCost = %.10f, want %.10f", *got, tt.wantResult)
			}
		})
	}
}

// --- fmtCost ---

func TestFmtCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input float64
		want  string
	}{
		{"zero", 0.00, "$0.00"},
		{"small", 0.05, "$0.05"},
		{"under 10", 9.99, "$9.99"},
		{"under 1000", 999.99, "$999.99"},
		{"rounds up to 1000 from below", 999.999, "$1,000.00"},
		{"rounds up fractional near boundary", 1234.9999, "$1,235.00"},
		{"exact 1000", 1000.00, "$1,000.00"},
		{"mid four digits", 1234.56, "$1,234.56"},
		{"five digits with cents", 10000.50, "$10,000.50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := fmtCost(tt.input)
			if got != tt.want {
				t.Errorf("fmtCost(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
