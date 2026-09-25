package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Standing has NO default, so "unset" is its own outcome and must never
// silently resolve to either side. This replaces a table that asserted unset
// resolved to session-only — the behaviour that let a type nobody classified
// decide, by omission, that the session's own approvers could approve it.
func TestSpiceDBResource_StandingDeclared(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "unset is NOT declared — there is no default to fall back to", in: "", want: false},
		{name: "an unrecognized value is not declared either", in: "sometimes", want: false},
		{name: "session-only is declared", in: StandingSessionOnly, want: true},
		{name: "required is declared", in: StandingRequired, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := SpiceDBResource{Name: "git_repo", Standing: tc.in}
			assert.Equal(t, tc.want, r.StandingDeclared())
		})
	}
}

// ValidateStanding is where the pairing rule lives: ApproverPermission must be
// present exactly when something consults it. Both halves matter — a required
// type without one leaves the approval router no pool to resolve, and a
// session-only type WITH one reads as governance that is never checked.
func TestSpiceDBResource_ValidateStanding(t *testing.T) {
	cases := []struct {
		name      string
		res       SpiceDBResource
		wantErr   bool
		errSubstr string
	}{
		{
			name: "required with an approver permission is complete",
			res:  SpiceDBResource{Name: "crm_company", Standing: StandingRequired, ApproverPermission: "owner"},
		},
		{
			name: "session-only with no approver permission is complete",
			res:  SpiceDBResource{Name: "git_repo", Standing: StandingSessionOnly},
		},
		{
			name:      "required without one leaves the router no pool",
			res:       SpiceDBResource{Name: "crm_company", Standing: StandingRequired},
			wantErr:   true,
			errSubstr: "approverPermission is unset",
		},
		{
			name:      "session-only with one claims governance nothing consults",
			res:       SpiceDBResource{Name: "git_repo", Standing: StandingSessionOnly, ApproverPermission: "owner"},
			wantErr:   true,
			errSubstr: "consults no permission",
		},
		{
			name:      "unset is refused rather than defaulted",
			res:       SpiceDBResource{Name: "git_repo"},
			wantErr:   true,
			errSubstr: "There is no default",
		},
		{
			name:      "an unrecognized standing is refused",
			res:       SpiceDBResource{Name: "git_repo", Standing: "sometimes"},
			wantErr:   true,
			errSubstr: "not recognized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.res.ValidateStanding()
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
			assert.Contains(t, err.Error(), tc.res.Name, "the message must name the offending type")
		})
	}
}
