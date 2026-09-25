package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// specWithConstraint builds a one-tool spec whose single CEL constraint is
// cel, so a test can choose between a rule that evaluates and one that cannot.
func specWithConstraint(t *testing.T, cel string) *mcpspec.Spec {
	t.Helper()
	return &mcpspec.Spec{
		Name: "crm-tools",
		Tools: []mcpspec.Tool{{
			Name: "search_records",
			Args: mcpspec.Args{
				AllowedFields: []string{"from"},
				Constraints:   []toolspec.Constraint{{CEL: cel, Message: "window too wide"}},
			},
		}},
	}
}

func evalTrust(t *testing.T, spec *mcpspec.Spec, args map[string]any) pipeline.Decision {
	t.Helper()
	inner, err := json.Marshal(args)
	require.NoError(t, err, "marshal inner args")
	env, err := json.Marshal(map[string]any{"operation_id": "op", "args": json.RawMessage(inner)})
	require.NoError(t, err, "marshal args envelope")

	h := NewMcpTrust(func(string) (*mcpspec.Spec, string) { return spec, "search_records" })
	return h.Eval(context.Background(), pipeline.Input{
		Tool: &pipeline.ToolCallInfo{Name: "crm_search_records", Args: env},
	})
}

// The distinction this whole path exists for. Both cases deny and both must —
// an unevaluatable gate has to be treated as a gate that said no — but only
// one of them is the CALLER's problem. Conflating them told a viewer they
// lacked access to data that no one could load, and told nobody who could fix
// it.
func TestMcpTrustSeparatesBrokenRulesFromRefusals(t *testing.T) {
	cases := []struct {
		name    string
		cel     string
		args    map[string]any
		wantDef bool
		// wantLocus/wantDetail are checked only when wantDef is true.
		wantLocus  string
		wantDetail string
	}{
		{
			// The production fault: the rule compiles, and errors only when a
			// real argument reaches it. No reconcile can see this — it needs a
			// live call with a live value.
			name:       "rule that errors on a real argument: Definition set, carrying locus and expression",
			cel:        `timestamp(args.from) >= timestamp("2020-01-01T00:00:00Z")`,
			args:       map[string]any{"from": "2026-02-01"},
			wantDef:    true,
			wantLocus:  "constraints[0]",
			wantDetail: `timestamp(args.from) >= timestamp("2020-01-01T00:00:00Z")`,
		},
		{
			name:       "rule that does not compile: Definition set",
			cel:        `args.from >>> 3`,
			args:       map[string]any{"from": "x"},
			wantDef:    true,
			wantLocus:  "constraints[0]",
			wantDetail: `args.from >>> 3`,
		},
		{
			name:       "rule that evaluates to a non-bool: Definition set",
			cel:        `args.from`,
			args:       map[string]any{"from": "x"},
			wantDef:    true,
			wantLocus:  "constraints[0]",
			wantDetail: `args.from`,
		},
		{
			// The policy working exactly as written. This one IS about the
			// caller, and must stay a plain refusal — marking it as a broken
			// definition would page an operator every time a rule did its job.
			name:    "rule that evaluates to false: an ordinary refusal, Definition nil",
			cel:     `args.from == "allowed"`,
			args:    map[string]any{"from": "denied"},
			wantDef: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := evalTrust(t, specWithConstraint(t, tc.cel), tc.args)
			require.Equal(t, pipeline.Deny, dec.Verdict, "every case here must fail closed")

			if !tc.wantDef {
				assert.Nil(t, dec.Definition, "a refusal is not a broken definition")
				return
			}

			require.NotNil(t, dec.Definition, "an unevaluatable rule must be marked as a definition fault")
			var de *pipeline.DefinitionError
			require.True(t, errors.As(dec.Definition, &de), "must carry the pipeline's operator-facing shape")
			assert.Equal(t, tc.wantLocus, de.Locus, "the locus is how an operator finds the rule")
			assert.Equal(t, tc.wantDetail, de.Detail, "the authored expression is what they search for")
			assert.Contains(t, de.Subject, "crm_search_records", "subject names the tool that is unusable")
			assert.Contains(t, de.Subject, "crm-tools", "subject names the server whose spec holds it")
			assert.NotNil(t, de.Err, "the underlying cause must survive, not be flattened to prose")
		})
	}
}

// Decision.Definition is an interface, so a hook that assigned a typed-nil
// pointer would produce a non-nil error that classifies every refusal as
// broken configuration and then panics on the first field read (AGENTS.md,
// "Nil interfaces"). The refusal case above covers the behaviour; this pins
// the representation, which is the part a future edit can quietly break.
func TestMcpTrustRefusalCarriesATrueNilDefinition(t *testing.T) {
	dec := evalTrust(t, specWithConstraint(t, `args.from == "allowed"`), map[string]any{"from": "denied"})
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.True(t, dec.Definition == nil, "must be a genuine nil interface, not a typed-nil pointer")
}
