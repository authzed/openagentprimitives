package manifests

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// These tests evaluate the CEL that ships in the install bundle — the exact
// strings the apiserver will run — rather than a Go re-statement of it. They
// bind every variable as dyn, which is enough to exercise presence (has()) and
// value logic; the apiserver's typed environment is exercised by the envtest
// suites (TestAgentSessionForkedFromImmutableCEL in pkg/apis/v1alpha1 and
// TestAgentSessionFinalizerPolicy in this package).

// evalBool compiles expr against dyn-typed vars and evaluates it.
func evalBool(t *testing.T, expr string, vars map[string]any) bool {
	t.Helper()
	opts := make([]cel.EnvOption, 0, len(vars))
	for name := range vars {
		opts = append(opts, cel.Variable(name, cel.DynType))
	}
	env, err := cel.NewEnv(opts...)
	require.NoError(t, err, "build CEL env")
	ast, iss := env.Compile(expr)
	require.NoError(t, iss.Err(), "compile %q", expr)
	prg, err := env.Program(ast)
	require.NoError(t, err, "program %q", expr)
	out, _, err := prg.Eval(vars)
	require.NoError(t, err, "eval %q", expr)
	b, ok := out.Value().(bool)
	require.True(t, ok, "%q must evaluate to bool, got %T", expr, out.Value())
	return b
}

// bundledDoc returns the one bundled document of kind with name.
func bundledDoc(t *testing.T, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, d := range allBundledDocs(t) {
		if d.GetKind() == kind && d.GetName() == name {
			return d
		}
	}
	require.Failf(t, "missing bundled doc", "%s %s is not in the install bundle", kind, name)
	return nil
}

// TestAgentSessionForkedFromRuleRefusesAnyChange pins spec.forkedFrom's
// spec-level transition rule: it may be neither added, removed nor repointed
// after create. A field-level rule could not catch add/remove (it never runs
// when the field is absent on one side), which is why the rule lives on spec.
func TestAgentSessionForkedFromRuleRefusesAnyChange(t *testing.T) {
	var rule string
	for _, r := range validationRules(t, bundledDoc(t, "CustomResourceDefinition", "agentsessions.agentprimitives.authzed.com")) {
		if strings.Contains(r.expr, "forkedFrom") {
			require.Empty(t, rule, "expected exactly one forkedFrom rule, found a second at %s", r.path)
			rule = r.expr
		}
	}
	require.NotEmpty(t, rule, "the AgentSession CRD carries no spec.forkedFrom rule")

	none := map[string]any{"class": "demo-agent"}
	forkA := map[string]any{"class": "demo-agent", "forkedFrom": "session-a"}
	forkB := map[string]any{"class": "demo-agent", "forkedFrom": "session-b"}

	cases := []struct {
		name     string
		old, new map[string]any
		want     bool
	}{
		{name: "unset to unset: allowed", old: none, new: none, want: true},
		{name: "A to A: allowed (a re-apply is a no-op)", old: forkA, new: forkA, want: true},
		{name: "unset to A: refused (adding after create)", old: none, new: forkA, want: false},
		{name: "A to unset: refused (removing)", old: forkA, new: none, want: false},
		{name: "A to B: refused (repointing)", old: forkA, new: forkB, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evalBool(t, rule, map[string]any{"self": tc.new, "oldSelf": tc.old}))
		})
	}
}

// TestAgentSessionFinalizerPolicyCEL pins the shipped finalizer admission
// policy's logic: its matchCondition selects exactly the writes that ADD the
// AgentSession finalizer, and its validation admits only the operator and
// channelsd service accounts.
func TestAgentSessionFinalizerPolicyCEL(t *testing.T) {
	const name = "agentsession-finalizer.agentprimitives.authzed.com"
	pol := bundledDoc(t, "ValidatingAdmissionPolicy", name)
	binding := bundledDoc(t, "ValidatingAdmissionPolicyBinding", name)

	policyName, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName")
	assert.Equal(t, name, policyName, "the binding must bind this policy")
	actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	assert.Equal(t, []string{"Deny"}, actions, "the binding must deny, not merely warn or audit")
	failurePolicy, _, _ := unstructured.NestedString(pol.Object, "spec", "failurePolicy")
	assert.Equal(t, "Fail", failurePolicy, "the policy must fail closed")
	assert.Empty(t, pol.GetNamespace(), "a ValidatingAdmissionPolicy is cluster-scoped")
	assert.Empty(t, binding.GetNamespace(), "a ValidatingAdmissionPolicyBinding is cluster-scoped")

	conds, _, err := unstructured.NestedSlice(pol.Object, "spec", "matchConditions")
	require.NoError(t, err)
	require.Len(t, conds, 1, "one matchCondition: the write adds the finalizer")
	match := conds[0].(map[string]any)["expression"].(string)
	vals, _, err := unstructured.NestedSlice(pol.Object, "spec", "validations")
	require.NoError(t, err)
	require.Len(t, vals, 1, "one validation: the caller is a trusted service account")
	validation := vals[0].(map[string]any)["expression"].(string)

	const fin = "agentsession.agentprimitives.authzed.com/finalizer"
	obj := func(finalizers ...any) map[string]any {
		md := map[string]any{"name": "s"}
		if finalizers != nil {
			md["finalizers"] = finalizers
		}
		return map[string]any{"metadata": md}
	}
	req := func(op string) map[string]any {
		return map[string]any{"operation": op, "userInfo": map[string]any{"username": "alice"}}
	}

	matchCases := []struct {
		name   string
		op     string
		object map[string]any
		old    any
		want   bool
	}{
		{name: "CREATE with the finalizer: evaluated", op: "CREATE", object: obj(fin), old: nil, want: true},
		{name: "CREATE with only another finalizer: not evaluated", op: "CREATE", object: obj("other/finalizer"), old: nil, want: false},
		{name: "CREATE with no finalizers: not evaluated", op: "CREATE", object: obj(), old: nil, want: false},
		{name: "UPDATE adding it to none: evaluated", op: "UPDATE", object: obj(fin), old: obj(), want: true},
		{name: "UPDATE adding it beside another: evaluated", op: "UPDATE", object: obj("other/finalizer", fin), old: obj("other/finalizer"), want: true},
		{name: "UPDATE keeping it: not evaluated", op: "UPDATE", object: obj(fin), old: obj(fin), want: false},
		{name: "UPDATE removing it: not evaluated", op: "UPDATE", object: obj(), old: obj(fin), want: false},
	}
	for _, tc := range matchCases {
		t.Run("match: "+tc.name, func(t *testing.T) {
			got := evalBool(t, match, map[string]any{"object": tc.object, "oldObject": tc.old, "request": req(tc.op)})
			assert.Equal(t, tc.want, got)
		})
	}

	userCases := []struct {
		name     string
		username string
		want     bool
	}{
		{name: "operator SA: admitted", username: "system:serviceaccount:agentprimitives-system:spicebox-operator", want: true},
		{name: "channelsd SA: admitted", username: "system:serviceaccount:agentprimitives-system:spicebox-channelsd", want: true},
		{name: "a human user: refused", username: "alice", want: false},
		{name: "a same-named SA in another namespace: refused", username: "system:serviceaccount:team-a:spicebox-operator", want: false},
		{name: "webd SA: refused", username: "system:serviceaccount:agentprimitives-system:spicebox-webd", want: false},
	}
	for _, tc := range userCases {
		t.Run("validation: "+tc.name, func(t *testing.T) {
			r := map[string]any{"operation": "CREATE", "userInfo": map[string]any{"username": tc.username}}
			assert.Equal(t, tc.want, evalBool(t, validation, map[string]any{"request": r}))
		})
	}
}
