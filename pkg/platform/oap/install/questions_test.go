package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

func boolPtr(b bool) *bool { return &b }

func TestResolve_SetFillsStringAnswer(t *testing.T) {
	qs := []oap.Question{
		{Name: "foo", Type: oap.QString, Prompt: "Foo", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.foo"}}},
	}

	ans, secrets, err := Resolve(qs, "", map[string]string{"foo": "bar"}, false)
	require.NoError(t, err)
	assert.Equal(t, "bar", ans["foo"])
	assert.Empty(t, secrets)
}

func TestResolve_SetCoercesIntAnswer(t *testing.T) {
	qs := []oap.Question{
		{Name: "n", Type: oap.QInt, Prompt: "N", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.n"}}},
	}

	ans, _, err := Resolve(qs, "", map[string]string{"n": "3"}, false)
	require.NoError(t, err)
	assert.Equal(t, int64(3), ans["n"])
}

func TestResolve_SetCoercesBoolAnswer(t *testing.T) {
	qs := []oap.Question{
		{Name: "flag", Type: oap.QBool, Prompt: "Flag", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.flag"}}},
	}

	ans, _, err := Resolve(qs, "", map[string]string{"flag": "true"}, false)
	require.NoError(t, err)
	assert.Equal(t, true, ans["flag"])
}

// TestResolve_AnswerTypeIsTheQuestionsNotThePaths pins the property that makes
// the three answer sources interchangeable: the Go type of an answer is decided
// by the question's declared Type, never by which source supplied it.
//
// It is the join no single-source test can see. A QInt answered by --set landed
// as int64 and applied; the same answer typed at the prompt landed as a string
// and the apply was REJECTED on the integer field, because only --set ran
// through the coercion. --values is the third path with the same shape (YAML
// decoding yields float64 for a number, which nothing converted either).
func TestResolve_AnswerTypeIsTheQuestionsNotThePaths(t *testing.T) {
	binding := []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}}

	viaSet := func(v string) func(*testing.T, oap.Question) (oap.Answers, error) {
		return func(t *testing.T, q oap.Question) (oap.Answers, error) {
			t.Helper()
			ans, _, err := Resolve([]oap.Question{q}, "", map[string]string{q.Name: v}, false)
			return ans, err
		}
	}
	viaValues := func(doc string) func(*testing.T, oap.Question) (oap.Answers, error) {
		return func(t *testing.T, q oap.Question) (oap.Answers, error) {
			t.Helper()
			path := filepath.Join(t.TempDir(), "values.yaml")
			require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
			ans, _, err := Resolve([]oap.Question{q}, path, nil, false)
			return ans, err
		}
	}
	viaPrompt := func(script string) func(*testing.T, oap.Question) (oap.Answers, error) {
		return func(t *testing.T, q oap.Question) (oap.Answers, error) {
			t.Helper()
			present, _ := scriptedPresentation(t, script)
			ans, _, err := Resolve([]oap.Question{q}, "", nil, true, present)
			return ans, err
		}
	}

	intQ := oap.Question{Name: "maxTurns", Type: oap.QInt, Prompt: "Max turns", Binding: binding}
	boolQ := oap.Question{Name: "verbose", Type: oap.QBool, Prompt: "Verbose", Binding: binding}

	cases := []struct {
		name   string
		q      oap.Question
		answer func(*testing.T, oap.Question) (oap.Answers, error)
		want   any
	}{
		{name: "int via --set: int64", q: intQ, answer: viaSet("5"), want: int64(5)},
		{name: "int via --values: int64", q: intQ, answer: viaValues("maxTurns: 5\n"), want: int64(5)},
		{name: "int typed at the prompt: int64, not the string the widget read", q: intQ, answer: viaPrompt("5\n"), want: int64(5)},
		{name: "bool via --set: bool", q: boolQ, answer: viaSet("true"), want: true},
		{name: "bool via --values: bool", q: boolQ, answer: viaValues("verbose: true\n"), want: true},
		{name: "bool typed at the prompt: bool, not the string the widget read", q: boolQ, answer: viaPrompt("true\n"), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ans, err := tc.answer(t, tc.q)
			require.NoError(t, err, "Resolve")
			assert.Equal(t, tc.want, ans[tc.q.Name], "the answer's Go type must come from the question, not the source")
		})
	}
}

func TestResolve_ValidationRejectsBadValue(t *testing.T) {
	qs := []oap.Question{
		{
			Name:       "n",
			Type:       oap.QInt,
			Prompt:     "N",
			Binding:    []oap.Binding{{Target: "AgentClass/demo-agent#spec.n"}},
			Validation: "args.n > 0",
		},
	}

	_, _, err := Resolve(qs, "", map[string]string{"n": "-1"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `question "n"`)
	assert.Contains(t, err.Error(), "args.n > 0")
}

func TestResolve_ValidationAcceptsGoodValue(t *testing.T) {
	qs := []oap.Question{
		{
			Name:       "n",
			Type:       oap.QInt,
			Prompt:     "N",
			Binding:    []oap.Binding{{Target: "AgentClass/demo-agent#spec.n"}},
			Validation: "args.n > 0",
		},
	}

	ans, _, err := Resolve(qs, "", map[string]string{"n": "3"}, false)
	require.NoError(t, err)
	assert.Equal(t, int64(3), ans["n"])
}

func TestResolve_SecretAnswerSplitsIntoSecretSpec_AbsentFromAnswers(t *testing.T) {
	qs := []oap.Question{
		{
			Name:   "apiToken",
			Type:   oap.QSecret,
			Prompt: "API token",
			Secret: &oap.SecretQuestion{
				CreateSecret: &oap.SecretTarget{Name: "demo-agent-token", Key: "token"},
			},
		},
	}

	ans, secrets, err := Resolve(qs, "", map[string]string{"apiToken": "super-secret-value"}, false)
	require.NoError(t, err)

	_, present := ans["apiToken"]
	assert.False(t, present, "secret answer must not appear in Answers")

	require.Len(t, secrets, 1)
	assert.Equal(t, SecretSpec{Name: "demo-agent-token", Key: "token", Value: "super-secret-value"}, secrets[0])
}

// TestResolve_SecretAnswerWithNoCreateSecretTarget_IsRefused pins the rule that
// a secret answer Resolve cannot materialize is refused, not dropped.
//
// A secret question declaring only `orExisting` asks for a value and has
// nowhere to put it: no SecretSpec can be built, and a secret answer never
// reaches oap.Answers either. Silently continuing means the operator types
// their credential, the install reports success, and the applied CRs reference
// a Secret nothing ever created — the failure only surfaces later as an
// unresolvable credential. Refusing names the manifest bug at the one moment
// somebody can still act on it.
func TestResolve_SecretAnswerWithNoCreateSecretTarget_IsRefused(t *testing.T) {
	const typed = "sk-live-do-not-print-me"
	qs := []oap.Question{{
		Name:   "widgetToken",
		Type:   oap.QSecret,
		Prompt: "Widget API token",
		Secret: &oap.SecretQuestion{OrExisting: true},
	}}

	ans, secrets, err := Resolve(qs, "", map[string]string{"widgetToken": typed}, false)

	require.Error(t, err, "an answered secret question with nowhere to put the value must not resolve")
	assert.Contains(t, err.Error(), `question "widgetToken"`, "the error must name the offending question")
	assert.NotContains(t, err.Error(), typed, "the error must never echo the typed credential")
	assert.Empty(t, secrets, "nothing to create")
	assert.NotContains(t, ans, "widgetToken", "a secret answer never belongs in Answers")
}

func TestResolve_SecretAnswerFromValuesFile_SplitsOut(t *testing.T) {
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("apiToken: from-values-file\n"), 0o600))

	qs := []oap.Question{
		{
			Name:   "apiToken",
			Type:   oap.QSecret,
			Prompt: "API token",
			Secret: &oap.SecretQuestion{
				CreateSecret: &oap.SecretTarget{Name: "demo-agent-token", Key: "token"},
			},
		},
	}

	ans, secrets, err := Resolve(qs, valuesPath, nil, false)
	require.NoError(t, err)
	assert.NotContains(t, ans, "apiToken")
	require.Len(t, secrets, 1)
	assert.Equal(t, "from-values-file", secrets[0].Value)
}

// TestResolve_ValidationErrorsNeverEchoASecretAnswer covers the window in which
// a secret answer is still plaintext: Resolve validates over the raw answer map
// and only afterwards splits QSecret answers into SecretSpecs, so every
// validation error in between is emitted with the secret in scope.
//
// The bundle controls both halves of that window. It authors the questions AND
// the CEL rules, so it chooses which answer a rule reads; a rule may name a
// secret question, and `enum` is accepted on a secret question. An error that
// echoes the value it rejected therefore hands the bundle a printf of the
// user's credential — and both sinks are unmasked: `oap` writes the error to
// stderr, admind puts it in the 400 body.
//
// Redaction is deliberately scoped to secret answers. The value is the most
// useful thing an install error can name for an ordinary typo, and
// TestResolve_EnumSingle_RejectsOutOfEnum / TestResolve_EnumList_RejectsElement
// OutOfEnum pin that non-secret answers keep naming it.
func TestResolve_ValidationErrorsNeverEchoASecretAnswer(t *testing.T) {
	const secret = "sk-live-do-not-print-me"

	secretQuestion := func(t *testing.T, mutate func(*oap.Question)) []oap.Question {
		t.Helper()
		q := oap.Question{
			Name:   "apiToken",
			Type:   oap.QSecret,
			Prompt: "API token",
			Secret: &oap.SecretQuestion{
				CreateSecret: &oap.SecretTarget{Name: "demo-agent-token", Key: "token"},
			},
		}
		mutate(&q)
		return []oap.Question{q}
	}

	cases := []struct {
		name    string
		mutate  func(*oap.Question)
		wantSay string
	}{
		{
			name:    "quantity() over a secret answer: error names the failure, never the token",
			mutate:  func(q *oap.Question) { q.Validation = "quantity(args.apiToken) > 0" },
			wantSay: "quantity",
		},
		{
			name:    "enum declared on a secret question: rejection names the question, never the token",
			mutate:  func(q *oap.Question) { q.Enum = []string{"nope"} },
			wantSay: `question "apiToken"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Resolve(secretQuestion(t, tc.mutate), "", map[string]string{"apiToken": secret}, false)

			require.Error(t, err, "the answer does not satisfy the rule; Resolve must still refuse it")
			assert.NotContains(t, err.Error(), secret, "the plaintext secret answer must not reach the error text")
			assert.Contains(t, err.Error(), tc.wantSay, "the error must still say what was wrong")
		})
	}
}

func TestResolve_NonInteractiveMissingRequired_Errors(t *testing.T) {
	qs := []oap.Question{
		{Name: "required1", Type: oap.QString, Prompt: "Required One", Required: boolPtr(true), Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.r1"}}},
		{Name: "optional1", Type: oap.QString, Prompt: "Optional One", Required: boolPtr(false), Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.o1"}}},
	}

	ans, secrets, err := Resolve(qs, "", nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required1")
	assert.NotContains(t, err.Error(), "optional1")
	assert.Nil(t, ans)
	assert.Nil(t, secrets)
}

func TestResolve_UnknownSetKey_Errors(t *testing.T) {
	qs := []oap.Question{
		{Name: "foo", Type: oap.QString, Prompt: "Foo", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.foo"}}},
	}

	// "fo" is a typo of "foo": it names no question, so it must be rejected
	// rather than silently no-op'd (leaving foo unanswered with wrong config).
	ans, secrets, err := Resolve(qs, "", map[string]string{"foo": "ok", "fo": "typo"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--set fo")
	assert.NotContains(t, err.Error(), "--set foo", "the matching key must not be flagged")
	assert.Nil(t, ans)
	assert.Nil(t, secrets)
}

func TestResolve_UnknownValuesKey_Errors(t *testing.T) {
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("foo: ok\nbogus: stray\n"), 0o600))

	qs := []oap.Question{
		{Name: "foo", Type: oap.QString, Prompt: "Foo", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.foo"}}},
	}

	_, _, err := Resolve(qs, valuesPath, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--values bogus")
}

func TestResolve_NonInteractiveRequiredWithDefault_UsesDefault(t *testing.T) {
	qs := []oap.Question{
		// Required (IsRequired defaults true) but carries a manifest Default: it
		// must resolve non-interactively from the Default, not hard-error missing.
		{
			Name:    "repos",
			Type:    oap.QResourceList,
			Prompt:  "Repos",
			Default: []any{"demo-org/demo-repo"},
			Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.boundEntities[github_repo].defaults"}},
		},
	}

	ans, _, err := Resolve(qs, "", nil, false)
	require.NoError(t, err, "a required question WITH a Default must resolve non-interactively")
	assert.Equal(t, []any{"demo-org/demo-repo"}, ans["repos"], "the manifest Default must fill the unanswered question")
}

func TestResolve_EnumSingle_AcceptsInEnum(t *testing.T) {
	qs := []oap.Question{
		{Name: "mode", Type: oap.QEnum, Prompt: "Mode", Enum: []string{"fast", "safe"}, Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.mode"}}},
	}
	ans, _, err := Resolve(qs, "", map[string]string{"mode": "fast"}, false)
	require.NoError(t, err)
	assert.Equal(t, "fast", ans["mode"])
}

func TestResolve_EnumSingle_RejectsOutOfEnum(t *testing.T) {
	qs := []oap.Question{
		{Name: "mode", Type: oap.QEnum, Prompt: "Mode", Enum: []string{"fast", "safe"}, Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.mode"}}},
	}
	_, _, err := Resolve(qs, "", map[string]string{"mode": "wobble"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `question "mode"`)
	assert.Contains(t, err.Error(), "wobble", "the rejected value must be named")
}

func TestResolve_EnumList_AcceptsAllInEnum(t *testing.T) {
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("toolchains: [go, node]\n"), 0o600))
	qs := []oap.Question{
		{Name: "toolchains", Type: oap.QResourceList, Prompt: "Toolchains", Enum: []string{"go", "node"}, Binding: []oap.Binding{{Target: "SpiceboxClass/codelike-bundle#spec.toolchains"}}},
	}
	ans, _, err := Resolve(qs, valuesPath, nil, false)
	require.NoError(t, err)
	assert.Equal(t, []any{"go", "node"}, ans["toolchains"])
}

func TestResolve_EnumList_RejectsElementOutOfEnum(t *testing.T) {
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("toolchains: [go, ruby]\n"), 0o600))
	qs := []oap.Question{
		{Name: "toolchains", Type: oap.QResourceList, Prompt: "Toolchains", Enum: []string{"go", "node"}, Binding: []oap.Binding{{Target: "SpiceboxClass/codelike-bundle#spec.toolchains"}}},
	}
	_, _, err := Resolve(qs, valuesPath, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `question "toolchains"`)
	assert.Contains(t, err.Error(), "ruby", "the offending element must be named")
}

func TestResolve_EnumList_DefaultUsedAndValidated(t *testing.T) {
	qs := []oap.Question{
		{
			Name:    "toolchains",
			Type:    oap.QResourceList,
			Prompt:  "Toolchains",
			Enum:    []string{"go", "node"},
			Default: []any{"go", "node"},
			Binding: []oap.Binding{{Target: "SpiceboxClass/codelike-bundle#spec.toolchains"}},
		},
	}
	ans, _, err := Resolve(qs, "", nil, false)
	require.NoError(t, err, "the enum-valid manifest Default must resolve non-interactively")
	assert.Equal(t, []any{"go", "node"}, ans["toolchains"])
}

func TestResolve_ResourceListNoEnum_SkipsEnumCheck(t *testing.T) {
	// A free-form resourceList (e.g. repos) has no Enum, so any value is accepted.
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("repos: [authzed/anything]\n"), 0o600))
	qs := []oap.Question{
		{Name: "repos", Type: oap.QResourceList, Prompt: "Repos", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.repos"}}},
	}
	ans, _, err := Resolve(qs, valuesPath, nil, false)
	require.NoError(t, err)
	assert.Equal(t, []any{"authzed/anything"}, ans["repos"])
}

func TestIsEmptyAnswer(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{name: "empty string is empty", v: "", want: true},
		{name: "nil is empty", v: nil, want: true},
		{name: "empty []string is empty", v: []string{}, want: true},
		{name: "empty []any is empty", v: []any{}, want: true},
		{name: "non-empty string is not empty", v: "go", want: false},
		{name: "non-empty []string is not empty", v: []string{"go"}, want: false},
		{name: "non-empty []any is not empty", v: []any{"go"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isEmptyAnswer(tc.v))
		})
	}
}

func TestResolve_ValuesFileSeedsThenSetOverrides(t *testing.T) {
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("foo: from-file\n"), 0o600))

	qs := []oap.Question{
		{Name: "foo", Type: oap.QString, Prompt: "Foo", Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.foo"}}},
	}

	ans, _, err := Resolve(qs, valuesPath, map[string]string{"foo": "from-set"}, false)
	require.NoError(t, err)
	assert.Equal(t, "from-set", ans["foo"], "--set must win over --values")
}

func TestEvalValidation_Quantity(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		args    map[string]any
		want    bool
		wantErr bool
	}{
		{
			name: "in range: passes",
			expr: `quantity(args['capacity.test-sandbox.memory']) >= quantity('1280Mi') && quantity(args['capacity.test-sandbox.memory']) <= quantity('3800Mi')`,
			args: map[string]any{"capacity.test-sandbox.memory": "1792Mi"},
			want: true,
		},
		{
			name: "below the floor: fails",
			expr: `quantity(args['capacity.test-sandbox.memory']) >= quantity('1280Mi')`,
			args: map[string]any{"capacity.test-sandbox.memory": "300Mi"},
			want: false,
		},
		{
			name: "above the ceiling: fails",
			expr: `quantity(args['capacity.test-sandbox.memory']) <= quantity('3800Mi')`,
			args: map[string]any{"capacity.test-sandbox.memory": "8Gi"},
			want: false,
		},
		{
			name: "mixed units compare correctly, not lexically",
			expr: `quantity('1Gi') > quantity('900Mi')`,
			args: map[string]any{},
			want: true,
		},
		{
			name: "fractional binary suffix parses",
			expr: `quantity('1.8Gi') > quantity('1792Mi')`,
			args: map[string]any{},
			want: true,
		},
		{
			name: "cpu millis compare correctly",
			expr: `quantity('500m') < quantity('1')`,
			args: map[string]any{},
			want: true,
		},
		{
			name:    "unparseable input is an ERROR, never a silent false",
			expr:    `quantity('not-a-quantity') > 0`,
			args:    map[string]any{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalValidation(tc.expr, tc.args)
			if tc.wantErr {
				require.Error(t, err, "an unparseable quantity must surface, not evaluate to false")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A pre-existing rule must keep working unchanged: adding a function to the env
// must not disturb expressions that never call it.
func TestEvalValidation_NonQuantityRulesUnaffected(t *testing.T) {
	ok, err := evalValidation(`args.count > 2`, map[string]any{"count": 5})
	require.NoError(t, err)
	assert.True(t, ok)
}

// scriptedPresentation returns the option an interactive Resolve is driven
// with in a test, reading script and rendering into the returned buffer.
//
// One driver for the whole run, which is the property the resolver depends
// on: huh builds a fresh greedy scanner per field, so a run that presented
// each question over its own driver would lose every answer after the first.
func scriptedPresentation(t *testing.T, script string) (ResolveOption, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	th := tui.NewTheme(tui.Caps{})
	return PresentOver(tui.Plain(strings.NewReader(script), &out, th), th), &out
}

// errDriver reports a presentation that could not run — a stand-in for a
// terminal that went away mid-question.
type errDriver struct{ err error }

func (d errDriver) Present(context.Context, string, *huh.Group) error { return d.err }

// TestResolve_Interactive_AnswersComeFromTheQuestionsAsked drives the real
// huh widgets for each question type over a scripted terminal.
//
// Every expected answer differs from what the widget would produce if it were
// never rendered: huh's accessible renderer answers a question it ran out of
// input for with that field's default and reports a nil error, so a case
// expecting the default would pass whether or not the question was asked.
func TestResolve_Interactive_AnswersComeFromTheQuestionsAsked(t *testing.T) {
	binding := []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}}
	cases := []struct {
		name   string
		q      oap.Question
		script string
		want   any
		// rendered is a fragment the widget only writes when it actually ran.
		rendered string
	}{
		{
			name:     "string question, typed over its default: the typed value wins",
			q:        oap.Question{Name: "memory", Type: oap.QString, Prompt: "Memory", Default: "1792Mi", Binding: binding},
			script:   "2048Mi\n",
			want:     "2048Mi",
			rendered: "Memory",
		},
		{
			name:     "enum question, third option chosen: that option is the answer",
			q:        oap.Question{Name: "tier", Type: oap.QEnum, Prompt: "Tier", Enum: []string{"alpha", "beta", "gamma"}, Binding: binding},
			script:   "3\n",
			want:     "gamma",
			rendered: "Tier",
		},
		{
			name:     "enum-constrained list, one option picked then finished: a one-element list",
			q:        oap.Question{Name: "addons", Type: oap.QResourceList, Prompt: "Addons", Enum: []string{"metrics", "tracing", "audit"}, Binding: binding},
			script:   "2\n0\n",
			want:     []string{"tracing"},
			rendered: "Addons",
		},
		{
			name:     "description question, answered: the description is shown above the field",
			q:        oap.Question{Name: "host", Type: oap.QString, Prompt: "Host", Description: "The instance this agent talks to.", Binding: binding},
			script:   "hub.example\n",
			want:     "hub.example",
			rendered: "The instance this agent talks to.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, out := scriptedPresentation(t, tc.script)
			ans, secrets, err := Resolve([]oap.Question{tc.q}, "", nil, true, present)
			require.NoError(t, err, "Resolve")
			assert.Empty(t, secrets, "no secret question in this case")
			assert.Equal(t, tc.want, ans[tc.q.Name], "answer")
			assert.Contains(t, out.String(), tc.rendered,
				"the question must actually have been rendered, not silently defaulted")
		})
	}
}

// TestResolve_Interactive_SecretAnswerBecomesASecretSpec covers the one
// question type whose answer never reaches oap.Answers.
func TestResolve_Interactive_SecretAnswerBecomesASecretSpec(t *testing.T) {
	qs := []oap.Question{{
		Name:   "token",
		Type:   oap.QSecret,
		Prompt: "API token",
		Secret: &oap.SecretQuestion{CreateSecret: &oap.SecretTarget{Name: "demo-token", Key: "token"}},
	}}
	present, out := scriptedPresentation(t, "typed-token-value\n")
	ans, secrets, err := Resolve(qs, "", nil, true, present)
	require.NoError(t, err, "Resolve")

	require.Len(t, secrets, 1, "one SecretSpec")
	assert.Equal(t, "typed-token-value", secrets[0].Value, "the typed value reaches the SecretSpec")
	assert.Equal(t, "demo-token", secrets[0].Name, "Secret name")
	assert.Equal(t, "token", secrets[0].Key, "Secret key")
	assert.NotContains(t, ans, "token", "a secret answer must never land in Answers")
	assert.Contains(t, out.String(), "API token", "the question must have been rendered")
}

// TestResolve_Interactive_OneDriverAnswersEveryQuestion is the join a
// per-question test cannot see.
//
// Three questions are answered from three lines of one stream. Presenting
// each over its own driver would let the first field's scanner swallow all
// three lines; the second and third would then read EOF and take their
// pre-fills, which huh's accessible renderer reports as completed forms with
// a nil error. Every expected answer therefore differs from its pre-fill.
func TestResolve_Interactive_OneDriverAnswersEveryQuestion(t *testing.T) {
	binding := []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}}
	qs := []oap.Question{
		{Name: "cpu", Type: oap.QString, Prompt: "CPU", Default: "500m", Binding: binding},
		{Name: "memory", Type: oap.QString, Prompt: "Memory", Default: "1792Mi", Binding: binding},
		{Name: "host", Type: oap.QString, Prompt: "Host", Default: "hub.invalid", Binding: binding},
	}
	present, _ := scriptedPresentation(t, "750m\n2048Mi\nhub.example\n")
	ans, _, err := Resolve(qs, "", nil, true, present)
	require.NoError(t, err, "Resolve")

	assert.Equal(t, "750m", ans["cpu"], "first question")
	assert.Equal(t, "2048Mi", ans["memory"], "second question read a drained stream")
	assert.Equal(t, "hub.example", ans["host"], "third question read a drained stream")
}

// TestResolve_Interactive_OptionalEmptyAnswerStaysUnanswered covers the rule
// that lets an optional question be declined: an empty answer records nothing
// rather than an empty value, so it neither overlays a CR field nor creates
// an empty Secret. The required question in the same run is what proves the
// run rendered at all.
func TestResolve_Interactive_OptionalEmptyAnswerStaysUnanswered(t *testing.T) {
	binding := []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}}
	qs := []oap.Question{
		{Name: "host", Type: oap.QString, Prompt: "Host", Default: "hub.invalid", Binding: binding},
		{Name: "label", Type: oap.QString, Prompt: "Label", Required: boolPtr(false), Binding: binding},
	}
	present, _ := scriptedPresentation(t, "hub.example\n\n")
	ans, _, err := Resolve(qs, "", nil, true, present)
	require.NoError(t, err, "Resolve")

	assert.Equal(t, "hub.example", ans["host"], "the required question was asked and answered")
	assert.NotContains(t, ans, "label", "an optional question answered with nothing stays unanswered")
}

// TestResolve_Interactive_RequiredQuestionRefusesABlankAnswer covers the gate
// deciding WHICH install questions accept nothing: Optional is wired from
// q.IsRequired(), so a required question with no Default refuses a blank rather
// than recording one and letting an empty value reach the resource.
//
// It also pins what that refusal costs a stream, which is not obvious and is
// the same mechanism that once put a threshold into a Secret's name: huh's
// accessible renderer re-prompts a rejected field by reading the NEXT line. So a
// blank answer to a required question is not an answer at all — the line after
// it answers that same question, and a later question can run out of input. A
// later REQUIRED question then fails the run closed; a later OPTIONAL one is
// simply left unanswered, the only silent outcome of the three and so the one
// that most needs a test standing over it.
func TestResolve_Interactive_RequiredQuestionRefusesABlankAnswer(t *testing.T) {
	binding := []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}}
	required := func(name, prompt string) oap.Question {
		return oap.Question{Name: name, Type: oap.QString, Prompt: prompt, Binding: binding}
	}
	optional := func(name, prompt string) oap.Question {
		return oap.Question{Name: name, Type: oap.QString, Prompt: prompt, Required: boolPtr(false), Binding: binding}
	}

	cases := []struct {
		name string
		qs   []oap.Question
		// script is what the operator types. Every expected answer below
		// differs from what an unrendered question produces — a required
		// question with no Default has no pre-fill, so "never asked" is the
		// refusal, never one of these values.
		script string
		// refusalIsRendered says the operator SAW the refusal and got another
		// go at the field. It is true exactly when a blank line was typed:
		// huh's accessible renderer runs the field validator on a typed line
		// and re-prompts when it fails, but on EOF it returns the bound value
		// with no error and no validator call at all — so a stream that simply
		// ran out is refused later, by the re-check in Apply, and its refusal
		// reaches the operator only as the returned error.
		refusalIsRendered bool
		wantErr           string
		wantAns           map[string]any
		absent            []string
	}{
		{
			name:    "input runs out on a required question: refused by Apply, naming the prompt",
			qs:      []oap.Question{required("cpu", "CPU")},
			script:  "",
			wantErr: "CPU: nothing was supplied",
		},
		{
			name:              "a blank to a required question is refused and re-asked: the next line answers it",
			qs:                []oap.Question{required("cpu", "CPU"), required("memory", "Memory")},
			script:            "\n750m\n2048Mi\n",
			refusalIsRendered: true,
			wantAns:           map[string]any{"cpu": "750m", "memory": "2048Mi"},
		},
		{
			name:              "the retry eats the line a later required question needed: refused, not shifted",
			qs:                []oap.Question{required("cpu", "CPU"), required("memory", "Memory")},
			script:            "\n750m\n",
			refusalIsRendered: true,
			wantErr:           "Memory: nothing was supplied",
		},
		{
			name:              "the retry eats the line a later optional question needed: that one stays unanswered",
			qs:                []oap.Question{required("cpu", "CPU"), optional("label", "Label")},
			script:            "\nfor-label\n",
			refusalIsRendered: true,
			wantAns:           map[string]any{"cpu": "for-label"},
			absent:            []string{"label"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, out := scriptedPresentation(t, tc.script)
			ans, _, err := Resolve(tc.qs, "", nil, true, present)

			// On the rows whose answers all still arrive, this is the ONLY
			// observable difference between a blank that was refused and one
			// that was recorded — without it those rows would pass either way.
			if tc.refusalIsRendered {
				assert.Contains(t, out.String(), "nothing was supplied",
					"a typed blank must be refused where the operator can read it and answer again")
			}

			if tc.wantErr != "" {
				require.Error(t, err, "an unanswerable required question must not be reported as success")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err, "Resolve")
			for name, want := range tc.wantAns {
				assert.Equal(t, want, ans[name], "answer for %q", name)
			}
			for _, name := range tc.absent {
				assert.NotContains(t, ans, name,
					"a question whose line was consumed by an earlier retry stays unanswered")
			}
		})
	}
}

// TestResolve_Interactive_SeededAnswerIsNotReAsked covers the State-first
// convention from the caller's side: an answer already supplied by --set
// leaves nothing for the question to ask, and the scripted line is left for
// the question that does need it.
func TestResolve_Interactive_SeededAnswerIsNotReAsked(t *testing.T) {
	binding := []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}}
	qs := []oap.Question{
		{Name: "cpu", Type: oap.QString, Prompt: "CPU", Binding: binding},
		{Name: "memory", Type: oap.QString, Prompt: "Memory", Binding: binding},
	}
	present, out := scriptedPresentation(t, "2048Mi\n")
	ans, _, err := Resolve(qs, "", map[string]string{"cpu": "750m"}, true, present)
	require.NoError(t, err, "Resolve")

	assert.Equal(t, "750m", ans["cpu"], "the --set answer stands")
	assert.Equal(t, "2048Mi", ans["memory"], "the one unanswered question consumed the one scripted line")
	assert.NotContains(t, out.String(), "CPU", "an answered question must not be asked again")
}

// TestResolve_Interactive_PresentationErrorIsUserFacing pins that the
// sequencer's own framing — `tui: present screen "cpu":` — is stripped before
// the message reaches the operator. A screen ID names a step in the
// sequencer, not anything they can act on.
func TestResolve_Interactive_PresentationErrorIsUserFacing(t *testing.T) {
	qs := []oap.Question{{
		Name: "cpu", Type: oap.QString, Prompt: "CPU",
		Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.field"}},
	}}
	th := tui.NewTheme(tui.Caps{})
	_, _, err := Resolve(qs, "", nil, true,
		PresentOver(errDriver{err: errors.New("the terminal went away")}, th))
	require.Error(t, err, "a failed presentation must not be reported as success")
	assert.Contains(t, err.Error(), "the terminal went away", "the underlying cause")
	assert.NotContains(t, err.Error(), "tui:", "internal framing must not reach the operator")
	assert.NotContains(t, err.Error(), "screen", "the screen ID names nothing actionable")
}

// A manifest question is asked from the middle of an install — between whatever
// the caller printed about the bundle above it and whatever it prints below —
// so the run declares itself inline rather than taking the alternate screen and
// putting all of that out of reach. See tui.Options.Inline for the rule.
//
// The declaration is only consulted on the fallback path: a caller that hands a
// driver in (which `oap agent install` does) has already made the choice, and
// stating it twice is how the two halves of one command come to disagree.
func TestAskOptionsDeclareTheRunInline(t *testing.T) {
	th := tui.NewTheme(tui.Caps{})

	opts := askOptions(th, nil)
	assert.True(t, opts.Inline, "install questions interleave with the install's own output")
	assert.NotNil(t, opts.In, "the fallback driver must buffer one named stream, not huh's own stdin")

	supplied := askOptions(th, errDriver{err: errors.New("unused")})
	assert.NotNil(t, supplied.Driver, "a supplied driver presents the run")
	assert.Equal(t, opts.Inline, supplied.Inline,
		"the declaration does not change with the driver; the driver simply outranks it")
}

func TestResolveValuesPartialDefersOnlyValidationErrorsCausedByMissingAnswers(t *testing.T) {
	questions := []oap.Question{
		{Name: "known", Type: oap.QString, Required: boolPtr(true), Validation: `args.known == args.missing`},
		{Name: "missing", Type: oap.QString, Required: boolPtr(true)},
	}
	answers, _, missing, err := resolveValuesPartial(questions, map[string]any{"known": "value"}, nil)
	require.NoError(t, err)
	require.NotNil(t, missing)
	assert.Equal(t, "value", answers["known"])

	questions[0].Validation = `1 / 0 == 1`
	_, _, _, err = resolveValuesPartial(questions, map[string]any{"known": "value"}, nil)
	require.Error(t, err, "a CEL failure independent of the omitted answer remains fatal")
}
