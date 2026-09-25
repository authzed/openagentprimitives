package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
)

// stubHarness is a minimal registrable Harness for registry tests.
type stubHarness struct{ name string }

func (s stubHarness) Name() string { return s.name }
func (s stubHarness) Container(harness.HarnessOpts) (harness.ContainerSpec, error) {
	return harness.ContainerSpec{}, nil
}
func (s stubHarness) ModelAccess() harness.ModelAccessMode { return harness.Proxied }

func TestResolve(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string
		want    string
	}{
		{
			name:  "absent name resolves to the oap-native default",
			input: "",
			want:  "ap-native",
		},
		{
			name:  "registered name resolves to that harness",
			input: "stub",
			want:  "stub",
		},
		{
			name:    "unregistered name is an error, never a silent default",
			input:   "nope",
			wantErr: `unknown harness "nope"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry.Reset()
			t.Cleanup(registry.Reset)
			registry.Register(stubHarness{name: "ap-native"})
			registry.Register(stubHarness{name: "stub"})

			got, err := registry.Resolve(tc.input)
			if tc.wantErr != "" {
				require.Error(t, err, "unregistered name must not resolve")
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, got, "a failed Resolve must return a nil interface, not a typed nil")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Name())
		})
	}
}

func TestResolveWithNoDefaultRegisteredIsAnError(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)

	got, err := registry.Resolve("")
	require.Error(t, err, "an empty name with no ap-native registered must fail closed")
	assert.Nil(t, got)
}
