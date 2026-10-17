package onepassword_service_account_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/onepassword_service_account"
)

// noBrowser is the opener handed to a Flow whose screens this test never
// presents, so nothing ever calls it. Named rather than nil so every
// construction in this file reads the same way.
func noBrowser(string) error { return nil }

const goodToken = "ops_eyJhbGciOiJFUzI1NiJ9CONTRACTSENTINELVALUE"

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

	f := onepassword_service_account.New(open)
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

// testRequest mirrors the catalog entry this flow serves
// (providers/onepassword-service-account.yaml): a declared tokenShape
// requiring the ops_ prefix.
func testRequest() builtins.Request {
	return builtins.Request{
		Provider: &provider.Provider{
			ID: "onepassword-service-account", Builtin: "onepassword-service-account", Shape: "bearer",
			Title:   "1Password",
			DocsURL: "https://developer.1password.com/docs/service-accounts/",
			TokenShape: &provider.TokenShape{
				Pattern:     `^ops_[A-Za-z0-9_-]+$`,
				Description: "service-account tokens start with ops_",
			},
		},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
		UserIntent:   "fill saved logins into pages the agent's browser toolbox visits",
	}
}

func TestFlowName(t *testing.T) {
	assert.Equal(t, "onepassword-service-account", onepassword_service_account.New(noBrowser).Name())
}

func TestScreensAreStableAndNamed(t *testing.T) {
	screens, err := onepassword_service_account.New(noBrowser).Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	assert.Equal(t, []string{"browser", "token"}, ids,
		"the service-account documentation is opened before the token is asked for, so the user knows where to look")
}

// TestPastedTokenIsStored is the happy path, asserted on the credential
// rather than on the absence of an error.
func TestPastedTokenIsStored(t *testing.T) {
	openedURL := ""
	stored, out, err := runWithBrowser(t,
		func(u string) error { openedURL = u; return nil },
		testRequest(), []string{goodToken}, nil)
	require.NoError(t, err)
	assert.Equal(t, goodToken, stored.Bearer)
	assert.Equal(t, "https://developer.1password.com/docs/service-accounts/", openedURL)
	assert.NotContains(t, out, goodToken,
		"the token must never be echoed into the run's own output")
}

// TestGarbageTokenIsRejected pins that the declared tokenShape (ops_...) is
// enforced before Store is ever called — a value that merely LOOKS like a
// token, but lacks the prefix, must never be persisted.
func TestGarbageTokenIsRejected(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{name: "missing ops_ prefix", token: "sk-live-not-a-1password-token"},
		// Not the bare "ops_" prefix: the flow's own guidance text contains
		// that literal substring ("starts with ops_"), so a garbage token
		// equal to just the prefix would trivially fail the leak check below
		// for a reason that has nothing to do with the flow echoing it.
		{name: "ops_ prefix with a disallowed character", token: "ops_not!valid"},
		{name: "wrong prefix entirely", token: "ghp_AbCdEfGhIjKlMnOp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := run(t, testRequest(), []string{tc.token}, nil)
			require.Error(t, err)
			assert.NotContains(t, out, tc.token,
				"a rejected token must never be echoed into the run's own output")
		})
	}
}

// TestBlankAnswerIsRefused is the fail-closed story for an input script that
// ran out: huh's accessible renderer cannot report a read error, so an
// unanswered screen reaches Result as a nil error unless Result's own
// emptiness guard catches it first.
func TestBlankAnswerIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		state func() *tui.State
	}{
		{
			name: "answered with whitespace: refused, nothing stored",
			state: func() *tui.State {
				st := tui.NewState()
				st.Set(onepassword_service_account.KeyToken, "   ")
				return st
			},
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
				assert.Error(t, onepassword_service_account.New(noBrowser).Result(context.Background(), req, tc.state()))
			})
			assert.Zero(t, storeCalls, "nothing may reach Store on a flow that was not answered")
		})
	}
}

// TestGuidanceNamesTheSingleVaultRequirement pins the one mistake this flow
// exists to prevent: a service account scoped to more than one vault, or to
// write access, is not what the browser toolbox's direct-credential path
// expects, and the guidance must say so before the user ever creates one.
func TestGuidanceNamesTheSingleVaultRequirement(t *testing.T) {
	_, out, err := run(t, testRequest(), []string{goodToken}, nil)
	require.NoError(t, err)
	assert.Contains(t, out, "read_items",
		"the guidance must name the permission the service account needs")
	assert.Contains(t, out, "ONE vault",
		"the guidance must say the scope is exactly one vault")
}

// TestGuidanceFitsTheNoteWidth guards the address: it is meant to be copied,
// and huh wraps an over-long note line at the form's column budget with no
// sign the halves belong together.
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
	assert.Contains(t, out, "Page: https://developer.1password.com/docs/service-accounts/",
		"the address must appear whole, on one line")
}

// TestVerify is the status-mapping table over the injected verifyFn seam:
// Flow.Verify never makes a network call or touches the SDK in this package's
// tests, by construction (NewWithVerify).
func TestVerify(t *testing.T) {
	cases := []struct {
		name       string
		verify     func(ctx context.Context, token string) (directcred.Pin, error)
		wantStatus builtins.VerifyStatus
		wantDetail string // substring
	}{
		{
			name: "exactly one vault visible: Valid, name in Detail and Metadata",
			verify: func(context.Context, string) (directcred.Pin, error) {
				return directcred.Pin{VaultID: "vid1", VaultName: "Agents"}, nil
			},
			wantStatus: builtins.VerifyValid,
			wantDetail: "Agents",
		},
		{
			name: "two vaults visible: Forbidden, count in Detail",
			verify: func(context.Context, string) (directcred.Pin, error) {
				return directcred.Pin{}, fmt.Errorf(
					"onepassword: expected exactly one accessible vault, found %d: %v", 2, []string{"Agents", "Personal"})
			},
			wantStatus: builtins.VerifyForbidden,
			wantDetail: "2",
		},
		{
			name: "zero vaults visible: Forbidden, count in Detail",
			verify: func(context.Context, string) (directcred.Pin, error) {
				return directcred.Pin{}, fmt.Errorf(
					"onepassword: expected exactly one accessible vault, found %d: %v", 0, []string{})
			},
			wantStatus: builtins.VerifyForbidden,
			wantDetail: "0",
		},
		{
			name: "SDK rejected the token constructing a client: Rejected",
			verify: func(context.Context, string) (directcred.Pin, error) {
				return directcred.Pin{}, fmt.Errorf("onepassword: creating client: %w", errors.New("invalid service account token"))
			},
			wantStatus: builtins.VerifyRejected,
		},
		{
			name: "listing vaults failed (network/other): Indeterminate",
			verify: func(context.Context, string) (directcred.Pin, error) {
				return directcred.Pin{}, fmt.Errorf("onepassword: listing vaults: %w", errors.New("dial tcp: i/o timeout"))
			},
			wantStatus: builtins.VerifyIndeterminate,
		},
		{
			name: "success with no vault name: Indeterminate rather than a false Valid",
			verify: func(context.Context, string) (directcred.Pin, error) {
				return directcred.Pin{}, nil
			},
			wantStatus: builtins.VerifyIndeterminate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := onepassword_service_account.NewWithVerify(noBrowser, tc.verify)
			res, err := f.Verify(context.Background(), builtins.VerifyRequest{
				Provider: testRequest().Provider,
				Value:    builtins.StoreValue{Bearer: goodToken},
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, res.Status)
			if tc.wantDetail != "" {
				assert.Contains(t, res.Detail, tc.wantDetail)
			}
			assert.NotEmpty(t, res.Detail, "every non-Valid verdict must say why, per VerifyResult.Detail")
			assert.NotContains(t, res.Detail, goodToken, "the token must never appear in Detail")
			for _, v := range res.Metadata {
				assert.NotContains(t, v, goodToken, "the token must never appear in Metadata")
			}
		})
	}
}

// TestVerify_EmptyTokenNeverCallsVerifyFn guards the one input Flow.Verify
// must refuse before it ever reaches the injected seam: an empty bearer value
// has nothing to check, so calling verify with it would ask a real backend
// (or, in the default wiring, the registry) a meaningless question.
func TestVerify_EmptyTokenNeverCallsVerifyFn(t *testing.T) {
	called := false
	f := onepassword_service_account.NewWithVerify(noBrowser, func(context.Context, string) (directcred.Pin, error) {
		called = true
		return directcred.Pin{}, nil
	})
	res, err := f.Verify(context.Background(), builtins.VerifyRequest{
		Provider: testRequest().Provider,
		Value:    builtins.StoreValue{Bearer: ""},
	})
	require.NoError(t, err)
	assert.False(t, called, "an empty token must never reach the verify seam")
	assert.Equal(t, builtins.VerifyIndeterminate, res.Status)
	assert.NotEmpty(t, res.Detail)
}
