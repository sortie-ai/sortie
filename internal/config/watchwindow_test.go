package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMaxWatchWindowMSBoundary proves MaxWatchWindowMS is exactly the
// largest millisecond count whose conversion to a time.Duration stays
// positive. Both operands are bound to int64 variables before the
// multiplication so the wrap is computed at run time: writing
// time.Duration(MaxWatchWindowMS+1) * time.Millisecond inline is a
// constant expression whose value, 9223372036855000000, exceeds
// math.MaxInt64 and is rejected by the compiler rather than wrapped.
func TestMaxWatchWindowMSBoundary(t *testing.T) {
	t.Parallel()

	limit := MaxWatchWindowMS
	within := time.Duration(limit) * time.Millisecond
	if within <= 0 {
		t.Errorf("time.Duration(MaxWatchWindowMS) * time.Millisecond = %d, want > 0", within)
	}

	over := MaxWatchWindowMS + 1
	beyond := time.Duration(over) * time.Millisecond
	if beyond > 0 {
		t.Errorf("time.Duration(MaxWatchWindowMS + 1) * time.Millisecond = %d, want <= 0 (overflow)", beyond)
	}
}

func TestValidateWatchWindowMS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ms      int
		wantErr bool
		wantMsg string
	}{
		{name: "Zero", ms: 0},
		{name: "One", ms: 1},
		{name: "Ceiling", ms: int(MaxWatchWindowMS)},
		{
			name:    "AboveCeiling",
			ms:      int(MaxWatchWindowMS + 1),
			wantErr: true,
			wantMsg: fmt.Sprintf("must not exceed %d (about 292 years); use 0 for no time limit, got %d", MaxWatchWindowMS, int(MaxWatchWindowMS+1)),
		},
		{
			name:    "Negative",
			ms:      -1,
			wantErr: true,
			wantMsg: "must be non-negative, got -1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateWatchWindowMS(tt.ms)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateWatchWindowMS(%d) unexpected error: %v", tt.ms, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateWatchWindowMS(%d) = nil, want error", tt.ms)
			}
			if got := err.Error(); got != tt.wantMsg {
				t.Errorf("ValidateWatchWindowMS(%d) = %q, want %q", tt.ms, got, tt.wantMsg)
			}
			if strings.Contains(err.Error(), "watch_window_ms") {
				t.Errorf("ValidateWatchWindowMS(%d) error = %q, must not contain %q", tt.ms, err.Error(), "watch_window_ms")
			}
		})
	}
}
