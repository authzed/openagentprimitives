package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// fakeIntrospectable is a stub Tool implementing Introspectable.
type fakeIntrospectable struct{ name, body string }

func (f *fakeIntrospectable) Name() string                                  { return f.name }
func (f *fakeIntrospectable) Kind() tool.Kind                               { return tool.KindSandbox }
func (f *fakeIntrospectable) Description() string                           { return "fake" }
func (f *fakeIntrospectable) InputSchema() json.RawMessage                  { return json.RawMessage(`{}`) }
func (f *fakeIntrospectable) Permission() authz.Permission                  { return authz.Permission{} }
func (f *fakeIntrospectable) PermissionVariants() []authz.PermissionVariant { return nil }
func (f *fakeIntrospectable) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (f *fakeIntrospectable) Introspect() (string, error) { return f.body, nil }

// plainTool implements tool.Tool but NOT Introspectable.
type plainTool struct{ name string }

func (p *plainTool) Name() string                                  { return p.name }
func (p *plainTool) Kind() tool.Kind                               { return tool.KindMeta }
func (p *plainTool) Description() string                           { return "plain" }
func (p *plainTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{}`) }
func (p *plainTool) Permission() authz.Permission                  { return authz.Permission{} }
func (p *plainTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (p *plainTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

func TestIntrospectTool_KnownTool(t *testing.T) {
	target := &fakeIntrospectable{name: "git_git", body: "FULL CONTRACT HERE"}
	it := meta.NewIntrospect(meta.IntrospectConfig{
		Resolve: func(n string) (tool.Tool, bool) {
			if n == "git_git" {
				return target, true
			}
			return nil, false
		},
	})
	res, err := it.Execute(context.Background(),
		json.RawMessage(`{"tool":"git_git"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content, "FULL CONTRACT HERE")
	assert.True(t, res.Trusted, "introspect_tool is a framework meta tool and must opt out of content-guard inspection")
}

func TestIntrospectTool_UnknownTool(t *testing.T) {
	it := meta.NewIntrospect(meta.IntrospectConfig{
		Resolve: func(string) (tool.Tool, bool) { return nil, false },
		Names:   func() []string { return []string{"git_git", "code_claude"} },
	})
	res, err := it.Execute(context.Background(),
		json.RawMessage(`{"tool":"nope"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "git_git")
	assert.Contains(t, res.Content, "code_claude")
}

func TestIntrospectTool_NotIntrospectable(t *testing.T) {
	plain := &plainTool{name: "respond_to_user"}
	it := meta.NewIntrospect(meta.IntrospectConfig{
		Resolve: func(string) (tool.Tool, bool) { return plain, true },
	})
	res, err := it.Execute(context.Background(),
		json.RawMessage(`{"tool":"respond_to_user"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content, "no additional contract")
}

func TestIntrospectTool_MissingArg(t *testing.T) {
	it := meta.NewIntrospect(meta.IntrospectConfig{
		Resolve: func(string) (tool.Tool, bool) { return nil, false },
	})
	res, err := it.Execute(context.Background(),
		json.RawMessage(`{"tool":""}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}
