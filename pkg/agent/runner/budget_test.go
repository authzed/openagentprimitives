package runner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func cfg(turns int32, tokens int64, dur time.Duration) spiceboxv1alpha1.BudgetConfig {
	return spiceboxv1alpha1.BudgetConfig{
		MaxTurns:    turns,
		MaxTokens:   tokens,
		MaxDuration: metav1.Duration{Duration: dur},
	}
}

// runClockElapsed returns a RunClock frozen at a fixed elapsed run-time, so a
// budget test can drive the duration dimension deterministically.
func runClockElapsed(d time.Duration) *runner.RunClock {
	base := time.Unix(0, 0)
	now := base.Add(d)
	return runner.NewRunClock(d, func() time.Time { return now }) // seed=d, no further advance ⇒ Elapsed=d
}

func TestBudget_Check(t *testing.T) {
	type usage struct {
		turns int32
		in    int64
		out   int64
	}
	cases := []struct {
		name         string
		cfg          spiceboxv1alpha1.BudgetConfig
		runElapsed   time.Duration
		sessionStart time.Time
		usage        usage
		want         string
	}{
		{
			name:       "within all limits: empty reason",
			cfg:        cfg(50, 1000, time.Hour),
			runElapsed: 0,
			usage:      usage{0, 0, 0},
			want:       "",
		},
		{
			name:       "turns exceeded",
			cfg:        cfg(2, 100000, time.Hour),
			runElapsed: 0,
			usage:      usage{2, 0, 0},
			want:       runner.BudgetReasonTurns,
		},
		{
			name:       "tokens exceeded",
			cfg:        cfg(50, 100, time.Hour),
			runElapsed: 0,
			usage:      usage{0, 60, 50},
			want:       runner.BudgetReasonTokens,
		},
		{
			name:       "run-time exceeded (active run-time, not wall-clock)",
			cfg:        cfg(50, 100000, time.Hour),
			runElapsed: 2 * time.Hour,
			usage:      usage{0, 0, 0},
			want:       runner.BudgetReasonDuration,
		},
		{
			name:         "idle does not exceed maxDuration: huge sessionStart age, tiny run-time",
			cfg:          cfg(50, 100000, time.Hour),
			runElapsed:   30 * time.Second,
			sessionStart: time.Now().Add(-10 * time.Hour), // idle for hours...
			usage:        usage{0, 0, 0},
			want:         "", // ...but only 30s of run-time, and no sessionExpiration set
		},
		{
			name: "sessionExpiration exceeded: wall-clock lifetime cap fires",
			cfg: spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				SessionExpiration: metav1.Duration{Duration: time.Hour},
			},
			runElapsed:   30 * time.Second,               // barely ran...
			sessionStart: time.Now().Add(-2 * time.Hour), // ...but 2h of wall-clock lifetime
			usage:        usage{0, 0, 0},
			want:         runner.BudgetReasonExpired,
		},
		{
			name: "sessionExpiration ignored when sessionStart is zero",
			cfg: spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				SessionExpiration: metav1.Duration{Duration: time.Hour},
			},
			runElapsed:   0,
			sessionStart: time.Time{}, // not started yet
			usage:        usage{0, 0, 0},
			want:         "",
		},
		{
			name:       "zero caps are unlimited",
			cfg:        cfg(0, 0, 0),
			runElapsed: 1000 * time.Hour,
			usage:      usage{1_000_000, 1_000_000_000, 1_000_000_000},
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := runner.NewBudget(tc.cfg, runClockElapsed(tc.runElapsed), tc.sessionStart)
			assert.Equal(t, tc.want, b.Check(tc.usage.turns, tc.usage.in, tc.usage.out))
		})
	}
}
