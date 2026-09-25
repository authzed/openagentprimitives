package idpscreens_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
)

// This package's questions are tui.Questions, so the screen MECHANICS — State
// first, skip, optional, the re-check, the summary line — are asserted once in
// pkg/cli/tui rather than again here. What is left is this package's own
// vocabulary: which policies exist, which one silence takes, and how a domain
// list is read.

// runScreens drives screens over a scripted input through the plain driver —
// the same one an off-TTY run gets — and hands back the answered State.
func runScreens(t *testing.T, screens []tui.Screen, st *tui.State, script string) (*tui.State, string, error) {
	t.Helper()
	var out bytes.Buffer
	answered, err := tui.RunWith(context.Background(), screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(script),
		Out:   &out,
	}, st)
	return answered, out.String(), err
}

// TestAccessSilenceNeverGrantsAllowAny is the guard on the one answer in this
// package that is not safe to fall into.
//
// It targets NewAccess's Default rather than its option order, because Default
// is what actually decides: a choice question that supplies one binds it over
// the first option, so pinning the order would pass with the Default reversed
// and prove nothing about the property. (Verified by mutation: swapping the
// option order leaves every silence assertion passing.)
//
// The last row is the interesting one. Silence there DOES land on allow-any,
// and that is correct: it retains a policy someone chose before rather than
// granting a new one. What must never happen is the first-setup row — a cluster
// that was restricted, or had no provider at all, becoming open to anyone the
// provider will authenticate because an input ran out.
func TestAccessSilenceNeverGrantsAllowAny(t *testing.T) {
	cases := []struct {
		name            string
		existingAny     bool
		existingDomains []string
		want            string
	}{
		{
			name: "first setup: silence restricts",
			want: idpscreens.AccessDomains,
		},
		{
			name:            "re-setup of a restricted provider: silence keeps restricting",
			existingDomains: []string{"demo-corp.example"},
			want:            idpscreens.AccessDomains,
		},
		{
			name:            "re-setup of a provider with both: silence restricts",
			existingAny:     true,
			existingDomains: []string{"demo-corp.example"},
			want:            idpscreens.AccessDomains,
		},
		{
			name:        "re-setup of an allow-any provider: silence retains allow-any",
			existingAny: true,
			want:        idpscreens.AccessAny,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scr := idpscreens.NewAccess(tc.existingAny, tc.existingDomains)
			answered, _, err := runScreens(t, []tui.Screen{scr}, tui.NewState(), "")
			require.NoError(t, err)
			assert.Equal(t, tc.want, answered.Get(idpscreens.KeyAccess))
		})
	}
}

// TestAccessAndDomainsTogether covers the two shared questions as the pair they
// are: choosing "any account" skips the domain list entirely, and choosing to
// restrict without listing one is refused.
func TestAccessAndDomainsTogether(t *testing.T) {
	cases := []struct {
		name        string
		script      string
		wantAccess  string
		wantDomains string
		wantErr     string
	}{
		{
			name:        "restrict, then list a domain: both recorded",
			script:      "1\ndemo-corp.example\n",
			wantAccess:  idpscreens.AccessDomains,
			wantDomains: "demo-corp.example",
		},
		{
			name:       "allow any: the domain question is skipped entirely",
			script:     "2\n",
			wantAccess: idpscreens.AccessAny,
		},
		{
			name:    "restrict, then run out of input: refused, naming the question",
			script:  "1\n",
			wantErr: "Allowed email domains",
		},
		{
			name: "restrict, then type only separators: refused, pointing back at the policy",
			// A value that is not empty but names no domain. It reaches the
			// question's own check, which a bare end-of-input never gets to.
			script:  "1\n,\n",
			wantErr: "at least one email domain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screens := []tui.Screen{idpscreens.NewAccess(false, nil), idpscreens.NewDomains(nil)}
			answered, _, err := runScreens(t, screens, tui.NewState(), tc.script)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, tui.UserFacing(err).Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAccess, answered.Get(idpscreens.KeyAccess))
			if tc.wantDomains == "" {
				assert.False(t, answered.Has(idpscreens.KeyDomains),
					"the domain question must not record an answer when it was skipped")
				return
			}
			assert.Equal(t, tc.wantDomains, answered.Get(idpscreens.KeyDomains))
		})
	}
}

// TestClientSecretKeepsTheStoredOneOnBlank covers the one option this package
// sets on the shared question, and the summary line that must never carry what
// was typed.
func TestClientSecretKeepsTheStoredOneOnBlank(t *testing.T) {
	t.Run("a stored secret is kept by a blank answer", func(t *testing.T) {
		answered, _, err := runScreens(t, []tui.Screen{idpscreens.NewClientSecret(true)}, tui.NewState(), "\n")
		require.NoError(t, err)
		assert.Equal(t, "", answered.Get(idpscreens.KeyClientSecret),
			"an explicit blank is the answer that means keep")
		require.Len(t, answered.Notes(), 1)
		assert.Equal(t, "kept the stored one", answered.Notes()[0].Value)
	})

	t.Run("a first setup refuses a blank", func(t *testing.T) {
		_, _, err := runScreens(t, []tui.Screen{idpscreens.NewClientSecret(false)}, tui.NewState(), "")
		require.Error(t, err)
		assert.Contains(t, tui.UserFacing(err).Error(), "Client secret")
	})

	t.Run("a supplied secret never reaches the summary", func(t *testing.T) {
		answered, _, err := runScreens(t, []tui.Screen{idpscreens.NewClientSecret(false)}, tui.NewState(), "s3cr3t-value\n")
		require.NoError(t, err)
		require.Len(t, answered.Notes(), 1)
		assert.Equal(t, "updated", answered.Notes()[0].Value)
		assert.NotContains(t, answered.Notes()[0].Value, "s3cr3t")
	})
}

func TestParseDomains(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "single domain", in: "demo-corp.example", want: []string{"demo-corp.example"}},
		{name: "spaces around separators are trimmed", in: "a.example, b.example", want: []string{"a.example", "b.example"}},
		{name: "a trailing comma is harmless", in: "a.example,", want: []string{"a.example"}},
		{name: "blanks only yield nothing", in: " , , ", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, idpscreens.ParseDomains(tc.in))
		})
	}
}

// TestCallbackAddressSpendsItsBudgetOnTheAddress pins why the label sits on its
// own line: inline, "Redirect URI: " costs 14 of the note's columns, and the
// address is the only part a user has to be able to copy.
//
// The fixture is 55 columns — under the budget on its own, over it once a
// 14-column prefix is added — so it is exactly the case the split rescues.
func TestCallbackAddressSpendsItsBudgetOnTheAddress(t *testing.T) {
	const url = "https://ap.platform.demo-corp.example/oidc/callback/idp"
	budget := tui.RailedNoteBudget()
	require.Greater(t, len(url)+len("Redirect URI: "), budget.Columns(),
		"this fixture must be one an inline label could not have carried, or it tests nothing")

	assert.True(t, idpscreens.CallbackAddress(url).Fits(budget))
}

// TestSecretUpdatedNeverReturnsTheValue is the guard on the one summary line a
// credential field renders for a value it actually received.
//
// It takes the value and must not use it — not even masked. The project's
// masker preserves the last four characters, which for a password is four
// characters of the password, so the tail assertion is the point of the test
// rather than a flourish.
func TestSecretUpdatedNeverReturnsTheValue(t *testing.T) {
	for _, v := range []string{"s3cr3t", "demo-admin-pw-7k3q", "xoxb-1234567890-abcdefghij"} {
		got := idpscreens.SecretUpdated(v)
		assert.NotEmpty(t, got, "a summary line with no value reads as a rendering fault")
		assert.NotContains(t, got, v, "SecretUpdated returned the credential")
		assert.NotContains(t, got, v[len(v)-4:], "SecretUpdated returned the tail of the credential")
	}
	assert.NotEmpty(t, idpscreens.SecretKept,
		"a blank credential still needs a summary line, or the run reports nothing about it")
}
