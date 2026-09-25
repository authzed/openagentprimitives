package llm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func TestCapabilitySet_Has(t *testing.T) {
	cases := []struct {
		name string
		set  llm.CapabilitySet
		cap  llm.Capability
		want bool
	}{
		{
			name: "populated set has the capability it was built with",
			set:  llm.NewCapabilitySet(llm.CapNativeFileOut, llm.CapNativeFileIn),
			cap:  llm.CapNativeFileOut,
			want: true,
		},
		{
			name: "populated set does not have an unrelated capability",
			set:  llm.NewCapabilitySet(llm.CapNativeFileOut),
			cap:  llm.CapNativeFileIn,
			want: false,
		},
		{
			name: "empty set has nothing",
			set:  llm.NewCapabilitySet(),
			cap:  llm.CapNativeFileOut,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.set.Has(tc.cap))
		})
	}
}
