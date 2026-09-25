package modality_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/modality"
)

func TestEnvNativeActive(t *testing.T) {
	cases := []struct {
		name string
		env  modality.Env
		cap  llm.Capability
		want bool
	}{
		{
			name: "opt-in + cap present => true",
			env: modality.Env{
				ModelCaps:   llm.NewCapabilitySet(llm.CapNativeFileOut),
				NativeOptIn: true,
			},
			cap:  llm.CapNativeFileOut,
			want: true,
		},
		{
			name: "opt-in but cap absent => false",
			env: modality.Env{
				ModelCaps:   llm.NewCapabilitySet(),
				NativeOptIn: true,
			},
			cap:  llm.CapNativeFileOut,
			want: false,
		},
		{
			name: "cap present but opt-out => false",
			env: modality.Env{
				ModelCaps:   llm.NewCapabilitySet(llm.CapNativeFileOut),
				NativeOptIn: false,
			},
			cap:  llm.CapNativeFileOut,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.env.NativeActive(tc.cap))
		})
	}
}
