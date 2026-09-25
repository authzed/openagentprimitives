package tailscale_authkey_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/tailscale_authkey"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

const goodKey = "tskey-auth-kABC123CNTRL-deadbeefcafe"

// run drives the flow end to end over a scripted stdin, exactly as the CLI
// does, and returns what reached Store.
//
// script is one line per prompt the run reaches. Every prompt MUST have a line:
// huh's accessible renderer cannot report a read error, so a short script
// silently answers the remainder from their defaults and completes with a nil
// error. That is why every assertion below is on the STORED credential and
// never on err == nil.
func run(t *testing.T, req builtins.Request, script []string, seed map[string]string) (builtins.StoreValue, string, error) {
	t.Helper()

	var stored builtins.StoreValue
	storeCalls := 0
	req.Store = func(_ context.Context, v builtins.StoreValue) error {
		storeCalls++
		stored = v
		return nil
	}

	f := tailscale_authkey.New()
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

func testRequest() builtins.Request {
	return builtins.Request{
		Provider: &provider.Provider{
			ID: "tailscale-authkey", Builtin: "tailscale-authkey", Shape: "bearer",
			DocsURL: "https://tailscale.com/kb/1085/auth-keys",
			TokenShape: &provider.TokenShape{
				Pattern:     `^tskey-auth-[A-Za-z0-9]+-[A-Za-z0-9_-]+$`,
				Description: "starts with tskey-auth-",
			},
		},
		Requirement:  authkind.CredentialRequirement{SuggestedName: "tailscale-authkey", BindingEnv: map[string]string{"TS_AUTHKEY": ""}},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
	}
}

func TestFlowName(t *testing.T) {
	assert.Equal(t, "tailscale-authkey", tailscale_authkey.New().Name())
}

func TestScreensAreStableAndNamed(t *testing.T) {
	screens, err := tailscale_authkey.New().Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	assert.Equal(t, []string{"browser", "key"}, ids,
		"the console page is opened before the key is asked for, so the user has somewhere to mint one")
}

// TestPastedKeyIsStored is the happy path, asserted on the credential rather
// than on the absence of an error. The settings the key must carry are checked
// too: a key created without them silently fails to work later.
func TestPastedKeyIsStored(t *testing.T) {
	rec := browsertest.Record(t)

	stored, out, err := run(t, testRequest(), []string{goodKey}, nil)
	require.NoError(t, err, "flow run")
	assert.Equal(t, goodKey, stored.Bearer)
	assert.Equal(t, "https://login.tailscale.com/admin/settings/keys", rec.Last())

	for _, want := range []string{"Reusable", "Ephemeral", "Pre-authorized", "tag:", "tagOwners"} {
		assert.Contains(t, out, want, "the required key settings must be spelled out")
	}
}

// TestRefusals covers every way this flow must decline to store, each of which
// would otherwise leave a credential that cannot authenticate.
func TestRefusals(t *testing.T) {
	browsertest.Record(t)

	cases := []struct {
		name      string
		script    []string
		errSubstr string
	}{
		{
			name: "a token from the wrong provider: refused, naming the shape it should have had",
			// Two lines because the field's validator re-prompts a rejected
			// value; the second is what the run ends up carrying.
			script:    []string{"ghp_not_a_tailscale_key", "still-not-a-tailscale-key"},
			errSubstr: "starts with tskey-auth-",
		},
		{
			name:      "no input at all: refused rather than storing an empty key",
			script:    nil,
			errSubstr: "nothing was supplied",
		},
		{
			name:      "a blank line: refused rather than storing an empty key",
			script:    []string{""},
			errSubstr: "nothing was supplied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, _, err := run(t, testRequest(), tc.script, nil)
			require.Error(t, err, "the flow must refuse this input")
			assert.Contains(t, err.Error(), tc.errSubstr)
			assert.Empty(t, stored.Bearer, "nothing may be stored on a refusal")
		})
	}
}

// TestSeededKeySkipsTheBrowser pins the reason the browser is a screen of its
// own: a key supplied ahead of time is a run with nothing to mint, so opening
// a tab would be pure noise.
func TestSeededKeySkipsTheBrowser(t *testing.T) {
	rec := browsertest.Record(t)

	stored, _, err := run(t, testRequest(), nil, map[string]string{tailscale_authkey.KeyAuthKey: goodKey})
	require.NoError(t, err)
	assert.Equal(t, goodKey, stored.Bearer, "the seeded key is what gets stored")
	assert.Zero(t, rec.Count(), "a run that already has its key must not open a browser")
}

// TestSeededKeyIsStillChecked: supplying an answer ahead of time skips the
// question, not the format gate.
func TestSeededKeyIsStillChecked(t *testing.T) {
	browsertest.Record(t)

	stored, _, err := run(t, testRequest(), nil, map[string]string{tailscale_authkey.KeyAuthKey: "ghp_wrong_provider"})
	require.Error(t, err, "a seeded key of the wrong shape must be refused")
	assert.Contains(t, err.Error(), "starts with tskey-auth-")
	assert.Empty(t, stored.Bearer)
}

// TestBrowserFailureIsSurfacedNotSwallowed: a browser that will not open is not
// fatal, but the user must be told where to go instead — on the screen where
// they need it, not in a log.
func TestBrowserFailureIsSurfacedNotSwallowed(t *testing.T) {
	browsertest.Fail(t, errors.New("no browser on this machine"))

	stored, out, err := run(t, testRequest(), []string{goodKey}, nil)
	require.NoError(t, err, "a browser that will not open must not fail the run")
	assert.Equal(t, goodKey, stored.Bearer)
	assert.Contains(t, out, "no browser on this machine", "the reason the browser did not open must reach the user")
	assert.Contains(t, out, "https://login.tailscale.com/admin/settings/keys",
		"the address must be shown so the user can open it themselves")
}

// TestGuidanceFitsTheNoteWidth guards the address and the tailnet-policy
// snippet: both are meant to be copied, and huh wraps an over-long note line at
// the form's column budget with no sign the halves belong together.
//
// The browser's own failure reason is exempt. It comes from the operating
// system, so its length is not ours to bound — which is exactly why it is
// composed onto a line of its own, and why this test proves it drags nothing
// else over with it.
func TestGuidanceFitsTheNoteWidth(t *testing.T) {
	const reason = "an unusually long explanation of why no browser could be started here"
	browsertest.Fail(t, errors.New(reason))

	_, out, err := run(t, testRequest(), []string{goodKey}, nil)
	require.NoError(t, err)
	for _, line := range tui.RailedNoteBudget().Overflows(out) {
		assert.Contains(t, line, reason,
			"only the operating system's own failure text may exceed the note width; %q (%d columns) did too",
			line, len([]rune(line)))
	}
	assert.Contains(t, out, "Page: https://login.tailscale.com/admin/settings/keys",
		"the address must appear whole, on one line")
	assert.Contains(t, out, `"tagOwners": { "tag:sre-bot": ["autogroup:admin"] }`,
		"the policy snippet must appear whole, on one line")
}

func TestVerifyIsUnsupported(t *testing.T) {
	res, err := tailscale_authkey.New().Verify(context.Background(), builtins.VerifyRequest{
		Value: builtins.StoreValue{Bearer: goodKey},
	})
	require.NoError(t, err)
	assert.Equal(t, builtins.VerifyUnsupported, res.Status,
		"there is no endpoint that will confirm a Tailscale auth key, and claiming otherwise is worse than saying nothing")
}
