package kubectl_kubeconfig_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/kubectl_kubeconfig"
)

const sampleKubeconfig = `apiVersion: v1
kind: Config
clusters: []
contexts: []
users: []
current-context: ""
`

// writeFile drops content into a fresh temp dir and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

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

	f := kubectl_kubeconfig.New()
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
			ID:      "kubectl-kubeconfig",
			DocsURL: "https://kubernetes.io/docs/concepts/configuration/organize-cluster-access-kubeconfig/",
			Prompt:  "walk the user through narrowing a kubeconfig",
		},
		IdentityName: "demo-bot",
		Namespace:    "demo-ns",
	}
}

func TestName(t *testing.T) {
	assert.Equal(t, "kubectl-kubeconfig", kubectl_kubeconfig.New().Name())
}

func TestScreensAreStableAndNamed(t *testing.T) {
	screens, err := kubectl_kubeconfig.New().Screens(context.Background(), testRequest())
	require.NoError(t, err)

	var ids []string
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	assert.Equal(t, []string{"kubeconfig"}, ids)
}

// TestTypedPathStoresTheFilesContents is the happy path, asserted on the
// credential rather than on the absence of an error.
func TestTypedPathStoresTheFilesContents(t *testing.T) {
	path := writeFile(t, "config", sampleKubeconfig)

	stored, out, err := run(t, testRequest(), []string{path}, nil)
	require.NoError(t, err, "flow run")
	assert.Equal(t, sampleKubeconfig, stored.KubeconfigYAML,
		"the whole file is the credential, byte for byte")
	assert.Contains(t, out, "demo-bot", "the user must be told which identity they are choosing a config for")
}

// TestSeededPathStoresTheFilesContents: a path supplied ahead of time is read
// and checked exactly like a typed one, which is the shape a non-interactive
// run arrives in.
func TestSeededPathStoresTheFilesContents(t *testing.T) {
	path := writeFile(t, "config", sampleKubeconfig)

	stored, _, err := run(t, testRequest(), nil, map[string]string{kubectl_kubeconfig.KeyPath: path})
	require.NoError(t, err)
	assert.Equal(t, sampleKubeconfig, stored.KubeconfigYAML)
}

// TestRefusals covers every way this flow must decline to store. Each case
// would otherwise leave a credential that cannot reach a cluster — or, in the
// empty-input case, an entirely blank one.
func TestRefusals(t *testing.T) {
	notAConfig := writeFile(t, "pod.yaml", "apiVersion: v1\nkind: Pod\n")
	notYAML := writeFile(t, "junk", "\tthis: is: not: yaml\n  - [oops\n")
	empty := writeFile(t, "empty", "   \n")

	cases := []struct {
		name      string
		script    []string
		seed      map[string]string
		errSubstr string
	}{
		{
			name: "a manifest that is not a kubeconfig: refused, naming what it actually is",
			// Two lines because the field's validator re-prompts a rejected
			// value; the second is what the run ends up carrying.
			script:    []string{notAConfig, notAConfig},
			errSubstr: `is not a kubeconfig`,
		},
		{
			name:      "a path that does not exist: refused, and the file is named",
			script:    []string{filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "nope")},
			errSubstr: "could not read the kubeconfig",
		},
		{
			name:      "a file that is not YAML at all: refused",
			script:    []string{notYAML, notYAML},
			errSubstr: "not valid YAML",
		},
		{
			name:      "an empty file: refused rather than storing a blank credential",
			script:    []string{empty, empty},
			errSubstr: "is empty",
		},
		{
			name:      "a seeded path that is not a kubeconfig: refused, since seeding skips the question and not the check",
			seed:      map[string]string{kubectl_kubeconfig.KeyPath: notAConfig},
			errSubstr: "is not a kubeconfig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, _, err := run(t, testRequest(), tc.script, tc.seed)
			require.Error(t, err, "the flow must refuse this input")
			assert.Contains(t, err.Error(), tc.errSubstr)
			assert.Empty(t, stored.KubeconfigYAML, "nothing may be stored on a refusal")
		})
	}
}

// TestBareEnterTakesTheKubeconfigTheShellIsUsing: the field is pre-filled with
// $KUBECONFIG, which is the answer nearly every user wants, so accepting it
// must take one keystroke and must not be mistaken for an empty answer.
func TestBareEnterTakesTheKubeconfigTheShellIsUsing(t *testing.T) {
	path := writeFile(t, "config", sampleKubeconfig)
	t.Setenv("KUBECONFIG", path)

	stored, _, err := run(t, testRequest(), []string{""}, nil)
	require.NoError(t, err, "a bare Enter must accept the pre-filled path")
	assert.Equal(t, sampleKubeconfig, stored.KubeconfigYAML)
}

// TestOnlyTheFirstEntryOfAKubeconfigListIsOffered: KUBECONFIG may name several
// files to merge, which is not a single file this flow could store. Offering
// the first as a correctable starting point beats guessing at the merge.
func TestOnlyTheFirstEntryOfAKubeconfigListIsOffered(t *testing.T) {
	first := writeFile(t, "first", sampleKubeconfig)
	second := writeFile(t, "second", strings.Replace(sampleKubeconfig, `current-context: ""`, `current-context: "other"`, 1))
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)

	stored, _, err := run(t, testRequest(), []string{""}, nil)
	require.NoError(t, err)
	assert.Equal(t, sampleKubeconfig, stored.KubeconfigYAML, "the first entry is what is offered")
}

// TestTildeIsExpanded: a shell expands ~ before a program sees an argument, so
// a path TYPED at a prompt is the one place it still has to be done by hand.
//
// Both separators are covered because people type "~/.kube/config" on Windows
// too — it is what kubectl's own documentation shows — and gating on the
// platform separator alone would leave that unexpanded there.
func TestTildeIsExpanded(t *testing.T) {
	cases := []struct {
		name string
		// rel is the path under the fake home the file is created at, and
		// typed is what the user enters.
		rel, typed string
	}{
		{name: "~/name: expanded against the home directory", rel: "config", typed: "~/config"},
		{name: `~\name: expanded too, since that is what a Windows user types`, rel: "config", typed: `~\config`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, tc.rel), []byte(sampleKubeconfig), 0o600))

			stored, _, err := run(t, testRequest(), []string{tc.typed}, nil)
			require.NoError(t, err, "a tilde path must resolve to a file that exists")
			assert.Equal(t, sampleKubeconfig, stored.KubeconfigYAML)
		})
	}
}

// TestGuidanceFitsTheNoteWidth: everything this flow composes has to render
// without huh wrapping it. The provider's documentation address is far too long
// for a note, so what appears there is the address CUT to the budget and marked
// — never the whole one, which huh would break into two halves that each look
// like an address and neither of which works.
func TestGuidanceFitsTheNoteWidth(t *testing.T) {
	budget := tui.RailedNoteBudget()
	path := writeFile(t, "config", sampleKubeconfig)

	_, out, err := run(t, testRequest(), []string{path}, nil)
	require.NoError(t, err)
	for _, line := range budget.Overflows(out) {
		assert.Fail(t, "a composed guidance line is too wide for a note", "%q (%d columns)", line, len([]rune(line)))
	}
	// Asserted on the URL itself, not on a "Docs: " prefix: the label sits on
	// its own line, so a prefixed assertion would hold whether or not the
	// address was rendered — which is the failure this line exists to catch.
	docs := testRequest().Provider.DocsURL
	require.NotEmpty(t, docs, "this provider must document an address, or the test proves nothing")
	require.False(t, flowscreens.DocsAddress(testRequest().Provider).Fits(budget),
		"this provider's address must be too wide for a note, or the test proves nothing")
	// Both halves of the rule, because either alone passes for the wrong
	// reason: NotContains alone is satisfied by an address withheld entirely,
	// and Contains alone by one rendered whole and wrapped.
	assert.NotContains(t, out, docs,
		"an address too long to render whole is never put in a note whole")
	assert.Contains(t, out, budget.Elide(docs),
		"it is cut to the budget and marked, so the note still names where to go")
}

func TestVerifyIsUnsupported(t *testing.T) {
	res, err := kubectl_kubeconfig.New().Verify(context.Background(), builtins.VerifyRequest{
		Value: builtins.StoreValue{KubeconfigYAML: sampleKubeconfig},
	})
	require.NoError(t, err)
	assert.Equal(t, builtins.VerifyUnsupported, res.Status)
}
