package flowscreens_test

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
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
)

// runScreens drives screens through the sequencer exactly as a flow does, over
// a scripted stdin, and returns the answered State plus what the driver wrote.
//
// Every prompt the run reaches MUST have a script line: huh's accessible
// renderer cannot report a read error, so a short script silently answers the
// remainder from their bound defaults instead of failing.
func runScreens(t *testing.T, screens []tui.Screen, script []string, st *tui.State) (*tui.State, string, error) {
	t.Helper()
	in := ""
	if len(script) > 0 {
		in = strings.Join(script, "\n") + "\n"
	}
	var out bytes.Buffer
	got, err := tui.RunWith(context.Background(), screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(in),
		Out:   &out,
	}, st)
	return got, out.String(), err
}

func TestBrowser_Outcomes(t *testing.T) {
	const url = "https://example.test/tokens/new"

	cases := []struct {
		name      string
		opts      flowscreens.BrowserOpts
		seedGuard bool
		// noOpener leaves BrowserOpts.Open nil. A field rather than something
		// inferred from the row's name: a future row whose wording happened to
		// match would silently lose its opener and stop testing what it says.
		noOpener   bool
		wantOpened []string
		wantErr    string // substring of Err(), "" = Err() must be nil
		wantNotes  int
	}{
		{
			name:       "opens the page and reports no failure",
			opts:       flowscreens.BrowserOpts{URL: url, Guard: "cred", NoteLabel: "Browser"},
			wantOpened: []string{url},
		},
		{
			name:      "credential already supplied: nothing is opened at all",
			opts:      flowscreens.BrowserOpts{URL: url, Guard: "cred", NoteLabel: "Browser"},
			seedGuard: true,
		},
		{
			name:      "no opener wired: reported rather than panicking mid-flow",
			opts:      flowscreens.BrowserOpts{URL: url, Guard: "cred", NoteLabel: "Browser"},
			noOpener:  true,
			wantErr:   "cannot open a browser",
			wantNotes: 1,
		},
		{
			name:      "no address to open: reported rather than launching nothing",
			opts:      flowscreens.BrowserOpts{Guard: "cred", NoteLabel: "Browser"},
			wantErr:   "no address to open",
			wantNotes: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opened []string
			opts := tc.opts
			opts.ID, opts.Label = "browser", "Browser"
			if !tc.noOpener {
				opts.Open = func(u string) error { opened = append(opened, u); return nil }
			}
			s := flowscreens.NewBrowser(opts)

			st := tui.NewState()
			if tc.seedGuard {
				st.Set("cred", "already-have-one")
			}
			got, _, err := runScreens(t, []tui.Screen{s}, nil, st)
			require.NoError(t, err, "a browser step must never abort the run")

			assert.Equal(t, tc.wantOpened, opened)
			if tc.wantErr == "" {
				assert.NoError(t, s.Err())
			} else {
				require.Error(t, s.Err())
				assert.Contains(t, s.Err().Error(), tc.wantErr)
			}
			assert.Len(t, got.Notes(), tc.wantNotes,
				"the summary records the step only when it could not do its job")
		})
	}
}

// TestBrowser_RequiresInteraction is the declaration a caller consults BEFORE a
// run that was told not to prompt.
//
// Without it, such a run opens a tab and only then refuses at the screen that
// would have taken what the page produced — because this screen asks nothing,
// so a fail-closed driver never sees it. The two rows are each other's control:
// remove the guard check and the second row starts refusing runs that work.
//
// The guard-already-set row covers a flow whose earlier screens derive the key
// themselves. No shipped flow does — Browser is the first screen in both that
// use it — so the row is the only place that branch is exercised at all.
func TestBrowser_RequiresInteraction(t *testing.T) {
	cases := []struct {
		name       string
		seedGuard  bool
		wantReason bool
	}{
		{
			name:       "credential not in hand: refused, because the page is the only way to mint it",
			wantReason: true,
		},
		{
			name:      "an earlier screen already produced it: no refusal, because no page will be opened",
			seedGuard: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := flowscreens.NewBrowser(flowscreens.BrowserOpts{
				ID: "browser", Label: "Browser", URL: "https://example.test/new", Guard: "authkey",
				Open: func(string) error { return nil },
			})
			st := tui.NewState()
			if tc.seedGuard {
				st.Set("authkey", "already-have-one")
			}
			got := s.RequiresInteraction(st)
			if !tc.wantReason {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, "Re-run without asking to skip prompts",
				"the refusal must offer a way out that exists")
			assert.NotContains(t, got, "authkey",
				"naming the State key invites the reader to go supply it, and no oap command takes a setup answer from a flag")
			assert.NotContains(t, got, "tui:", "a refusal a user reads carries no framing of ours")
		})
	}
}

// TestBrowser_FailureIsReadableByTheNextScreen is the seam that keeps a failed
// open from being swallowed: the screen after it composes its guidance from
// Err(), which is where the user actually needs the address.
func TestBrowser_FailureIsReadableByTheNextScreen(t *testing.T) {
	b := flowscreens.NewBrowser(flowscreens.BrowserOpts{
		ID: "browser", Label: "Browser", URL: "https://example.test/new", Guard: "cred",
		Open: func(string) error { return errors.New("no display") },
	})
	in := tui.NewText(tui.TextOpts{QuestionOpts: tui.QuestionOpts{
		ID: "cred", Label: "Cred", Key: "cred", Title: "Paste it",
		Guidance: func(*tui.State) string {
			if err := b.Err(); err != nil {
				return "Your browser didn't open:\n  " + err.Error()
			}
			return ""
		},
	}})

	_, out, err := runScreens(t, []tui.Screen{b, in}, []string{"a-value"}, tui.NewState())
	require.NoError(t, err)
	assert.Contains(t, out, "no display",
		"the reason the browser failed must reach the screen where the user can act on it")
}

func TestRailedNoteBudgetOverflows(t *testing.T) {
	// 61 columns is the body width at an 80-column terminal; asserting a
	// specific number here would pin a layout constant this package does not
	// own, so the test builds strings relative to what it measures instead.
	short := strings.Repeat("x", 20)
	require.Empty(t, tui.RailedNoteBudget().Overflows(short), "a plainly short line must fit")

	long := strings.Repeat("y", 200)
	over := tui.RailedNoteBudget().Overflows(short + "\n" + long + "\n" + short)
	require.Len(t, over, 1, "only the over-wide line may be reported")
	assert.Equal(t, long, over[0])
}

// TestDocsAddress covers what this package still decides about a provider's
// documentation address: whether there is one at all. What HAPPENS to it —
// into the note when it fits, into the summary when it does not — is
// tui.Address's rule, asserted once there rather than per flow.
func TestDocsAddress(t *testing.T) {
	assert.Equal(t, tui.Address{}, flowscreens.DocsAddress(nil),
		"no provider means no address, not an address with an empty URL")

	got := flowscreens.DocsAddress(&provider.Provider{ID: "demo-provider", DocsURL: "https://docs.demo-provider.example"})
	assert.Equal(t, "https://docs.demo-provider.example", got.URL)
	assert.NotEmpty(t, got.Label, "an address with no label cannot be recorded in the summary")
}

func TestTokenShapeCheck(t *testing.T) {
	shaped := &provider.Provider{
		ID:         "demo-provider",
		TokenShape: &provider.TokenShape{Pattern: `^dm_`, Description: "starts with dm_"},
	}

	cases := []struct {
		name      string
		prov      *provider.Provider
		value     string
		errSubstr string // "" = must be accepted
	}{
		{name: "matching value: accepted", prov: shaped, value: "dm_abc"},
		{name: "mismatched value: refused, quoting the provider's own hint", prov: shaped, value: "xx_abc", errSubstr: "starts with dm_"},
		{name: "provider declares no shape: accepted, since we do not know the format", prov: &provider.Provider{ID: "p"}, value: "anything"},
		{name: "no provider at all: accepted, since there is no format to check against", prov: nil, value: "anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := flowscreens.TokenShapeCheck(tc.prov)(tui.NewState(), tc.value)
			if tc.errSubstr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}
