package channelkinds_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestParseTriggerOutcome covers the whole closed set plus the two ways a
// caller can get it wrong. The outcome vocabulary is what a model supplies, so
// an unrecognized value must be refused rather than passed through to a
// provider that would reject it (or, worse, accept it as something else).
func TestParseTriggerOutcome(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    channelkinds.TriggerOutcome
		wantErr bool
	}{
		{name: "clean parses", in: "clean", want: channelkinds.TriggerOutcomeClean},
		{name: "problems_found parses", in: "problems_found", want: channelkinds.TriggerOutcomeProblemsFound},
		{name: "could_not_finish parses", in: "could_not_finish", want: channelkinds.TriggerOutcomeCouldNotFinish},
		{name: "empty is refused, not defaulted", in: "", wantErr: true},
		{name: "a provider's own vocabulary is refused", in: "action_required", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := channelkinds.ParseTriggerOutcome(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "could_not_finish",
					"a refusal must name the legal values, or the model cannot correct itself")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTriggerOutcomes_IsTheWholeSet keeps the enumeration and the parser from
// drifting: the list is what a tool's JSON schema offers a model, and the
// parser is what accepts the answer. A value in one and not the other is a
// tool whose own schema proposes an argument it then refuses.
func TestTriggerOutcomes_IsTheWholeSet(t *testing.T) {
	all := channelkinds.TriggerOutcomes()
	require.NotEmpty(t, all)
	for _, o := range all {
		got, err := channelkinds.ParseTriggerOutcome(string(o))
		require.NoError(t, err, "TriggerOutcomes() offers %q but ParseTriggerOutcome refuses it", o)
		assert.Equal(t, o, got)
	}
}
