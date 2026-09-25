package onepassword_scim_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/onepassword_scim"
)

// noBrowser is the opener handed to a Flow whose screens this test never
// presents, so nothing ever calls it. Named rather than nil so every
// construction in this file reads the same way.
func noBrowser(string) error { return nil }

const goodToken = "eyJhbGciOiJFUzI1NiJ9.CONTRACT.SENTINELVALUE"

// run drives the flow end to end over a scripted stdin with a browser that
// opens nothing, and returns what reached Store.
//
// script is one line per prompt the run reaches. Every prompt MUST have a
// line: huh's accessible renderer cannot report a read error, so a short
// script silently answers the remainder from their defaults and completes with
// a nil error. That is why every assertion below is on the STORED credential
// and never on err == nil.
func run(t *testing.T, req builtins.Request, script []string, seed map[string]string) (builtins.StoreValue, string, error) {
	t.Helper()
	return runWithBrowser(t, func(string) error { return nil }, req, script, seed)
}

// runWithBrowser is run with the browser opener the caller wants — one that
// records the URL, or one that fails.
//
// Every run in this package builds its Flow with an injected opener, which is
// why none of them can reach a real browser: the opener is a constructor
// argument rather than a package-level hook with a setter, so the safe path is
// the one you get by writing the test at all, not the one you get by
// remembering to opt out of the unsafe one.
func runWithBrowser(
	t *testing.T, open func(string) error,
	req builtins.Request, script []string, seed map[string]string,
) (builtins.StoreValue, string, error) {
	t.Helper()

	var stored builtins.StoreValue
	storeCalls := 0
	req.Store = func(_ context.Context, v builtins.StoreValue) error {
		storeCalls++
		stored = v
		return nil
	}

	f := onepassword_scim.New(open)
	screens, err := f.Screens(context.Background(), req)
	if err != nil {
		return builtins.StoreValue{}, "", err
	}

	st := tui.NewState()
	for k, v := range seed {
		st.Set(k, v)
	}
	in := ""
	if len(script) > 0 {
		in = strings.Join(script, "\n") + "\n"
	}
	var out bytes.Buffer
	st, err = tui.RunWith(context.Background(), screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(in),
		Out:   &out,
	}, st)
	if err != nil {
		return builtins.StoreValue{}, out.String(), err
	}

	err = f.Result(context.Background(), req, st)
	if err == nil {
		assert.Equal(t, 1, storeCalls, "a successful Result must store exactly once")
	} else {
		assert.Zero(t, storeCalls, "a failing Result must not have stored anything")
	}
	return stored, out.String(), err
}

// testRequest mirrors the catalog entry this flow serves. It declares NO
// tokenShape, matching providers/onepassword-scim.yaml: 1Password documents no
// stable layout for this token, so there is no format to gate on.
func testRequest() builtins.Request {
	return builtins.Request{
		Provider: &provider.Provider{
			ID: "onepassword-scim", Builtin: "onepassword-scim", Shape: "bearer",
			Title:   "1Password",
			DocsURL: "https://support.1password.com/scim/",
		},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
		UserIntent:   "read-only access to managed groups and their members for directory sync",
	}
}

func TestFlowName(t *testing.T) {
	assert.Equal(t, "onepassword-scim", onepassword_scim.New(noBrowser).Name())
}

func TestScreensAreStableAndNamed(t *testing.T) {
	screens, err := onepassword_scim.New(noBrowser).Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	assert.Equal(t, []string{"browser", "token"}, ids,
		"the setup documentation is opened before the token is asked for, so the user knows where to look")
}

// TestPastedTokenIsStored is the happy path, asserted on the credential rather
// than on the absence of an error.
func TestPastedTokenIsStored(t *testing.T) {
	openedURL := ""
	stored, out, err := runWithBrowser(t,
		func(u string) error { openedURL = u; return nil },
		testRequest(), []string{goodToken}, nil)
	require.NoError(t, err)
	assert.Equal(t, goodToken, stored.Bearer)
	assert.Equal(t, "https://support.1password.com/scim/", openedURL)
	assert.NotContains(t, out, goodToken,
		"the token must never be echoed into the run's own output")
}

// TestBlankAnswerIsRefused is the whole fail-closed story for this flow, and
// the reason it gets its own test rather than leaning on a format gate: the
// provider declares no tokenShape, so provider.ValidateToken accepts anything
// — including "" — and the emptiness guard in Result is the ONLY thing between
// an input script that ran out and a stored credential authenticating as
// nobody.
func TestBlankAnswerIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		state func() *tui.State
	}{
		{
			name:  "answered with whitespace: refused, nothing stored",
			state: func() *tui.State { st := tui.NewState(); st.Set(onepassword_scim.KeyToken, "   "); return st },
		},
		{
			name:  "never answered at all: refused, nothing stored",
			state: tui.NewState,
		},
		{
			name:  "a nil State from a run that failed partway: refused, no panic",
			state: func() *tui.State { return nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testRequest()
			var storeCalls int
			req.Store = func(context.Context, builtins.StoreValue) error { storeCalls++; return nil }

			assert.NotPanics(t, func() {
				assert.Error(t, onepassword_scim.New(noBrowser).Result(context.Background(), req, tc.state()))
			})
			assert.Zero(t, storeCalls, "nothing may reach Store on a flow that was not answered")
		})
	}
}

// TestGuidanceSeparatesTheTwoSetupArtifacts pins the one mistake this flow
// exists to prevent: bridge setup hands the operator a bearer token AND a
// scimsession file, and only the first is a per-request credential. It also
// pins that the bridge's own address is declared out of scope here — it is
// collected by the wizard's endpoint screen, and a second copy asked for here
// is how the two come to disagree.
func TestGuidanceSeparatesTheTwoSetupArtifacts(t *testing.T) {
	_, out, err := run(t, testRequest(), []string{goodToken}, nil)
	require.NoError(t, err)
	assert.Contains(t, out, "scimsession",
		"the guidance must name the artifact that is NOT the credential")
	assert.Contains(t, out, "address",
		"the guidance must say the bridge's address is asked for elsewhere")
}

// TestGuidanceFitsTheNoteWidth guards the address: it is meant to be copied,
// and huh wraps an over-long note line at the form's column budget with no
// sign the halves belong together.
//
// The browser's own failure reason is exempt. It comes from the operating
// system, so its length is not ours to bound — which is exactly why it is
// composed onto a line of its own, and why this test proves it drags nothing
// else over with it.
func TestGuidanceFitsTheNoteWidth(t *testing.T) {
	const reason = "an unusually long explanation of why no browser could be started here"
	_, out, err := runWithBrowser(t,
		func(string) error { return errors.New(reason) },
		testRequest(), []string{goodToken}, nil)
	require.NoError(t, err)
	for _, line := range tui.RailedNoteBudget().Overflows(out) {
		assert.Contains(t, line, reason,
			"only the operating system's own failure text may exceed the note width; %q (%d columns) did too",
			line, len([]rune(line)))
	}
	assert.Contains(t, out, "Page: https://support.1password.com/scim/",
		"the address must appear whole, on one line")
}

// TestVerifyIsUnsupported pins the reason this credential gets no probe: the
// bridge is customer-hosted, so there is no catalog-constant endpoint to check
// the token against.
func TestVerifyIsUnsupported(t *testing.T) {
	res, err := onepassword_scim.New(noBrowser).Verify(context.Background(), builtins.VerifyRequest{
		Value: builtins.StoreValue{Bearer: goodToken},
	})
	require.NoError(t, err)
	assert.Equal(t, builtins.VerifyUnsupported, res.Status,
		"there is no fixed host to probe, and claiming otherwise is worse than saying nothing")
	assert.NotEmpty(t, res.Detail, "an unsupported verdict must say why, per VerifyResult.Detail")
}
