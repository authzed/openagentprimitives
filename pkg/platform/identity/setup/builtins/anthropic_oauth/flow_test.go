package anthropic_oauth_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/anthropic_oauth"
)

const goodToken = "sk-ant-oat01-TESTTOKEN"

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

	f := anthropic_oauth.New()
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
			ID:           "anthropic-oauth",
			Instructions: "Run `claude setup-token` and paste the result.",
			DocsURL:      "https://code.claude.com/docs/en/authentication",
			TokenShape:   &provider.TokenShape{Pattern: `^sk-ant-`, Description: "starts with sk-ant-"},
		},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
	}
}

func TestName(t *testing.T) {
	assert.Equal(t, "anthropic-oauth", anthropic_oauth.New().Name())
}

func TestScreensAreStableAndNamed(t *testing.T) {
	screens, err := anthropic_oauth.New().Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	assert.Equal(t, []string{"token"}, ids,
		"this flow drives no OAuth dance and opens no page: the user generates the token themselves")
}

// TestPastedTokenIsStoredAfterTheInstructions is the happy path, asserted on
// the credential rather than on the absence of an error. The instructions are
// the whole reason this flow exists — the token is minted by a command on the
// user's own machine — so they have to be in front of the user before the
// question is.
func TestPastedTokenIsStoredAfterTheInstructions(t *testing.T) {
	stored, out, err := run(t, testRequest(), []string{goodToken}, nil)
	require.NoError(t, err, "flow run")
	assert.Equal(t, goodToken, stored.Bearer, "the pasted token is stored as a bearer credential")
	assert.Contains(t, out, "setup-token", "the instructions must be shown before the prompt")
	assert.Contains(t, out, "demo-bot", "the user must be told which identity they are connecting")
}

// TestRefusals covers every way this flow must decline to store, each of which
// would otherwise leave a credential that cannot authenticate.
func TestRefusals(t *testing.T) {
	cases := []struct {
		name      string
		script    []string
		errSubstr string
	}{
		{
			name: "an API key rather than a Code token: refused, naming the shape it should have had",
			// Two lines because the field's validator re-prompts a rejected
			// value; the second is what the run ends up carrying.
			script:    []string{"not-a-token", "still-not-a-token"},
			errSubstr: "starts with sk-ant-",
		},
		{
			name:      "no input at all: refused rather than storing an empty token",
			script:    nil,
			errSubstr: "nothing was supplied",
		},
		{
			name:      "a blank line: refused rather than storing an empty token",
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

// TestSeededTokenIsStillChecked: supplying an answer ahead of time skips the
// question, not the format gate.
func TestSeededTokenIsStillChecked(t *testing.T) {
	t.Run("well-formed: stored without asking", func(t *testing.T) {
		stored, _, err := run(t, testRequest(), nil, map[string]string{anthropic_oauth.KeyToken: goodToken})
		require.NoError(t, err)
		assert.Equal(t, goodToken, stored.Bearer)
	})
	t.Run("wrong shape: refused rather than stored unasked", func(t *testing.T) {
		stored, _, err := run(t, testRequest(), nil, map[string]string{anthropic_oauth.KeyToken: "ghp_wrong_provider"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "starts with sk-ant-")
		assert.Empty(t, stored.Bearer)
	})
}

// TestComposedGuidanceFitsTheNoteWidth covers the lines this flow writes
// itself. The provider's instructions are exempt: they are prose we neither
// author nor reflow, and prose that wraps is untidy rather than broken.
func TestComposedGuidanceFitsTheNoteWidth(t *testing.T) {
	req := testRequest()
	req.Provider.Instructions = ""
	_, out, err := run(t, req, []string{goodToken}, nil)
	require.NoError(t, err)
	for _, line := range tui.RailedNoteBudget().Overflows(out) {
		assert.Fail(t, "a composed guidance line is too wide for a note", "%q (%d columns)", line, len([]rune(line)))
	}
	// The label is on its own line so the address gets the note's whole width;
	// asserted as the address alone on a line, which is what a user copies.
	assert.Contains(t, out, "\nhttps://code.claude.com/docs/en/authentication\n",
		"a docs address short enough to render whole must be offered, on a line of its own")
}

func TestVerifyIsUnsupported(t *testing.T) {
	res, err := anthropic_oauth.New().Verify(context.Background(), builtins.VerifyRequest{
		Value: builtins.StoreValue{Bearer: goodToken},
	})
	require.NoError(t, err)
	assert.Equal(t, builtins.VerifyUnsupported, res.Status,
		"a Claude Code token is audience-bound, so no endpoint will confirm it")
}
