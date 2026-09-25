package authz

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustCompile compiles expr and fails the test immediately if it doesn't —
// for building the fixed program TestEvalTriggerInstance's cases share.
func mustCompile(t *testing.T, expr string) cel.Program {
	t.Helper()
	prg, err := CompileTriggerInstanceExpr(expr)
	require.NoError(t, err, "compile %q", expr)
	return prg
}

func TestCompileTriggerInstanceExpr(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"payload field path compiles", `payload.pull_request.node_id`, false},
		{"event guard compiles", `event == "pull_request" ? payload.pull_request.node_id : ""`, false},
		{"unknown root refused", `args.argv[0]`, true},
		{"syntax error refused", `payload.(`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileTriggerInstanceExpr(tc.expr)
			if tc.wantErr {
				require.Error(t, err, "expected a compile error for %q", tc.expr)
				return
			}
			require.NoError(t, err, "expected %q to compile", tc.expr)
		})
	}
}

func TestEvalTriggerInstance(t *testing.T) {
	prg := mustCompile(t, `payload.pull_request.node_id`)

	cases := []struct {
		name    string
		payload string
		want    string
		wantErr bool
	}{
		{"present", `{"pull_request":{"node_id":"PR_kw1"}}`, "PR_kw1", false},
		{"missing key fails closed", `{"pull_request":{}}`, "", true},
		{"non-string fails closed", `{"pull_request":{"node_id":7}}`, "", true},
		{"empty string fails closed", `{"pull_request":{"node_id":""}}`, "", true},
		{"unparseable payload fails closed", `{`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalTriggerInstance(prg, "pull_request", []byte(tc.payload))
			if tc.wantErr {
				require.Error(t, err, "expected a fail-closed error for payload %s", tc.payload)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err, "expected payload %s to evaluate", tc.payload)
			assert.Equal(t, tc.want, got)
		})
	}
}
