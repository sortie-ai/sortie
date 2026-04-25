package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "TrackerError with ErrTrackerNotFound",
			err:  &TrackerError{Kind: ErrTrackerNotFound, Message: "issue not found"},
			want: true,
		},
		{
			name: "TrackerError with ErrTrackerAPI",
			err:  &TrackerError{Kind: ErrTrackerAPI, Message: "server error"},
			want: false,
		},
		{
			name: "TrackerError with ErrTrackerTransport",
			err:  &TrackerError{Kind: ErrTrackerTransport, Message: "connection refused"},
			want: false,
		},
		{
			name: "wrapped TrackerError with ErrTrackerNotFound",
			err:  fmt.Errorf("wrapped: %w", &TrackerError{Kind: ErrTrackerNotFound, Message: "not found"}),
			want: true,
		},
		{
			name: "doubly wrapped TrackerError with ErrTrackerNotFound",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &TrackerError{Kind: ErrTrackerNotFound})),
			want: true,
		},
		{
			name: "unrelated error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "wrapped unrelated error",
			err:  fmt.Errorf("context: %w", errors.New("unrelated")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsNotFound(tt.err)
			if got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
