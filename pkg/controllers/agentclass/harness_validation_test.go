// pkg/controllers/agentclass/harness_validation_test.go
//
// Pure-function tests for validateHarness: no envtest required.
package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/authzed/openagentprimitives/pkg/agent/harness/apnative" // register the default harness; controller.go no longer does (consumers, not the controller package, own this per AGENTS.md)
	"github.com/authzed/openagentprimitives/pkg/agent/harness/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
)

// init registers a "demo-harness" fake alongside the "ap-native" harness
// that this file's own blank import to pkg/agent/harness/apnative
// registers at process start. Uses the idempotent guard (like
// binding_validation_test.go's chregistry init) so re-linking this test
// package alongside any other file that registers the same name doesn't
// panic on duplicate registration.
//
// The harness registry is a process-global singleton shared by every test
// in this package, so TestValidateHarness deliberately never calls
// registry.Reset(): resetting would wipe the "ap-native" entry that only
// apnative's own init() ever creates, permanently breaking every other
// test in the package that reconciles a class with an absent
// spec.harness for the rest of the test binary's run.
func init() {
	if _, ok := registry.Get("demo-harness"); !ok {
		registry.Register(fake.New("demo-harness"))
	}
}

func TestValidateHarness(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{name: "absent harness is valid and means ap-native", spec: ""},
		{name: "registered harness is valid", spec: "demo-harness"},
		{
			name:    "unregistered harness is rejected",
			spec:    "ghost",
			wantErr: `unknown harness "ghost"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHarness(tc.spec)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}
