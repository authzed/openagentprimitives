package synthesize_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// fakeTool is a minimal tool.Tool used as the Factory output in tests.
type fakeTool struct {
	name string
}

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Kind() tool.Kind              { return tool.KindMeta }
func (f *fakeTool) Description() string          { return "fake" }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (*fakeTool) Permission() authz.Permission                  { return authz.Permission{StateImpact: authz.Stateless} }
func (*fakeTool) PermissionVariants() []authz.PermissionVariant { return nil }

// fakeSource is a stub Source backed by a literal prefix and slice of entries.
type fakeSource struct {
	prefix  string
	entries []synthesize.Entry
}

func (f fakeSource) Prefix() string              { return f.prefix }
func (f fakeSource) Entries() []synthesize.Entry { return f.entries }

func makeFactory(t *testing.T) (func(name string) tool.Tool, func() []string) {
	t.Helper()
	var seen []string
	return func(name string) tool.Tool {
			seen = append(seen, name)
			return &fakeTool{name: name}
		}, func() []string {
			return seen
		}
}

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercase passthrough", in: "abc", want: "abc"},
		{name: "uppercase -> lowercase", in: "ABC", want: "abc"},
		{name: "underscore + dash preserved", in: "abc_def-ghi", want: "abc_def-ghi"},
		{name: "mixed alphanumeric -> lowercase", in: "a1B2", want: "a1b2"},
		{name: "space -> dash", in: "hello world", want: "hello-world"},
		{name: "multi-byte emoji collapses to single dash", in: "emoji😀x", want: "emoji-x"},
		{name: "empty stays empty", in: "", want: ""},
		{name: "underscore + uppercase + dash mix", in: "linear_DELETE-issue", want: "linear_delete-issue"},
		{name: "truncates at 128 chars", in: strings.Repeat("a", 130), want: strings.Repeat("a", 128)},
		{name: "exact 128 chars passes through", in: strings.Repeat("a", 128), want: strings.Repeat("a", 128)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, synthesize.NormalizeName(tc.in))
		})
	}
}

func TestBuild_BuildsToolPerEntry(t *testing.T) {
	factory, _ := makeFactory(t)
	src := fakeSource{
		prefix: "linear",
		entries: []synthesize.Entry{
			{Name: "search_issues", Factory: factory},
			{Name: "DELETE-issue", Factory: factory},
		},
	}
	tools, err := synthesize.Build(src)
	require.NoError(t, err)
	require.Len(t, tools, 2)
	assert.Equal(t, "linear_search_issues", tools[0].Name())
	assert.Equal(t, "linear_delete-issue", tools[1].Name())
}

func TestBuild_AppliesPrefix(t *testing.T) {
	factory, _ := makeFactory(t)
	src := fakeSource{
		prefix: "linear",
		entries: []synthesize.Entry{
			{Name: "tool_one", Factory: factory},
		},
	}
	tools, err := synthesize.Build(src)
	require.NoError(t, err)
	assert.Equal(t, "linear_tool_one", tools[0].Name(), "expected prefix joined with _")
}

func TestBuild_EmptyPrefix(t *testing.T) {
	factory, _ := makeFactory(t)
	src := fakeSource{
		prefix: "",
		entries: []synthesize.Entry{
			{Name: "agent_work_complete", Factory: factory},
		},
	}
	tools, err := synthesize.Build(src)
	require.NoError(t, err)
	assert.Equal(t, "agent_work_complete", tools[0].Name(), "empty prefix should leave name alone")
}

func TestBuild_CollisionDetected(t *testing.T) {
	factory, _ := makeFactory(t)
	// "tool one" and "tool-one" both normalize to "tool-one".
	src := fakeSource{
		prefix: "",
		entries: []synthesize.Entry{
			{Name: "tool one", Factory: factory},
			{Name: "tool-one", Factory: factory},
		},
	}
	_, err := synthesize.Build(src)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "tool one", "collision error should mention both raw names")
	assert.Contains(t, msg, "tool-one")
	assert.Contains(t, msg, "collision")
}

func TestBuild_PreservesOrder(t *testing.T) {
	factory, seen := makeFactory(t)
	src := fakeSource{
		prefix: "p",
		entries: []synthesize.Entry{
			{Name: "alpha", Factory: factory},
			{Name: "beta", Factory: factory},
			{Name: "gamma", Factory: factory},
			{Name: "delta", Factory: factory},
		},
	}
	tools, err := synthesize.Build(src)
	require.NoError(t, err)
	wantNames := []string{"p_alpha", "p_beta", "p_gamma", "p_delta"}
	gotNames := make([]string, len(tools))
	for i, tl := range tools {
		gotNames[i] = tl.Name()
	}
	assert.Equal(t, wantNames, gotNames)
	// Factory invocation order should match Source.Entries() order too.
	assert.Equal(t, wantNames, seen())
}
