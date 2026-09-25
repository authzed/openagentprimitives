package portforward

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestPortForwarderConstruction(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *rest.Config
		namespace  string
		service    string
		selector   string
		targetPort uint16
		localPort  uint16
		wantErr    bool
		wantLocal  uint16
	}{
		{
			name:       "ok / operator forward, OS-assigned local",
			cfg:        &rest.Config{Host: "https://example.invalid"},
			namespace:  "agentprimitives-system",
			service:    "spicebox-operator",
			selector:   "app.kubernetes.io/name=spicebox-operator",
			targetPort: 8082,
			localPort:  0,
			wantLocal:  0,
		},
		{
			name:       "ok / spicedb forward, fixed local",
			cfg:        &rest.Config{Host: "https://example.invalid"},
			namespace:  "agentprimitives-system",
			service:    "spicebox-spicedb",
			selector:   "app.kubernetes.io/name=spicebox-spicedb",
			targetPort: 50051,
			localPort:  60061,
			wantLocal:  60061,
		},
		{
			name:       "nil cfg: returns error",
			cfg:        nil,
			namespace:  "x",
			service:    "y",
			selector:   "z",
			targetPort: 1,
			wantErr:    true,
		},
		{
			name:       "empty namespace: returns error",
			cfg:        &rest.Config{},
			namespace:  "",
			service:    "y",
			selector:   "z",
			targetPort: 1,
			wantErr:    true,
		},
		{
			name:       "empty service: returns error",
			cfg:        &rest.Config{},
			namespace:  "x",
			service:    "",
			selector:   "z",
			targetPort: 1,
			wantErr:    true,
		},
		{
			name:       "empty selector: returns error",
			cfg:        &rest.Config{},
			namespace:  "x",
			service:    "y",
			selector:   "",
			targetPort: 1,
			wantErr:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf, err := New(tc.cfg, tc.namespace, tc.service, tc.selector, tc.targetPort, tc.localPort)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, pf)
			assert.Equal(t, tc.selector, pf.selector, "selector round-trips through New")
			assert.Equal(t, tc.wantLocal, pf.requestedPort, "requested local port round-trips through New")
		})
	}
}
