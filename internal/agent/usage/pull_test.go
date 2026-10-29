package usage_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

func TestPuller_Pull(t *testing.T) {
	t.Parallel()

	const bound = 5 * time.Second
	source := usage.Source{Strategy: usage.StrategyPull, Windows: usage.WindowDeclaration{Share: usage.SharePercent}}
	reading := []usage.WindowReading{{Window: "w", Share: new(float64(10))}}
	counting := func(results ...error) (*usage.Puller, *int) {
		calls := new(int)
		return usage.NewPuller(source, bound, false, func(context.Context) ([]usage.WindowReading, error) {
			*calls++
			return reading, results[min(*calls, len(results))-1]
		}), calls
	}

	t.Run("unsupported error stops later pulls", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			p, calls := counting(fmt.Errorf("probe: %w", usage.ErrPullUnsupported))

			_, firstErr := p.Pull(t.Context(), usage.EndingAnswered)
			got, secondErr := p.Pull(t.Context(), usage.EndingAnswered)

			if !errors.Is(firstErr, usage.ErrPullUnsupported) || got != nil || secondErr != nil || *calls != 1 {
				t.Errorf("Pull() twice = %v, %v after error %v with %d calls, want nil, nil after ErrPullUnsupported with 1", got, secondErr, firstErr, *calls)
			}
		})
	})

	t.Run("other error leaves the next pull running", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			p, calls := counting(errors.New("transient"), nil)

			_, firstErr := p.Pull(t.Context(), usage.EndingAnswered)
			got, secondErr := p.Pull(t.Context(), usage.EndingAnswered)

			if firstErr == nil || secondErr != nil || len(got) != 1 || *calls != 2 {
				t.Errorf("Pull() twice = %v, %v after error %v with %d calls, want 1 window, nil, an error, 2 calls", got, secondErr, firstErr, *calls)
			}
		})
	})

	t.Run("blocked pull returns no window at the bound", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			p := usage.NewPuller(source, bound, false, func(ctx context.Context) ([]usage.WindowReading, error) {
				<-ctx.Done()
				return reading, ctx.Err()
			})
			start := time.Now()

			got, err := p.Pull(t.Context(), usage.EndingAnswered)

			if got != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != bound {
				t.Errorf("Pull() = %v, %v after %v, want nil, DeadlineExceeded after %v", got, err, time.Since(start), bound)
			}
		})
	})

	t.Run("done context calls nothing", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			p, calls := counting(nil)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			got, err := p.Pull(ctx, usage.EndingAnswered)

			if got != nil || err != nil || *calls != 0 {
				t.Errorf("Pull(done context) = %v, %v with %d calls, want nil, nil with 0", got, err, *calls)
			}
		})
	})

	t.Run("credential verification has no puller", func(t *testing.T) {
		t.Parallel()

		p := usage.NewPuller(source, bound, true, nil)

		got, err := p.Pull(t.Context(), usage.EndingAnswered)

		if p != nil || p.PullsAfter(usage.EndingAnswered) || got != nil || err != nil {
			t.Errorf("NewPuller(credentialVerification) = %v, Pull() = %v, %v, want nil, nil, nil", p, got, err)
		}
	})

	for ending, pulls := range map[usage.TurnEnding]bool{
		usage.EndingAnswered:       true,
		usage.EndingTimedOut:       false,
		usage.EndingReadFailed:     false,
		usage.EndingCancelled:      false,
		usage.EndingHumanInput:     false,
		usage.EndingProtocolBroken: false,
		0:                          false,
	} {
		t.Run(fmt.Sprintf("ending %v", ending), func(t *testing.T) {
			t.Parallel()
			p, calls := counting(nil)

			_, err := p.Pull(t.Context(), ending)

			if err != nil || p.PullsAfter(ending) != pulls || (*calls == 1) != pulls {
				t.Errorf("Pull(%v) = %v with %d calls, PullsAfter = %t, want pulled %t", ending, err, *calls, p.PullsAfter(ending), pulls)
			}
		})
	}
}
