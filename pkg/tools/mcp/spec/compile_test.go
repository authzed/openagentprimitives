package spec

import (
	"strings"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadBytes loads a YAML spec from inline bytes, failing the test on any
// load error so the caller can focus on Compile-time assertions.
func loadBytes(t *testing.T, src []byte) *Spec {
	t.Helper()
	sp, err := LoadBytes(src)
	require.NoError(t, err, "LoadBytes")
	return sp
}

// loadFile loads a YAML spec from a testdata path, failing on any load error.
func loadFile(t *testing.T, path string) *Spec {
	t.Helper()
	sp, err := Load(path)
	require.NoError(t, err, "Load %s", path)
	return sp
}

// TestCompile_OK covers every shape of spec we expect Compile to accept
// without error: the happy-path file, the has() guard pattern, size()/
// matches() built-ins, and a nil spec (no-op).
func TestCompile_OK(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) *Spec
	}{
		{
			name: "happy path testdata file compiles",
			build: func(t *testing.T) *Spec {
				return loadFile(t, "testdata/linear-readonly.yaml")
			},
		},
		{
			name: "has() guard around optional arg compiles",
			build: func(t *testing.T) *Spec {
				return loadBytes(t, []byte(`
name: t
version: "1"
server: { url: http://x, transport: streamable-http }
tools:
  - name: tool
    args:
      constraints:
        - cel: '!has(args.optional) || args.optional == "ok"'
`))
			},
		},
		{
			name: "size() and matches() built-ins compile",
			build: func(t *testing.T) *Spec {
				return loadBytes(t, []byte(`
name: t
version: "1"
server: { url: http://x, transport: streamable-http }
tools:
  - name: tool
    args:
      constraints:
        - cel: 'args.query.size() <= 200'
        - cel: 'args.issueId.matches("^[A-Z]+-[0-9]+$")'
`))
			},
		},
		{
			name:  "nil spec is a no-op",
			build: func(*testing.T) *Spec { return nil },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.build(t))
			assert.NoError(t, err, "Compile")
		})
	}
}

// TestCompile_Error covers every shape of spec we expect Compile to reject:
// malformed CEL, dual/missing resource-id forms on permission checks, and
// uncompilable CEL in a permissionVariant.when. Each row asserts Compile
// returns an error whose message contains every required substring.
func TestCompile_Error(t *testing.T) {
	cases := []struct {
		name     string
		build    func(t *testing.T) *Spec
		wantSubs []string
	}{
		{
			name: "malformed CEL at tools[0].constraints[0]: syntax error path reported",
			build: func(t *testing.T) *Spec {
				return loadFile(t, "testdata/bad-cel.yaml")
			},
			wantSubs: []string{"tools[0]", "constraints[0]", "Syntax error"},
		},
		{
			name: "malformed CEL at non-zero indices: reports tools[1] / constraints[1]",
			build: func(t *testing.T) *Spec {
				return loadBytes(t, []byte(`
name: t
version: "1"
server: { url: http://x, transport: streamable-http }
tools:
  - name: ok
    args:
      constraints:
        - cel: 'args.x == 1'
  - name: bad
    args:
      constraints:
        - cel: 'args.y == 1'
        - cel: 'args.z (((  '   # syntax error in tools[1].args.constraints[1]
`))
			},
			wantSubs: []string{"tools[1]", "constraints[1]"},
		},
		{
			name: "permission.check sets both resourceIDTemplate and resourceIDExpr",
			build: func(*testing.T) *Spec {
				return &Spec{Tools: []Tool{{
					Name: "t", Args: Args{AllowedFields: []string{"a"}},
					Permission: &authz.Permission{
						StateImpact: authz.Readonly,
						Check: &authz.PermissionCheck{
							ResourceType: "x", Permission: "read",
							ResourceIDTemplate: "{a}", ResourceIDExpr: "args.a",
						},
					},
				}}}
			},
			wantSubs: []string{"exactly one of resourceIDTemplate or resourceIDExpr"},
		},
		{
			name: "permission.check sets neither resourceIDTemplate nor resourceIDExpr",
			build: func(*testing.T) *Spec {
				return &Spec{Tools: []Tool{{
					Name: "t", Args: Args{AllowedFields: []string{"a"}},
					Permission: &authz.Permission{
						StateImpact: authz.Readonly,
						Check: &authz.PermissionCheck{
							ResourceType: "x", Permission: "read",
						},
					},
				}}}
			},
			wantSubs: []string{"exactly one of resourceIDTemplate or resourceIDExpr"},
		},
		{
			name: "permissionVariants[0].when is not valid CEL",
			build: func(*testing.T) *Spec {
				return &Spec{Tools: []Tool{{
					Name: "t", Args: Args{AllowedFields: []string{"objectType"}},
					PermissionVariants: []authz.PermissionVariant{{
						When: "this is not valid CEL %%%",
						Check: authz.Permission{
							StateImpact: authz.Readonly,
							Check: &authz.PermissionCheck{
								ResourceType: "x", ResourceIDExpr: "args.objectType", Permission: "read",
							},
						},
					}},
				}}}
			},
			wantSubs: []string{"permissionVariants[0].when"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.build(t))
			require.Error(t, err, "Compile must return error")
			for _, want := range tc.wantSubs {
				assert.Contains(t, err.Error(), want, "Compile err should mention %q", want)
			}
		})
	}
}

// TestCompile_WarnsOnConstraintWithoutMessage covers the CEL-without-
// message lint: introspect_tool surfaces Constraint.Message as the
// agent-facing justification of a CEL gate, so a CEL-without-message
// constraint degrades to a generic "an additional constraint applies"
// line. Compile emits a non-fatal warning (it must NOT fail).
func TestCompile_WarnsOnConstraintWithoutMessage(t *testing.T) {
	cases := []struct {
		name        string
		constraints []toolspec.Constraint
		wantWarn    bool
	}{
		{
			name:        "CEL set, message empty: warns",
			constraints: []toolspec.Constraint{{CEL: "args.x > 0", Message: ""}},
			wantWarn:    true,
		},
		{
			name:        "CEL set, message present: no warning",
			constraints: []toolspec.Constraint{{CEL: "args.x > 0", Message: "x must be positive"}},
			wantWarn:    false,
		},
		{
			name:        "CEL set, message is only whitespace: warns",
			constraints: []toolspec.Constraint{{CEL: "args.x > 0", Message: "   "}},
			wantWarn:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &Spec{
				Name:    "x",
				Version: "1",
				Server:  Server{URL: "https://e.x/mcp", Transport: TransportStreamableHTTP},
				Tools: []Tool{{
					Name: "t",
					Args: Args{Constraints: tc.constraints},
				}},
			}
			res, err := Compile(sp)
			require.NoError(t, err, "Compile must succeed — the lint is non-fatal")

			var msgFound bool
			for _, w := range res.Warnings {
				lower := strings.ToLower(w.Message)
				if strings.Contains(lower, "constraint") &&
					(strings.Contains(lower, "message") || strings.Contains(lower, "justification")) {
					msgFound = true
					break
				}
			}
			if tc.wantWarn {
				assert.True(t, msgFound,
					"expected a constraint/message warning; got %+v", res.Warnings)
			} else {
				assert.False(t, msgFound,
					"expected no constraint/message warning; got %+v", res.Warnings)
			}
		})
	}
}

func TestCompile_WarnsOnUnenforceableDenyEffects(t *testing.T) {
	sp := &Spec{
		Name:    "x",
		Version: "1",
		Server:  Server{URL: "https://e.x/mcp", Transport: TransportStreamableHTTP},
		Tools: []Tool{
			{
				Name: "t",
				Deny: Deny{
					Effects: toolspec.DenyEffects{
						Writes: []string{"network"}, // no MCP analog → warning
					},
				},
			},
		},
	}
	res, err := Compile(sp)
	require.NoError(t, err, "Compile")
	require.NotEmpty(t, res.Warnings, "expected at least one warning")

	var messages []string
	for _, w := range res.Warnings {
		messages = append(messages, w.Message)
	}
	containsWrites := false
	for _, m := range messages {
		if strings.Contains(m, "Writes") {
			containsWrites = true
			break
		}
	}
	assert.True(t, containsWrites, "expected a warning mentioning Writes; got %+v", res.Warnings)
}
