package channelkinds_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// TestUnavailableWizard_RefusesOnEveryMethod pins that the value
// UnavailableWizard returns refuses on every method of Wizard, each
// carrying reason.
//
// Each method gets its own subtest rather than one shared require.Error
// chain: a require.Error on an earlier call aborts the test on failure, so a
// later method that quietly started succeeding would never be reached and
// its regression would go unnoticed. Subtests make every method's refusal an
// independent fact.
func TestUnavailableWizard_RefusesOnEveryMethod(t *testing.T) {
	const reason = "demo kind builds its Channel elsewhere"
	w := channelkinds.UnavailableWizard(reason)

	t.Run("Inputs", func(t *testing.T) {
		qs, err := w.Inputs(context.Background(), channelkinds.WizardInput{})
		require.Error(t, err)
		assert.Nil(t, qs)
		assert.Contains(t, err.Error(), reason)
	})

	t.Run("Handoff", func(t *testing.T) {
		spec, err := w.Handoff(context.Background(), channelkinds.WizardInput{})
		require.Error(t, err, "a refusing wizard must refuse the handoff too, not return a nil spec")
		assert.Nil(t, spec, "a refusing Handoff returns no spec")
		assert.Contains(t, err.Error(), reason)
	})

	t.Run("Resolve", func(t *testing.T) {
		derived, err := w.Resolve(context.Background(), channelkinds.WizardInput{}, map[string]string{})
		require.Error(t, err, "a refusing wizard must refuse the derivation step too, not return no answers")
		assert.Nil(t, derived, "a refusing Resolve derives nothing")
		assert.Contains(t, err.Error(), reason)
	})

	t.Run("Result", func(t *testing.T) {
		_, err := w.Result(channelkinds.WizardInput{}, map[string]string{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), reason)
	})
}

// TestValidateInputs covers the renderer-relevant subset of oap.Question's
// rules ValidateInputs enforces — distinct from, and narrower than,
// oap.Manifest.ValidateQuestions, which additionally requires a Binding on
// every non-secret question and a CreateSecret/OrExisting block on every
// QSecret one. Neither of those two bundle-install rules applies to a
// channel wizard's answers, which Result applies itself. A third
// ValidateQuestions rule — a Secret block is invalid on a non-QSecret
// question — is NOT bundle-install specific and does carry over; see
// ValidateInputs' own doc for why.
func TestValidateInputs(t *testing.T) {
	valid := oap.Question{Name: "org", Type: oap.QString, Prompt: "GitHub organization"}

	cases := []struct {
		name      string
		qs        []oap.Question
		wantErr   bool
		errSubstr string
	}{
		{
			name: "one well-formed question passes",
			qs:   []oap.Question{valid},
		},
		{
			name: "empty slice passes",
			qs:   nil,
		},
		{
			name:      "empty name is rejected",
			qs:        []oap.Question{{Type: oap.QString, Prompt: "x"}},
			wantErr:   true,
			errSubstr: "name is required",
		},
		{
			name: "duplicate name is rejected",
			qs: []oap.Question{
				{Name: "org", Type: oap.QString, Prompt: "a"},
				{Name: "org", Type: oap.QString, Prompt: "b"},
			},
			wantErr:   true,
			errSubstr: "duplicate input name",
		},
		{
			name:      "unknown type is rejected",
			qs:        []oap.Question{{Name: "org", Type: oap.QuestionType("bogus"), Prompt: "x"}},
			wantErr:   true,
			errSubstr: "unknown type",
		},
		{
			name:      "enum with no values is rejected",
			qs:        []oap.Question{{Name: "tier", Type: oap.QEnum, Prompt: "Tier"}},
			wantErr:   true,
			errSubstr: "requires enum values",
		},
		{
			name:      "empty prompt is rejected",
			qs:        []oap.Question{{Name: "org", Type: oap.QString}},
			wantErr:   true,
			errSubstr: "prompt is required",
		},
		{
			name:      "a binding is rejected: it would be silently ignored by every renderer",
			qs:        []oap.Question{{Name: "org", Type: oap.QString, Prompt: "x", Binding: []oap.Binding{{Target: "Channel/demo-channel#spec.org"}}}},
			wantErr:   true,
			errSubstr: "binding is a bundle-install concept",
		},
		{
			name: "a QSecret question with no Secret block passes: unlike a bundle-install Question, Secret is not required here",
			qs:   []oap.Question{{Name: "token", Type: oap.QSecret, Prompt: "API token"}},
		},
		{
			name: "a secret block on a non-secret type is rejected: Result builds its own Secret, so nothing reads this one",
			qs: []oap.Question{{
				Name: "org", Type: oap.QString, Prompt: "x",
				Secret: &oap.SecretQuestion{OrExisting: true},
			}},
			wantErr:   true,
			errSubstr: "secret block is only valid for type=secret",
		},
		{
			name: "enum labels the same length as the enum: accepted",
			qs: []oap.Question{{
				Name: "route", Type: oap.QEnum, Prompt: "Which route?",
				Enum:       []string{"false", "true"},
				EnumLabels: []string{"Show me the manifest", "I already have an app"},
			}},
		},
		{
			name: "no enum labels at all: accepted, the value is its own label",
			qs: []oap.Question{{
				Name: "route", Type: oap.QEnum, Prompt: "Which route?",
				Enum: []string{"false", "true"},
			}},
		},
		{
			name: "short enum labels are rejected: the pairing is positional, so they would label the wrong rows",
			qs: []oap.Question{{
				Name: "route", Type: oap.QEnum, Prompt: "Which route?",
				Enum:       []string{"false", "true", "provision"},
				EnumLabels: []string{"Show me the manifest", "I already have an app"},
			}},
			wantErr:   true,
			errSubstr: "enumLabels has 2 entries but enum has 3",
		},
		{
			name: "a validation rule is rejected: nothing on the channel side evaluates it yet",
			qs: []oap.Question{{
				Name: "org", Type: oap.QString, Prompt: "x",
				Validation: "args.org != ''",
			}},
			wantErr:   true,
			errSubstr: "validation is not evaluated",
		},
		{
			name: "a gate on an earlier question: accepted, and is what makes a branching set possible",
			qs: []oap.Question{
				{Name: "route", Type: oap.QEnum, Prompt: "Which route?", Enum: []string{"have", "provision"}},
				{Name: "token", Type: oap.QSecret, Prompt: "Token",
					AskWhen: oap.AskWhen{Question: "route", In: []string{"have"}}},
			},
		},
		{
			name: "a gate on a LATER question is rejected: its answer does not exist when the gate is read",
			qs: []oap.Question{
				{Name: "token", Type: oap.QSecret, Prompt: "Token",
					AskWhen: oap.AskWhen{Question: "route", In: []string{"have"}}},
				{Name: "route", Type: oap.QEnum, Prompt: "Which route?", Enum: []string{"have", "provision"}},
			},
			wantErr:   true,
			errSubstr: "is not declared before it",
		},
		{
			name: "a gate on the question's own answer is rejected",
			qs: []oap.Question{{
				Name: "token", Type: oap.QSecret, Prompt: "Token",
				AskWhen: oap.AskWhen{Question: "token", In: []string{"have"}},
			}},
			wantErr:   true,
			errSubstr: "reads its own answer",
		},
		{
			name: "a gate naming no answer is rejected: it could never open",
			qs: []oap.Question{
				{Name: "route", Type: oap.QEnum, Prompt: "Which route?", Enum: []string{"have", "provision"}},
				{Name: "token", Type: oap.QSecret, Prompt: "Token",
					AskWhen: oap.AskWhen{Question: "route"}},
			},
			wantErr:   true,
			errSubstr: "no answer to it",
		},
		{
			name: "gate values with no question to read them is rejected",
			qs: []oap.Question{{
				Name: "token", Type: oap.QSecret, Prompt: "Token",
				AskWhen: oap.AskWhen{In: []string{"have"}},
			}},
			wantErr:   true,
			errSubstr: "names no question to read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := channelkinds.ValidateInputs(tc.qs)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

// TestValidateInputs_AllSixTypesAccepted pins that every declared
// oap.QuestionType, not just the two exercised above (QString, QEnum),
// passes ValidateInputs on an otherwise well-formed question. Without this,
// deleting a single case from ValidateInputs' type switch — say, QInt —
// leaves every case in TestValidateInputs green, because none of them
// constructs a QInt question.
func TestValidateInputs_AllSixTypesAccepted(t *testing.T) {
	types := []oap.QuestionType{oap.QString, oap.QInt, oap.QBool, oap.QEnum, oap.QSecret, oap.QResourceList}
	for _, typ := range types {
		t.Run(string(typ), func(t *testing.T) {
			q := oap.Question{Name: "field", Type: typ, Prompt: "Field"}
			if typ == oap.QEnum {
				q.Enum = []string{"a", "b"}
			}
			assert.NoError(t, channelkinds.ValidateInputs([]oap.Question{q}))
		})
	}
}
