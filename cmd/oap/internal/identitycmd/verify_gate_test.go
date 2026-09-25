package identitycmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

type gateFlow struct{ result builtins.VerifyResult }

func (gateFlow) Name() string { return "gate-flow" }
func (gateFlow) Screens(context.Context, builtins.Request) ([]tui.Screen, error) {
	return nil, errors.New("gateFlow asks nothing; it exists for its Verify")
}
func (gateFlow) Result(context.Context, builtins.Request, *tui.State) error {
	return errors.New("gateFlow stores nothing; it exists for its Verify")
}
func (f gateFlow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return f.result, nil
}

// TestConfirmYN pins the answer vocabulary and, above all, the EOF contract:
// verifyGate refuses to store when it could not ask, so "nobody was there" must
// arrive as an error and never as a silent default in either direction.
func TestConfirmYN(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    bool
		wantErr bool
	}{
		{name: "y: true", input: "y\n", want: true},
		{name: "YES: true, case-insensitively", input: "YES\n", want: true},
		{name: "n: false", input: "n\n", want: false},
		{name: "empty line: false, the default-no", input: "\n", want: false},
		{name: "unrecognised word: false rather than a re-prompt", input: "wat\n", want: false},
		{name: "EOF with nothing typed: error, so an unattended run cannot be read as an answer", input: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &strings.Builder{}
			got, err := confirmYN(strings.NewReader(tc.input), out, "Store it anyway?")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Contains(t, out.String(), "Store it anyway? [y/N] ")
		})
	}
}

// TestConfirmYNSequentialCallsShareReader is why the read is byte-at-a-time: a
// bufio.Reader would pull the whole stream into a buffer that dies with the
// call, so the second question would see EOF and the gate would refuse to store
// a token the operator had already answered for.
func TestConfirmYNSequentialCallsShareReader(t *testing.T) {
	r := strings.NewReader("n\ny\nyes\n")
	out := &strings.Builder{}

	got1, err := confirmYN(r, out, "1?")
	require.NoError(t, err)
	assert.False(t, got1, "first line 'n' is false")

	got2, err := confirmYN(r, out, "2?")
	require.NoError(t, err)
	assert.True(t, got2, "second line 'y' is true, so call 1 did not eat it")

	got3, err := confirmYN(r, out, "3?")
	require.NoError(t, err)
	assert.True(t, got3, "third line 'yes' is true")
}

func TestVerifyGate(t *testing.T) {
	rejected := builtins.VerifyResult{Status: builtins.VerifyRejected, Detail: "GitHub rejected the token: 401"}
	valid := builtins.VerifyResult{Status: builtins.VerifyValid, Detail: "authenticated as octocat"}
	indet := builtins.VerifyResult{Status: builtins.VerifyIndeterminate, Detail: "could not reach api.github.com"}
	forbidden := builtins.VerifyResult{
		Status: builtins.VerifyForbidden,
		Detail: "GitHub accepted the credential but refused this check — Resource protected by organization SAML enforcement.",
	}

	cases := []struct {
		name        string
		result      builtins.VerifyResult
		skipVerify  bool
		interactive bool
		stdin       string
		wantErr     string   // "" = proceed
		wantOut     []string // substrings the operator must see
		wantNotOut  []string // substrings that would misdescribe the verdict
	}{
		{name: "valid: proceeds, prints subject", result: valid, wantOut: []string{"authenticated as octocat"}},
		{name: "indeterminate: proceeds with warning", result: indet, wantOut: []string{"could not reach", "storing anyway"}},
		{name: "unsupported: proceeds with a note", result: builtins.VerifyResult{Status: builtins.VerifyUnsupported, Detail: "no verifier"}, wantOut: []string{"no live verification available"}},
		{name: "rejected non-interactive: fail closed, suggests --skip-verify", result: rejected, wantErr: "--skip-verify", wantOut: []string{"401"}},
		{name: "rejected interactive, confirm y: proceeds", result: rejected, interactive: true, stdin: "y\n", wantOut: []string{"Store it anyway?"}},
		{name: "rejected interactive, decline: refuses", result: rejected, interactive: true, stdin: "n\n", wantErr: "not stored"},
		{name: "rejected interactive, EOF stdin: confirmation unavailable, fails closed", result: rejected, interactive: true, stdin: "", wantErr: "confirmation unavailable"},
		{name: "rejected + --skip-verify: proceeds, notes the skip", result: rejected, skipVerify: true, wantOut: []string{"skip"}},

		// A 403: the provider took the credential and refused this one check.
		// The POLICY is the rejected one — this is a credential-provisioning
		// gate and there is an operator standing at it — but the WORDING must
		// not tell them a working credential is bad, so every row here also
		// pins what must NOT be said.
		{
			name:       "forbidden non-interactive: fails closed, and says authenticated-but-refused rather than rejected",
			result:     forbidden,
			wantErr:    "refusing to store an unconfirmed token",
			wantOut:    []string{"authenticated but was refused for this check", "SAML enforcement"},
			wantNotOut: []string{"token verification failed", "rejected"},
		},
		{
			name:        "forbidden interactive, confirm y: proceeds (control — the prompt is a real decision point)",
			result:      forbidden,
			interactive: true,
			stdin:       "y\n",
			wantOut:     []string{"authenticated but was refused for this check", "Store it anyway?"},
			wantNotOut:  []string{"token verification failed"},
		},
		{
			name:        "forbidden interactive, decline: refuses, naming the refused check and not a rejection",
			result:      forbidden,
			interactive: true,
			stdin:       "n\n",
			wantErr:     "not stored",
			wantOut:     []string{"authenticated but was refused for this check"},
			wantNotOut:  []string{"token verification failed"},
		},
		{
			name:        "forbidden interactive, EOF stdin: cannot ask, so fails closed",
			result:      forbidden,
			interactive: true,
			stdin:       "",
			wantErr:     "confirmation unavailable",
			wantOut:     []string{"authenticated but was refused for this check"},
		},
		{
			name:       "forbidden + --skip-verify: still overridable",
			result:     forbidden,
			skipVerify: true,
			wantOut:    []string{"skip"},
		},

		// A verdict this build has no branch for. Falling out of the switch is
		// the conservative path here — the arms above are the only ones that may
		// store unattended — so an unknown verdict must land in the same
		// warn-and-ask policy rather than proceeding.
		{
			name:    "unrecognised verdict non-interactive: fails closed rather than storing on a verdict nobody here understands",
			result:  builtins.VerifyResult{Status: "verdict-this-build-does-not-know", Detail: "a verdict from a later build"},
			wantErr: "--skip-verify",
			wantOut: []string{"could not be checked"},
		},
		{
			name:        "unrecognised verdict interactive, confirm y: proceeds (control — the gate really ran)",
			result:      builtins.VerifyResult{Status: "verdict-this-build-does-not-know", Detail: "a verdict from a later build"},
			interactive: true,
			stdin:       "y\n",
			wantOut:     []string{"Store it anyway?"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Snapshot the real builtins so registering our fake doesn't
			// permanently wipe the registry for later-sorted tests in this
			// package (Reset clears ALL flows, not just gate-flow).
			saved := builtins.All()
			builtins.Reset()
			t.Cleanup(func() {
				builtins.Reset()
				for _, f := range saved {
					builtins.Register(f)
				}
			})
			builtins.Register(gateFlow{result: tc.result})
			prov := &provider.Provider{ID: "gate-prov", Builtin: "gate-flow"}

			out := &strings.Builder{}
			_, err := verifyGate(context.Background(), out, prov,
				builtins.StoreValue{Bearer: "tok"}, tc.skipVerify,
				strings.NewReader(tc.stdin), tc.interactive)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			for _, want := range tc.wantOut {
				assert.Contains(t, out.String(), want)
			}
			for _, unwanted := range tc.wantNotOut {
				assert.NotContains(t, out.String(), unwanted,
					"the wording must describe this verdict and not a different one")
			}
		})
	}
}
