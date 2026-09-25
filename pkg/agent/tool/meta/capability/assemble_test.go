package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// caps builds a map[string]apiextensionsv1.JSON with a single key/rawJSON
// pair, for constructing AgentClassSpec.Capabilities in tests.
func caps(key, raw string) map[string]apiextensionsv1.JSON {
	return map[string]apiextensionsv1.JSON{key: {Raw: []byte(raw)}}
}

// stubTool is a minimal tool.Tool for assembly tests.
type stubTool struct{ name string }

func (s stubTool) Name() string                 { return s.name }
func (s stubTool) Kind() tool.Kind              { return tool.KindMeta }
func (s stubTool) Description() string          { return "" }
func (s stubTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (s stubTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (s stubTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (s stubTool) PermissionVariants() []authz.PermissionVariant { return nil }

// offerCap is a fake capability driven by fields.
type offerCap struct {
	name  string
	def   bool
	infra bool
	tools []tool.Tool
	skip  *SkipReason
}

func (o *offerCap) Name() string                                  { return o.name }
func (o *offerCap) DefaultOn() bool                               { return o.def }
func (o *offerCap) Infrastructural() bool                         { return o.infra }
func (o *offerCap) ParseConfig(json.RawMessage) (Config, error)   { return nil, nil }
func (o *offerCap) Offer(OfferContext) ([]tool.Tool, *SkipReason) { return o.tools, o.skip }

func TestAssembleActiveOrderingAndIntrospectionLast(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "core", infra: true, tools: []tool.Tool{stubTool{"agent_work_complete"}}})
	Register(&offerCap{name: "planning", def: true, tools: []tool.Tool{stubTool{"update_plan"}}})
	Register(&offerCap{name: "memory", tools: []tool.Tool{stubTool{"query_memory"}}}) // opt-in, not granted → inactive
	Register(&offerCap{name: "introspection", infra: true, tools: []tool.Tool{stubTool{"introspect_tool"}}})

	class := &spiceboxv1alpha1.AgentClass{}
	got := Assemble(context.Background(), AssembleDeps{
		Class:        class,
		NonMetaTools: []tool.Tool{stubTool{"sandbox_run"}},
		Logger:       logr.Discard(),
	})

	names := make([]string, 0, len(got))
	for _, tt := range got {
		names = append(names, tt.Name())
	}
	assert.Equal(t, []string{"agent_work_complete", "update_plan", "sandbox_run", "introspect_tool"}, names)
	assert.NotContains(t, names, "query_memory", "opt-in memory not granted → absent")
}

func TestAssembleGrantedOptInActivates(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "memory", tools: []tool.Tool{stubTool{"query_memory"}}})
	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Capabilities: caps("memory", `{}`),
		},
	}
	got := Assemble(context.Background(), AssembleDeps{Class: class, Logger: logr.Discard()})
	require.Len(t, got, 1)
	assert.Equal(t, "query_memory", got[0].Name())
}

func TestAssembleSkipLogsNothingInjected(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "artifacts", tools: nil, skip: &SkipReason{Capability: "artifacts", Reason: "no renderer"}})
	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("artifacts", `{}`)},
	}
	got := Assemble(context.Background(), AssembleDeps{Class: class, Logger: logr.Discard()})
	assert.Empty(t, got, "granted-but-skipped capability injects nothing")
}

// A capability that withholds ONE of the tools it contributes returns the rest
// ALONGSIDE the reason. Both halves matter and neither implies the other:
// dropping the returned tools would punish a session for a deliberate
// narrowing, and dropping the log line would make a tool vanish with nothing
// anywhere saying why.
func TestAssemble_PartialSkip_KeepsTheToolsAndStillLogsTheReason(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{
		name: "channel_interaction", def: true,
		tools: []tool.Tool{stubTool{"await_user_message"}, stubTool{"update_status"}},
		skip:  &SkipReason{Capability: "channel_interaction", Reason: "respond_to_user withheld: it reaches another agent"},
	})

	var logged []string
	logger := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})
	got := Assemble(context.Background(), AssembleDeps{
		Class:  &spiceboxv1alpha1.AgentClass{},
		Logger: logger,
	})

	names := make([]string, 0, len(got))
	for _, tt := range got {
		names = append(names, tt.Name())
	}
	assert.Equal(t, []string{"await_user_message", "update_status"}, names,
		"the tools that were NOT withheld must still be injected")
	require.Len(t, logged, 1, "a withheld tool must produce exactly one line")
	assert.Contains(t, logged[0], "respond_to_user withheld")
}

// An infrastructural capability explicitly disabled via {"enabled":false} is
// STILL active — the infra bypass ignores the grant/enable gate entirely.
func TestAssembleInfraIgnoresExplicitDisable(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "core", infra: true, tools: []tool.Tool{stubTool{"agent_work_complete"}}})
	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("core", `{"enabled":false}`)},
	}
	got := Assemble(context.Background(), AssembleDeps{Class: class, Logger: logr.Discard()})
	require.Len(t, got, 1, "infra capability must stay active despite {enabled:false}")
	assert.Equal(t, "agent_work_complete", got[0].Name())
}

// A non-infra capability whose ParseConfig is a no-op, granted with a
// malformed {enabled} blob, goes inactive (fail-closed) AND emits a log line —
// the malformed-JSON error must never be silently dropped even when the
// capability's own ParseConfig can't observe it.
func TestAssembleMalformedEnabledLogsAndInactivates(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "memory", tools: []tool.Tool{stubTool{"query_memory"}}}) // ParseConfig is a no-op
	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("memory", `{`)}, // malformed JSON
	}

	var logged []string
	lg := funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{})

	got := Assemble(context.Background(), AssembleDeps{Class: class, Logger: lg})
	assert.Empty(t, got, "malformed {enabled} → capability inactive, no tools injected")
	require.NotEmpty(t, logged, "malformed {enabled} must emit a log line, not be silently dropped")
	assert.Contains(t, logged[0], "parse failed", "log line should describe the parse failure")
	assert.Contains(t, logged[0], "memory", "log line should name the capability")
}

// sectionCap is a fake SectionOfferer: the same fields as offerCap plus the
// sections it contributes. Offer delegates to OfferWithSections and drops the
// sections, which is the contract every real SectionOfferer keeps.
type sectionCap struct {
	offerCap
	sections []PromptSection
}

func (s *sectionCap) OfferWithSections(OfferContext) ([]tool.Tool, []PromptSection, *SkipReason) {
	return s.tools, s.sections, s.skip
}

func TestAssembleAll_CollectsSectionsFromSectionOfferersInRegistryOrder(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "core", infra: true, tools: []tool.Tool{stubTool{"agent_work_complete"}}})
	Register(&sectionCap{
		offerCap: offerCap{name: "update_view", tools: []tool.Tool{stubTool{"update_view"}}},
		sections: []PromptSection{{Title: "Your page", Body: "hooks..."}},
	})
	Register(&sectionCap{
		offerCap: offerCap{name: "workspace", def: true, tools: []tool.Tool{stubTool{"ws_list"}}},
		sections: []PromptSection{{Title: "Your workspace", Body: "paths..."}},
	})
	class := &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("update_view", `{}`)}}

	got := AssembleAll(context.Background(), AssembleDeps{Class: class, Logger: logr.Discard()})

	names := make([]string, 0, len(got.Tools))
	for _, tt := range got.Tools {
		names = append(names, tt.Name())
	}
	assert.Equal(t, []string{"agent_work_complete", "update_view", "ws_list"}, names)
	assert.Equal(t, []PromptSection{
		{Title: "Your page", Body: "hooks..."},
		{Title: "Your workspace", Body: "paths..."},
	}, got.Sections, "sections arrive in the same registry order as their tools; a plain Capability contributes none")
}

func TestAssembleAll_InactiveSectionOffererContributesNoSection(t *testing.T) {
	resetRegistryForTest(t)
	Register(&sectionCap{
		offerCap: offerCap{name: "update_view", tools: []tool.Tool{stubTool{"update_view"}}},
		sections: []PromptSection{{Title: "Your page", Body: "hooks..."}},
	})
	got := AssembleAll(context.Background(), AssembleDeps{Class: &spiceboxv1alpha1.AgentClass{}, Logger: logr.Discard()})
	assert.Empty(t, got.Tools, "opt-in, not granted")
	assert.Empty(t, got.Sections, "no tool, no text: the two are one decision")
}

func TestAssembleAll_DropsASectionThatCameWithNoToolAndLogsIt(t *testing.T) {
	resetRegistryForTest(t)
	Register(&sectionCap{
		offerCap: offerCap{name: "update_view", tools: nil, skip: &SkipReason{Capability: "update_view", Reason: "no hooks"}},
		sections: []PromptSection{{Title: "Your page", Body: "hooks..."}},
	})
	class := &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("update_view", `{}`)}}
	var lines []string
	lg := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})

	got := AssembleAll(context.Background(), AssembleDeps{Class: class, Logger: lg})

	assert.Empty(t, got.Tools)
	assert.Empty(t, got.Sections, "text describing tools the agent does not have is dropped")
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "no hooks", "the skip is still logged")
	assert.Contains(t, joined, "prompt text with no tool", "and so is the drop — never silent")
}

func TestAssemble_ReturnsExactlyAssembleAllsTools(t *testing.T) {
	resetRegistryForTest(t)
	Register(&offerCap{name: "core", infra: true, tools: []tool.Tool{stubTool{"agent_work_complete"}}})
	Register(&sectionCap{
		offerCap: offerCap{name: "update_view", tools: []tool.Tool{stubTool{"update_view"}}},
		sections: []PromptSection{{Title: "Your page", Body: "hooks..."}},
	})
	class := &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("update_view", `{}`)}}
	deps := AssembleDeps{Class: class, Logger: logr.Discard()}
	all := AssembleAll(context.Background(), deps)
	only := Assemble(context.Background(), deps)
	require.Len(t, only, len(all.Tools))
	for i := range only {
		assert.Equal(t, all.Tools[i].Name(), only[i].Name())
	}
}
