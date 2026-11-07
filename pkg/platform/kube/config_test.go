package kube

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
		{name: "already set (GetConfig's 20/30): kept", in: rest.Config{QPS: 20, Burst: 30}, wantQPS: 20, wantBurst: 30},
		{name: "only QPS set: Burst defaulted, QPS kept", in: rest.Config{QPS: 7}, wantQPS: 7, wantBurst: DefaultBurst},
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
