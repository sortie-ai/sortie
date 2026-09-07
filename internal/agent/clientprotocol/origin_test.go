package clientprotocol

import (
	"testing"
	"time"
)

// TestSessionOriginsRecordStoresFreshEntry confirms that record makes
// createdAt report the identifier just recorded, at the instant the
// ledger's own clock returned.
func TestSessionOriginsRecordStoresFreshEntry(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	o := &sessionOrigins{now: func() time.Time { return at }}

	o.record("workspace", "sess-1")

	got, ok := o.createdAt("workspace", "sess-1")
	if !ok {
		t.Fatalf("createdAt(%q, %q) ok = false, want true immediately after record", "workspace", "sess-1")
	}
	if !got.Equal(at) {
		t.Errorf("createdAt(%q, %q) = %v, want %v", "workspace", "sess-1", got, at)
	}
}

// TestSessionOriginsRecordIgnoresEmptySessionID confirms that record
// does nothing for an empty identifier, since resolveSession never
// loads one.
func TestSessionOriginsRecordIgnoresEmptySessionID(t *testing.T) {
	t.Parallel()

	o := &sessionOrigins{now: func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }}

	o.record("workspace", "")

	if _, ok := o.createdAt("workspace", ""); ok {
		t.Errorf("createdAt(%q, %q) ok = true, want false: record must ignore an empty session id", "workspace", "")
	}
}

// TestSessionOriginsRecordEvictsEntriesOlderThanRetention confirms
// that a record call evicts every entry whose recorded instant is
// older than originRetention, while leaving the entry it is itself
// writing in place.
func TestSessionOriginsRecordEvictsEntriesOlderThanRetention(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	var clock time.Time
	o := &sessionOrigins{now: func() time.Time { return clock }}

	clock = base
	o.record("workspace", "old-session")

	if _, ok := o.createdAt("workspace", "old-session"); !ok {
		t.Fatalf("createdAt(%q, %q) ok = false, want true right after record", "workspace", "old-session")
	}

	clock = base.Add(originRetention + time.Millisecond)
	o.record("workspace", "new-session")

	if _, ok := o.createdAt("workspace", "old-session"); ok {
		t.Errorf("createdAt(%q, %q) ok = true, want false: an entry older than originRetention must be evicted on the next record call", "workspace", "old-session")
	}
	got, ok := o.createdAt("workspace", "new-session")
	if !ok {
		t.Fatalf("createdAt(%q, %q) ok = false, want true", "workspace", "new-session")
	}
	if !got.Equal(clock) {
		t.Errorf("createdAt(%q, %q) = %v, want %v", "workspace", "new-session", got, clock)
	}
}

// TestLoadDeferral covers loadDeferral's full output range: zero
// across a minute boundary, a bounded positive wait within one
// minute, the closed upper bound at exactly one minute, and the
// one-minute clamp for a now that precedes createdAt.
func TestLoadDeferral(t *testing.T) {
	t.Parallel()

	minuteStart := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)

	tests := []struct {
		name      string
		createdAt time.Time
		now       time.Time
		want      time.Duration
	}{
		{
			name:      "same minute yields the remaining time to the boundary",
			createdAt: minuteStart.Add(45 * time.Second),
			now:       minuteStart.Add(50 * time.Second),
			want:      10 * time.Second,
		},
		{
			name:      "now already in a later minute yields zero",
			createdAt: minuteStart.Add(45 * time.Second),
			now:       minuteStart.Add(90 * time.Second),
			want:      0,
		},
		{
			name:      "now exactly at the boundary yields zero",
			createdAt: minuteStart.Add(45 * time.Second),
			now:       minuteStart.Add(time.Minute),
			want:      0,
		},
		{
			name:      "createdAt on the minute boundary itself yields the full minute",
			createdAt: minuteStart,
			now:       minuteStart,
			want:      time.Minute,
		},
		{
			name:      "a backward clock step larger than a minute clamps to one minute",
			createdAt: minuteStart.Add(45 * time.Second),
			now:       minuteStart.Add(-10 * time.Minute),
			want:      time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := loadDeferral(tt.createdAt, tt.now)

			if got != tt.want {
				t.Errorf("loadDeferral(%v, %v) = %v, want %v", tt.createdAt, tt.now, got, tt.want)
			}
			if got < 0 || got > time.Minute {
				t.Errorf("loadDeferral(%v, %v) = %v, want a value in [0, time.Minute]", tt.createdAt, tt.now, got)
			}
		})
	}
}
