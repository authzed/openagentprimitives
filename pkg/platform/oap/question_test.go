package oap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateQuestions(t *testing.T) {
	cases := []struct {
		name string
		qs   []Question
		want string // "" = no error
	}{
		{
			name: "valid string question with binding: ok",
			qs: []Question{{
				Name: "repos", Type: QResourceList, Prompt: "repos?",
				Binding: []Binding{{Target: "AgentClass/pm#spec.boundEntities[github_repo].defaults"}},
			}},
		},
		{
			name: "valid secret question: ok",
			qs: []Question{{
				Name: "githubToken", Type: QSecret, Prompt: "PAT?",
				Secret: &SecretQuestion{CreateSecret: &SecretTarget{Name: "gh-pat", Key: "token"}},
			}},
		},
		{name: "empty name: error", qs: []Question{{Type: QString, Binding: []Binding{{Target: "X/y#a"}}}}, want: "name is required"},
		{name: "duplicate name: error", qs: []Question{
			{Name: "a", Type: QString, Binding: []Binding{{Target: "X/y#a"}}},
			{Name: "a", Type: QString, Binding: []Binding{{Target: "X/y#b"}}},
		}, want: "duplicate question name"},
		{name: "unknown type: error", qs: []Question{{Name: "a", Type: "weird", Binding: []Binding{{Target: "X/y#a"}}}}, want: "unknown type"},
		{name: "enum without values: error", qs: []Question{{Name: "a", Type: QEnum, Binding: []Binding{{Target: "X/y#a"}}}}, want: "requires enum values"},
		{name: "enum labels matching the enum: ok", qs: []Question{{
			Name: "a", Type: QEnum, Enum: []string{"x", "y"}, EnumLabels: []string{"Ex", "Why"},
			Binding: []Binding{{Target: "X/y#a"}},
		}}},
		// Positional pairing: a short list would label the wrong rows, so the
		// operator would read one option and answer another.
		{name: "enum labels shorter than the enum: error", qs: []Question{{
			Name: "a", Type: QEnum, Enum: []string{"x", "y", "z"}, EnumLabels: []string{"Ex", "Why"},
			Binding: []Binding{{Target: "X/y#a"}},
		}}, want: "enumLabels has 2 entries but enum has 3"},
		{name: "secret without block: error", qs: []Question{{Name: "a", Type: QSecret}}, want: "requires a secret block"},
		{name: "secret block on non-secret: error", qs: []Question{{Name: "a", Type: QString, Secret: &SecretQuestion{OrExisting: true}, Binding: []Binding{{Target: "X/y#a"}}}}, want: "only valid for type=secret"},
		{name: "non-secret without binding: error", qs: []Question{{Name: "a", Type: QString}}, want: "needs at least one binding"},
		{
			name: "capacity.-prefixed name: error (reserved for synthesized capacity questions)",
			qs: []Question{{
				Name: "capacity.test-sandbox.memory", Type: QString,
				Binding: []Binding{{Target: "SpiceboxClass/test-sandbox#spec.resources.memory"}},
			}},
			want: "capacity.",
		},
		{
			name: "capacity_tier name (no dot): ok, only the dotted prefix is reserved",
			qs: []Question{{
				Name: "capacity_tier", Type: QString,
				Binding: []Binding{{Target: "SpiceboxClass/test-sandbox#spec.resources.memory"}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Questions: tc.qs}
			err := m.ValidateQuestions()
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestIsRequired_DefaultsTrue(t *testing.T) {
	assert.True(t, Question{}.IsRequired())
	no := false
	assert.False(t, Question{Required: &no}.IsRequired())
}

// TestApplies is the whole reading of a branching question set: one rule,
// stated here, so a terminal driving screens and a server deciding what is
// still owed cannot come to ask different things of the same operator.
//
// The unanswered case is the one worth naming. A gate whose question has not
// been answered yet reads as "does not apply", which is what makes a set
// rendered in declaration order behave: the credential questions of a route
// nobody has chosen are not put to anyone.
func TestApplies(t *testing.T) {
	gated := Question{
		Name:    "token",
		AskWhen: AskWhen{Question: "route", In: []string{"have", "reuse"}},
	}

	cases := []struct {
		name    string
		q       Question
		answers map[string]string
		want    bool
	}{
		{name: "no gate: asked whatever else was answered", q: Question{Name: "org"}, want: true},
		{name: "the gate's answer is one it names: asked", q: gated, answers: map[string]string{"route": "have"}, want: true},
		{name: "a second named answer opens the same gate: asked", q: gated, answers: map[string]string{"route": "reuse"}, want: true},
		{name: "the gate's answer is another route: not asked", q: gated, answers: map[string]string{"route": "provision"}, want: false},
		{name: "the gate's question is unanswered: not asked", q: gated, answers: map[string]string{}, want: false},
		{name: "a padded answer still opens the gate: asked", q: gated, answers: map[string]string{"route": "  have "}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.q.Applies(func(k string) string { return tc.answers[k] }))
		})
	}

	assert.True(t, Question{Name: "org"}.Applies(nil),
		"a caller with no answers at all still asks an ungated question")
	assert.False(t, gated.Applies(nil),
		"and can satisfy no gate, rather than dereferencing a nil reader")
}

func TestSplitReserved(t *testing.T) {
	t.Run("reserved-prefixed keys route separately from everything else", func(t *testing.T) {
		other, reserved := SplitReserved(map[string]string{
			"demoToken":                  "abc",
			"capacity.demo-class.memory": "2Gi",
		})
		assert.Equal(t, map[string]string{"demoToken": "abc"}, other)
		assert.Equal(t, map[string]string{"capacity.demo-class.memory": "2Gi"}, reserved)
	})

	t.Run("nil input yields two empty (not nil) maps", func(t *testing.T) {
		other, reserved := SplitReserved(nil)
		assert.Empty(t, other)
		assert.Empty(t, reserved)
	})
}
