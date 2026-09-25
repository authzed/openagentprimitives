package capability

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// declaringCap is an offerCap that declares ProviderSurface. Embedding rather
// than a flag, because the declaration is an INTERFACE — a capability either
// satisfies it or does not, and a bool would be testing something else.
type declaringCap struct{ *offerCap }

func (declaringCap) ActsOnProviderSurface() {}

var _ ProviderSurface = declaringCap{}

// TestProviderSurfaceTools_ReportsOnlyWhatDeclaringCapabilitiesOffered pins the
// three ways a tool can fail to belong in the set: its capability declared
// nothing, its capability is inactive, or its capability offered nothing this
// time. Each is a separate reason and none implies the others.
func TestProviderSurfaceTools_ReportsOnlyWhatDeclaringCapabilitiesOffered(t *testing.T) {
	newDeclaring := func(name string, def bool, tools []tool.Tool, skip *SkipReason) declaringCap {
		return declaringCap{&offerCap{name: name, def: def, tools: tools, skip: skip}}
	}

	cases := []struct {
		name  string
		setup func(t *testing.T)
		class *spiceboxv1alpha1.AgentClass
		want  []string
	}{
		{
			name: "a declaring capability's tools are the set",
			setup: func(t *testing.T) {
				t.Helper()
				Register(newDeclaring("trigger_status", true, []tool.Tool{
					stubTool{"claim_trigger_status"}, stubTool{"conclude_trigger_status"},
				}, nil))
				Register(&offerCap{name: "core", infra: true, tools: []tool.Tool{stubTool{"agent_work_complete"}}})
			},
			class: &spiceboxv1alpha1.AgentClass{},
			want:  []string{"claim_trigger_status", "conclude_trigger_status"},
		},
		{
			name: "a capability that declares nothing contributes nothing here",
			setup: func(t *testing.T) {
				t.Helper()
				Register(&offerCap{name: "channel_interaction", def: true, tools: []tool.Tool{stubTool{"respond_to_user"}}})
			},
			class: &spiceboxv1alpha1.AgentClass{},
			want:  nil,
		},
		{
			name: "a declaring capability the class disabled contributes nothing",
			setup: func(t *testing.T) {
				t.Helper()
				Register(newDeclaring("trigger_status", true, []tool.Tool{stubTool{"claim_trigger_status"}}, nil))
			},
			class: &spiceboxv1alpha1.AgentClass{
				Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("trigger_status", `{"enabled":false}`)},
			},
			want: nil,
		},
		{
			name: "a declaring capability that skipped contributes nothing",
			setup: func(t *testing.T) {
				t.Helper()
				Register(newDeclaring("trigger_status", true, nil,
					&SkipReason{Capability: "trigger_status", Reason: "the kind reports no trigger status"}))
			},
			class: &spiceboxv1alpha1.AgentClass{},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRegistryForTest(t)
			tc.setup(t)

			got := ProviderSurfaceTools(context.Background(), AssembleDeps{
				Class: tc.class, Logger: logr.Discard(),
			})
			names := make([]string, 0, len(got))
			for _, tl := range got {
				names = append(names, tl.Name())
			}
			assert.Equal(t, tc.want, nilIfEmpty(names))
		})
	}
}

// TestProviderSurfaceTools_AgreesWithAssemble is the invariant a consumer
// depends on: every tool reported here is a tool the SAME deps put in the
// merged list. A tool in one and not the other would reach a caller as a claim
// about a session that never had it.
func TestProviderSurfaceTools_AgreesWithAssemble(t *testing.T) {
	resetRegistryForTest(t)
	Register(declaringCap{&offerCap{
		name: "trigger_status", def: true,
		tools: []tool.Tool{stubTool{"claim_trigger_status"}, stubTool{"conclude_trigger_status"}},
	}})
	// Opt-in and NOT granted below, so it is the capability the two answers can
	// disagree about: a resolution path that skipped the grant would report its
	// tool as reaching a provider on a session that was never offered it.
	Register(declaringCap{&offerCap{name: "provider_writes", tools: []tool.Tool{stubTool{"write_upstream"}}}})
	Register(&offerCap{name: "core", infra: true, tools: []tool.Tool{stubTool{"agent_work_complete"}}})

	deps := AssembleDeps{Class: &spiceboxv1alpha1.AgentClass{}, Logger: logr.Discard()}
	assembled := map[string]bool{}
	for _, tl := range Assemble(context.Background(), deps) {
		assembled[tl.Name()] = true
	}

	for _, tl := range ProviderSurfaceTools(context.Background(), deps) {
		assert.True(t, assembled[tl.Name()],
			"%s is reported as reaching a provider surface but was never assembled", tl.Name())
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
