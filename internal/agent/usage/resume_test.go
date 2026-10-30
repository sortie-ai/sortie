package usage

import (
	"maps"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestResumeBaseline_Observe_CounterReset(t *testing.T) {
	t.Parallel()

	usage := func(in, out, read, write int64) domain.TokenUsage {
		return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, CacheReadTokens: read, CacheWriteTokens: write}
	}
	start, own := usage(100, 10, 1000, 500), usage(10, 2, 100, 20)
	b := NewResumeBaseline(&start, nil)

	b.Observe(usage(130, 25, 1400, 600), nil, nil)

	run, _, estimated := b.Observe(usage(20, 5, 300, 50), nil, &own)
	if want := usage(40, 17, 500, 120); run != want || !estimated {
		t.Errorf("Observe(reset reading, own) = %+v, %v, want %+v, true", run, estimated, want)
	}

	run, _, estimated = b.Observe(usage(25, 9, 350, 70), nil, nil)
	if want := usage(45, 21, 550, 140); run != want || estimated {
		t.Errorf("Observe(reading after the reset) = %+v, %v, want %+v, false", run, estimated, want)
	}
}

func TestResumeBaseline_Observe_RederivedBaselineExactness(t *testing.T) {
	t.Parallel()

	reading := domain.TokenUsage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CacheReadTokens: 300}
	prior := domain.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 900}

	tests := []struct {
		name          string
		resume        *domain.TokenUsage
		own           *domain.TokenUsage
		wantEstimated bool
	}{
		{name: "unknown baseline, own covers the reading", own: &reading, wantEstimated: false},
		{name: "unknown baseline, no own", wantEstimated: true},
		{name: "counter reset, own covers the reading", resume: &prior, own: &reading, wantEstimated: false},
		{name: "counter reset, no own", resume: &prior, wantEstimated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := NewResumeBaseline(tt.resume, nil)

			_, _, estimated := b.Observe(reading, nil, tt.own)

			if estimated != tt.wantEstimated {
				t.Errorf("Observe(reading, %v) estimated = %v, want %v", tt.own, estimated, tt.wantEstimated)
			}
		})
	}
}

func TestResumeBaseline_Observe_PerModelSeed(t *testing.T) {
	t.Parallel()

	usage := func(in, out int64) domain.TokenUsage {
		return domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}
	}
	base := usage(10, 2)
	reading := map[string]domain.TokenUsage{"a": usage(15, 4), "b": usage(5, 1)}

	tests := []struct {
		name       string
		seed       map[string]domain.TokenUsage
		wantRun    map[string]domain.TokenUsage
		wantResume map[string]domain.TokenUsage
	}{
		{
			name:       "consistent seed is subtracted per model",
			seed:       map[string]domain.TokenUsage{"a": usage(8, 2), "b": usage(2, 0)},
			wantRun:    map[string]domain.TokenUsage{"a": usage(7, 2), "b": usage(3, 1)},
			wantResume: reading,
		},
		{
			name:       "seed that does not add up to the combined figure claims nothing",
			seed:       map[string]domain.TokenUsage{"a": usage(8, 2)},
			wantResume: reading,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := NewResumeBaseline(&base, tt.seed)

			_, run, _ := b.Observe(usage(20, 5), reading, nil)
			_, resume := b.Resume()

			if !maps.Equal(run, tt.wantRun) {
				t.Errorf("Observe(reading) by model = %+v, want %+v", run, tt.wantRun)
			}
			if !maps.Equal(resume, tt.wantResume) {
				t.Errorf("Resume() by model = %+v, want %+v", resume, tt.wantResume)
			}
		})
	}
}
