package agentcmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestAgentAuthzCmd_Registered(t *testing.T) {
	cmd := NewCmd(&apcmd.Globals{})
	var found bool
	for _, c := range cmd.Commands() {
		if c.Name() == "authz" {
			found = true
		}
	}
	assert.True(t, found, "`oap agent authz` must be registered under `oap agent`")
}

func TestRenderAuthzView_AllOn(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Slots: []spiceboxv1alpha1.AuthzSlot{
					{ResourceType: "github_repo", Description: "repo", Permission: "read"},
				},
				ApprovalTimeout:    &metav1.Duration{Duration: 7 * time.Minute},
				ToolCalls:          &spiceboxv1alpha1.ToolCallsAuthz{Mode: "enforcing"},
				Session:            &spiceboxv1alpha1.SessionAuthz{InteractPermission: "user:owner"},
				InformationLeakage: &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"},
				Scope:              &spiceboxv1alpha1.ScopeSpec{Enabled: true, ColdStart: "extractAndApprove"},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderAuthzView(&buf, tui.NewTheme(tui.Caps{}), ac))
	out := buf.String()

	// Header carries the class name and the consolidated timeout.
	assert.Contains(t, out, "demo-agent")
	assert.Contains(t, out, "7m0s", "consolidated approval timeout must be shown")

	// Active hooks rendered with their points.
	assert.Contains(t, out, "tool_call_authz")
	assert.Contains(t, out, "scope")
	assert.Contains(t, out, "cold_start_scope")
	assert.Contains(t, out, "info_leak_audience")
	assert.Contains(t, out, "session_cleanup")
	assert.Contains(t, out, "pre_tool_call", "hook fire points must be shown")
	assert.Contains(t, out, "session_end")

	// McpTrust is rendered conditional (session-tool dependent).
	mcpLine := lineWithHook(t, out, "mcp_trust")
	assert.Contains(t, strings.ToLower(mcpLine), "conditional",
		"McpTrust must render as conditional at the class level")
}

func TestRenderAuthzView_ScopeOnly_ToolCallsDisabled(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "scoped", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: "disabled"},
				Scope:     &spiceboxv1alpha1.ScopeSpec{Enabled: true, ColdStart: "off"},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderAuthzView(&buf, tui.NewTheme(tui.Caps{}), ac))
	out := buf.String()

	scopeLine := lineWithHook(t, out, "scope")
	assert.Contains(t, strings.ToLower(scopeLine), "active",
		"Scope must render ACTIVE even with toolCalls disabled (spec §8 gap)")
	assert.NotContains(t, strings.ToLower(scopeLine), "inactive",
		"scope line must be the active scope hook, not cold_start_scope")

	tcaLine := lineWithHook(t, out, "tool_call_authz")
	assert.Contains(t, strings.ToLower(tcaLine), "inactive",
		"tool_call_authz must render inactive when toolCalls.mode=disabled")

	// Default timeout (10m) when authz.approvalTimeout is unset.
	assert.Contains(t, out, "10m0s")
}

// lineWithHook returns the line whose first whitespace-delimited field equals
// name (so "scope" does not match the "cold_start_scope" row).
func lineWithHook(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return line
		}
	}
	t.Fatalf("no row for hook %q in:\n%s", name, out)
	return ""
}

// The interact gate is active for a class whose policy was DERIVED as much as
// for one that declared it — the same effective value governs the session
// either way. A view reading only the declared half would report the gate off
// on exactly the classes that most need it explained: the webhook-driven ones,
// which never declare the field because the only sensible value is per-install.
func TestRenderAuthzView_DerivedInteractPermissionActivatesTheGate(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentClassStatus{
			DerivedSessionInteractPermission: "slack_channel:C0DEMO123#member",
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderAuthzView(&buf, tui.NewTheme(tui.Caps{}), ac))
	out := buf.String()

	assert.Contains(t, out, "slack_channel:C0DEMO123#member",
		"the derived subject-set must be visible; an operator cannot read it off the spec")
	assert.Contains(t, out, "derived",
		"and the line must say the value was derived, not authored")
	interactLine := strings.ToLower(lineWithHook(t, out, "interact"))
	assert.Contains(t, interactLine, "active",
		"the interact hook is active on the effective policy, declared or derived")
	assert.NotContains(t, interactLine, "inactive",
		"a derived policy is in force; the gate is not off")
}

// The declared case must keep saying "declared", so the two are told apart at a
// glance — a derived whole-channel subject-set and one an operator chose carry
// the same weight in SpiceDB and very different weight in review.
func TestRenderAuthzView_DeclaredInteractPermissionSaysDeclared(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: "group:demo-maintainers#member"},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{
			// Never written together with a declared value by the reconciler;
			// present here to prove the view reads the effective precedence and
			// not whichever field it finds first.
			DerivedSessionInteractPermission: "slack_channel:C0DEMO123#member",
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderAuthzView(&buf, tui.NewTheme(tui.Caps{}), ac))
	out := buf.String()

	assert.Contains(t, out, "group:demo-maintainers#member")
	assert.Contains(t, out, "declared")
	assert.NotContains(t, out, "slack_channel:C0DEMO123#member",
		"a declared policy is what is in force; the derived field is not")
}
