package runner

import (
	"testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
)

func hookNamesAt(reg *pipeline.Registry, p pipeline.Point) []string {
	var out []string
	for _, h := range reg.Hooks(p) {
		out = append(out, h.Name())
	}
	return out
}

// enableScopeAndLeakageForTest drives the shared activation table to
// "scope on + leakage on" by setting the minimal Loop/AgentClass fields
// activationConfig() reads (pipeline_wiring.go): scope.enabled via
// AgentClass.Spec.Authz.Scope, and leakage mode via LeakageConfig.ResolvedMode().
func enableScopeAndLeakageForTest(t *testing.T, l *Loop) {
	t.Helper()
	if l.AgentClass == nil {
		l.AgentClass = &spiceboxv1alpha1.AgentClass{}
	}
	if l.AgentClass.Spec.Authz == nil {
		l.AgentClass.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{}
	}
	l.AgentClass.Spec.Authz.Scope = &spiceboxv1alpha1.ScopeSpec{Enabled: true}
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}
}

func TestBuildPipelineRegistry_MinimalConfig(t *testing.T) {
	// No AgentClass, no toolguard/content/revocation, no MCP, no scope/leakage
	// config. ToolAuthMode defaults to "enforcing" when unset (matches the CRD
	// default — see (*Loop).toolAuthMode), so tool_call_authz is still active;
	// scope/leakage need AgentClass/LeakageConfig respectively and are absent
	// here. Only the unconditional SessionEnd cleanup hook and the
	// default-enforcing tool_call_authz hook are registered.
	l := &Loop{}
	reg := l.buildPipelineRegistry()
	assert.Equal(t, []string{"session_cleanup"}, hookNamesAt(reg, pipeline.SessionEnd))
	assert.Equal(t, []string{"tool_call_authz"}, hookNamesAt(reg, pipeline.PreToolCall))
}

func TestBuildPipelineRegistry_FullConfig_OrderPreserved(t *testing.T) {
	l := &Loop{
		AgentClass: &spiceboxv1alpha1.AgentClass{}, // scope/leakage configured below
	}
	// Drive the activation table to "all on" via the projected config. Set the
	// minimal fields activationConfig() reads (see pipeline_wiring.go).
	l.ToolAuthMode = "enforcing"
	// scope + leakage on:
	enableScopeAndLeakageForTest(t, l) // helper sets AgentClass scope.enabled + LeakageConfig mode=enforcing

	reg := l.buildPipelineRegistry()

	pre := hookNamesAt(reg, pipeline.PreToolCall)
	// Order: tool_call_authz(20) < scope(30) < info_leak_audience.
	//
	// info_leak_audience joined PreToolCall with per-datum provenance: a tool
	// call carries data OUT of the session, and its audience is whoever can
	// read the destination. It sits LAST on purpose — there is nothing to say
	// about the egress of a call that authorization or scope has already
	// refused, and running it earlier would spend a destination lookup on
	// calls that never happen.
	assert.Equal(t, []string{"tool_call_authz", "scope", "info_leak_audience"}, pre)

	post := hookNamesAt(reg, pipeline.PostToolCall)
	// A hook added here must also be CLASSIFIED for the ungated meta path: the
	// runner runs the read-side PostToolCall hooks around the ungated execute
	// too (readSidePostHooks in leakageread_meta.go), and membership there is a
	// judgement, not a consequence of the order value.
	assert.Equal(t, []string{"scope", "info_leak_read", "info_leak_audience"}, post)

	assert.Equal(t, []string{"info_leak_audience"}, hookNamesAt(reg, pipeline.PreResponse))
	assert.Equal(t, []string{"session_cleanup"}, hookNamesAt(reg, pipeline.SessionEnd))
}
