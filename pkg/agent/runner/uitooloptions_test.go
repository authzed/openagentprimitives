package runner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// fakeUITool is a minimal tool.Tool plus the optional ServerReadOnlyHint()
// the readonly gate probes, mirroring apptoolcall_test.go's fakeAppTool.
// Named distinctly from prompt_test.go's own fakeTool — both live in
// package runner_test.
type fakeUITool struct {
	name   string
	impact authz.StateImpact
	hint   bool
}

func (f fakeUITool) Name() string                                  { return f.name }
func (f fakeUITool) Kind() tool.Kind                               { return tool.KindMCP }
func (f fakeUITool) Description() string                           { return "fake tool" }
func (f fakeUITool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (f fakeUITool) Permission() authz.Permission                  { return authz.Permission{StateImpact: f.impact} }
func (f fakeUITool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f fakeUITool) ServerReadOnlyHint() bool                      { return f.hint }
func (f fakeUITool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

func TestUIToolOptionsMirrorsTheRuntimeGate(t *testing.T) {
	appTools := map[string]tool.Tool{
		"crm_list_leads":    fakeUITool{name: "crm_list_leads", impact: authz.Readonly, hint: true},
		"crm_advance_stage": fakeUITool{name: "crm_advance_stage", impact: authz.Readwrite},
		"crm_peek_no_hint":  fakeUITool{name: "crm_peek_no_hint", impact: authz.Readonly, hint: false},
	}
	o := runner.UIToolOptions(appTools)

	assert.True(t, o.GrantedTools["crm_advance_stage"], "a mutating tool IS granted — an ACTION may name it")
	assert.True(t, o.ReadonlyTools["crm_list_leads"])
	assert.False(t, o.ReadonlyTools["crm_advance_stage"])
	assert.False(t, o.ReadonlyTools["crm_peek_no_hint"],
		"a missing readOnlyHint denies — the same predicate handleAppToolCallReq applies")
	require.NotNil(t, o.NormalizeToolName)
	// synthesize.NormalizeName lowercases and replaces every character
	// outside [a-z0-9_-] with '-': "CRM.List_Leads" -> "crm-list_leads" (the
	// dot becomes a dash; the existing underscore is left alone).
	assert.Equal(t, "crm-list_leads", o.NormalizeToolName("CRM.List_Leads"),
		"a Binding.Ref carries no CRD pattern and must be normalized into the AppTools key vocabulary")
}

func TestUIToolOptionsFailsClosedOnEmptyAppTools(t *testing.T) {
	o := runner.UIToolOptions(nil)
	assert.Empty(t, o.GrantedTools)
	assert.Empty(t, o.ReadonlyTools)
}

// TestAttachUIViewWiresPublishClosingOverRuntimeSessionIdentity proves
// rt.Publish names the Runtime's OWN Namespace/Session — never a Loop
// field, since Loop carries no session identity of its own — and reads
// Loop.UIPublish LATE (at call time), matching ToolOptions' own late-bound
// contract: internal/cmd/runner constructs uiViewRT and calls AttachUIView BEFORE
// loop.UIPublish is assigned.
func TestAttachUIViewWiresPublishClosingOverRuntimeSessionIdentity(t *testing.T) {
	rt := &uiview.Runtime{Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui"}
	l := &runner.Loop{}
	runner.AttachUIView(rt, l)
	require.NotNil(t, rt.Publish, "AttachUIView must wire rt.Publish — a severed wiring means an open page never learns of a write")

	// UIPublish is set AFTER AttachUIView, mirroring internal/cmd/runner/main.go's
	// real ordering (uiViewRT is attached before the NATS-wiring block that
	// assigns loop.UIPublish).
	var gotNS, gotName string
	var gotEnv channelevents.Envelope
	l.UIPublish = func(_ context.Context, ns, name string, env channelevents.Envelope) error {
		gotNS, gotName, gotEnv = ns, name, env
		return nil
	}

	require.NoError(t, rt.Publish(context.Background(), channelevents.Envelope{Kind: channelevents.KindUIViewUpdate}))
	assert.Equal(t, "demo-ns", gotNS)
	assert.Equal(t, "demo-session", gotName)
	assert.Equal(t, channelevents.KindUIViewUpdate, gotEnv.Kind)
}

// TestAttachUIViewPublishIsNilSafeWhenLoopUIPublishUnset covers the
// kubectl-driven/test-Loop case: Loop.UIPublish stays nil (no NATS), and
// rt.Publish must degrade to a no-op rather than panic on a nil func call.
func TestAttachUIViewPublishIsNilSafeWhenLoopUIPublishUnset(t *testing.T) {
	rt := &uiview.Runtime{Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui"}
	l := &runner.Loop{} // UIPublish left nil
	runner.AttachUIView(rt, l)

	assert.NotPanics(t, func() {
		require.NoError(t, rt.Publish(context.Background(), channelevents.Envelope{Kind: channelevents.KindUIViewUpdate}))
	})
}
