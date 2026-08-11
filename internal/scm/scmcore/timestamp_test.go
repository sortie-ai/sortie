package scmcore

import (
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestParseTimestampOrZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			name:  "fractional second with positive zone offset",
			input: "2026-08-11T01:53:22.509+02:00",
			want:  time.Date(2026, time.August, 10, 23, 53, 22, 509000000, time.UTC),
		},
		{
			name:  "fractional second in zulu form",
			input: "2026-01-10T09:00:00.000Z",
			want:  time.Date(2026, time.January, 10, 9, 0, 0, 0, time.UTC),
		},
		{
			name:  "whole second in zulu form",
			input: "2026-08-10T08:00:00Z",
			want:  time.Date(2026, time.August, 10, 8, 0, 0, 0, time.UTC),
		},
		{
			name:  "malformed timestamp yields the zero time",
			input: "not-a-timestamp",
			want:  time.Time{},
		},
		{
			name:  "empty timestamp yields the zero time",
			input: "",
			want:  time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParseTimestampOrZero(tt.input)
			if !got.Equal(tt.want) {
				t.Errorf("ParseTimestampOrZero(%q) = %v, want %v", tt.input, got, tt.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("ParseTimestampOrZero(%q): location = %v, want UTC", tt.input, got.Location())
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	t.Parallel()

	t.Run("a valid value returns the UTC instant with a nil error interface", func(t *testing.T) {
		t.Parallel()

		got, err := ParseTimestamp("test field", "2026-08-10T08:00:00Z")
		if err != nil {
			t.Fatalf("ParseTimestamp() err = %v, want nil (a typed-nil return would fail this comparison)", err)
		}

		want := time.Date(2026, time.August, 10, 8, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("ParseTimestamp() = %v, want %v", got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("ParseTimestamp(): location = %v, want UTC", got.Location())
		}
	})

	t.Run("a malformed value returns a payload error naming the field", func(t *testing.T) {
		t.Parallel()

		got, err := ParseTimestamp("review submitted_at", "not-a-timestamp")
		if !got.IsZero() {
			t.Errorf("ParseTimestamp() = %v, want the zero time on failure", got)
		}

		var scmErr *domain.SCMError
		if !errors.As(err, &scmErr) {
			t.Fatalf("ParseTimestamp() error = %v, want *domain.SCMError", err)
		}
		if scmErr.Kind != domain.ErrSCMPayload {
			t.Errorf("ParseTimestamp() error Kind = %q, want %q", scmErr.Kind, domain.ErrSCMPayload)
		}
		wantMessage := "failed to parse review submitted_at"
		if scmErr.Message != wantMessage {
			t.Errorf("ParseTimestamp() error Message = %q, want %q", scmErr.Message, wantMessage)
		}
		if scmErr.Err == nil {
			t.Error("ParseTimestamp() error Err = nil, want the wrapped *time.ParseError")
		}
	})

	t.Run("an empty value returns a payload error", func(t *testing.T) {
		t.Parallel()

		_, err := ParseTimestamp("issue event created_at", "")

		var scmErr *domain.SCMError
		if !errors.As(err, &scmErr) {
			t.Fatalf("ParseTimestamp() error = %v, want *domain.SCMError", err)
		}
		if scmErr.Kind != domain.ErrSCMPayload {
			t.Errorf("ParseTimestamp() error Kind = %q, want %q", scmErr.Kind, domain.ErrSCMPayload)
		}
	})
}
