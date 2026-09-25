package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func TestExposeNamespace(t *testing.T) {
	cases := []struct {
		name string
		g    apcmd.Globals
		want string
	}{
		{
			name: "-n unset: the system namespace oap install uses",
			g:    apcmd.Globals{},
			want: apcmd.SystemNamespace,
		},
		{
			name: "-n set: the global namespace, not the system one",
			g:    apcmd.Globals{Namespace: "custom-ns"},
			want: "custom-ns",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, exposeNamespace(&tc.g))
		})
	}
}

func TestSpiceDBExpose_DefaultArgv(t *testing.T) {
	argv := buildSpiceDBExposeKubectlArgv("agentprimitives-system", 50051, 50051)
	assert.Equal(t,
		[]string{"port-forward", "-n", "agentprimitives-system", "svc/spicebox-spicedb", "50051:50051"},
		argv,
	)
}

func TestSpiceDBExpose_CustomPorts(t *testing.T) {
	argv := buildSpiceDBExposeKubectlArgv("custom-ns", 60051, 8443)
	assert.Equal(t,
		[]string{"port-forward", "-n", "custom-ns", "svc/spicebox-spicedb", "60051:8443"},
		argv,
	)
}
