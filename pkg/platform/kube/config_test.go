package kube

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestWithRateLimitDefaults(t *testing.T) {
	cases := []struct {
		name      string
		in        rest.Config
		wantQPS   float32
		wantBurst int
	}{
		{name: "unset (in-cluster config): raised to the server defaults", in: rest.Config{}, wantQPS: DefaultQPS, wantBurst: DefaultBurst},
		{name: "both set: kept", in: rest.Config{QPS: 20, Burst: 30}, wantQPS: 20, wantBurst: 30},
		{name: "only QPS set: Burst defaulted, QPS kept", in: rest.Config{QPS: 7}, wantQPS: 7, wantBurst: DefaultBurst},
		{name: "only Burst set: QPS defaulted, Burst kept", in: rest.Config{Burst: 10}, wantQPS: DefaultQPS, wantBurst: 10},
		{name: "limiting disabled (GetConfig's QPS -1): left disabled, Burst not defaulted", in: rest.Config{QPS: -1}, wantQPS: -1, wantBurst: 0},
		{name: "limiting disabled with a Burst set: both kept", in: rest.Config{QPS: -1, Burst: 30}, wantQPS: -1, wantBurst: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.in
			withRateLimitDefaults(&cfg)
			assert.Equal(t, tc.wantQPS, cfg.QPS)
			assert.Equal(t, tc.wantBurst, cfg.Burst)
		})
	}
}

// stubConfigSources swaps the two package-level config sources for the test's
// duration. Tests using it must not run in parallel with each other.
func stubConfigSources(t *testing.T, inCluster, fallback func() (*rest.Config, error)) {
	t.Helper()
	origInCluster, origFallback := inClusterConfig, fallbackConfig
	t.Cleanup(func() { inClusterConfig, fallbackConfig = origInCluster, origFallback })
	inClusterConfig, fallbackConfig = inCluster, fallback
}

// TestRestConfigAppliesRateLimitDefaultsOnBothBranches pins that neither
// branch of RestConfig returns a config that skipped withRateLimitDefaults.
func TestRestConfigAppliesRateLimitDefaultsOnBothBranches(t *testing.T) {
	errNotInCluster := errors.New("not in cluster")
	errNoKubeconfig := errors.New("no kubeconfig")
	unreachable := func() (*rest.Config, error) {
		return nil, errors.New("fallback must not be consulted when in-cluster succeeds")
	}

	cases := []struct {
		name      string
		inCluster func() (*rest.Config, error)
		fallback  func() (*rest.Config, error)
		wantErr   error
		wantQPS   float32
		wantBurst int
	}{
		{
			name:      "in-cluster config with limits unset: raised to the server defaults",
			inCluster: func() (*rest.Config, error) { return &rest.Config{}, nil },
			fallback:  unreachable,
			wantQPS:   DefaultQPS,
			wantBurst: DefaultBurst,
		},
		{
			name:      "not in cluster, fallback with limiting disabled: left disabled",
			inCluster: func() (*rest.Config, error) { return nil, errNotInCluster },
			fallback:  func() (*rest.Config, error) { return &rest.Config{QPS: -1}, nil },
			wantQPS:   -1,
			wantBurst: 0,
		},
		{
			name:      "not in cluster, fallback with limits unset: raised to the server defaults",
			inCluster: func() (*rest.Config, error) { return nil, errNotInCluster },
			fallback:  func() (*rest.Config, error) { return &rest.Config{}, nil },
			wantQPS:   DefaultQPS,
			wantBurst: DefaultBurst,
		},
		{
			name:      "neither source available: the fallback's error is returned unwrapped",
			inCluster: func() (*rest.Config, error) { return nil, errNotInCluster },
			fallback:  func() (*rest.Config, error) { return nil, errNoKubeconfig },
			wantErr:   errNoKubeconfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubConfigSources(t, tc.inCluster, tc.fallback)

			cfg, err := RestConfig()
			if tc.wantErr != nil {
				assert.Same(t, tc.wantErr, err, "the error must be the fallback's own, not a wrap")
				assert.Nil(t, cfg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantQPS, cfg.QPS)
			assert.Equal(t, tc.wantBurst, cfg.Burst)
		})
	}
}
