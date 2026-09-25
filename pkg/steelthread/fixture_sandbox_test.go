package steelthread_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// objectMeta gives a gathered CR the live namespace liveFixture uses, so the
// rewrite's namespace replacement has something to replace.
func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "acme-prod"}
}

// applyOrderOfKind returns the position, IN APPLY ORDER, of the emitted file
// carrying a document of the given kind — failing outright when none does.
//
// Apply order is the files sorted by NAME, not the order RewriteFixture
// happened to append them in. The two differ, and the difference is exactly
// what an ordering bug looks like: a file appended before the AgentClass but
// NAMED to sort after it is applied second, while a slice-position check sees
// it first and passes.
func applyOrderOfKind(t *testing.T, files []steelthread.FixtureFile, kind string) int {
	t.Helper()
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b steelthread.FixtureFile) int { return strings.Compare(a.Name, b.Name) })

	var names []string
	for i, f := range sorted {
		names = append(names, f.Name)
		if bytes.Contains(f.YAML, []byte("kind: "+kind+"\n")) {
			return i
		}
	}
	require.Failf(t, "kind not emitted", "no file carries a %s document; emitted %v", kind, names)
	return -1
}

// withSandboxBundles gives a live fixture the shape the sre session has, and it
// is the shape that matters: TWO toolBundles over ONE SpiceboxClass, whose two
// toolspecs both resolve to the SAME class tool. A bare class-tool key would
// collide between them.
func withSandboxBundles(f *steelthread.FixtureInput) {
	f.Class.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "demo-discovery", Class: "demo-sandbox", Toolspecs: []string{"demo-list"}},
		{Name: "demo-fetch", Class: "demo-sandbox", Toolspecs: []string{"demo-get"}},
	}
	f.SandboxClasses = []*spiceboxv1alpha1.SpiceboxClass{{
		ObjectMeta: objectMeta("demo-sandbox"),
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "demo-sandbox:dev",
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "demo", Command: []string{"/usr/local/bin/demo-tool"}},
			},
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "demo-list"}, {Name: "demo-get"}},
		},
	}}
	f.Toolspecs = []*spiceboxv1alpha1.SpiceboxToolspec{
		{
			ObjectMeta: objectMeta("demo-list"),
			Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
				Name: "demo-list", Version: "1",
				// The toolkit name is what matches a class tool, NOT the
				// toolspec's own name. Named differently from both on purpose.
				Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "demo", Revision: "2026-06-10"},
				AllowSubcommands: []string{"list"},
			},
		},
		{
			ObjectMeta: objectMeta("demo-get"),
			Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
				Name: "demo-get", Version: "1",
				Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "demo", Revision: "2026-06-10"},
				AllowSubcommands: []string{"get"},
				// A producer. RewriteFixture drops this; see the case below.
				SecretOutput: &spiceboxv1alpha1.ToolspecSecretOutput{
					Name: "credential", Source: "file:/out/credential",
					Description: "Fixture credential, never shown here.",
				},
			},
		},
	}
	f.Toolkits = []*spiceboxv1alpha1.SpiceboxToolkit{{
		ObjectMeta: objectMeta("demo"),
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "demo", Version: "1", ToolkitRevision: "2026-06-10",
			Target: spiceboxv1alpha1.ToolkitTarget{Binary: "demo-tool"},
		},
	}}
}

func TestSandboxTools_ResolvesAClassToolThroughTheToolspectsToolkitName(t *testing.T) {
	in := liveFixture(t, withSandboxBundles)

	got := steelthread.SandboxToolsForTest(in)
	assert.Equal(t, map[string][]string{
		"demo-discovery": {"demo"},
		"demo-fetch":     {"demo"},
	}, got,
		"a toolspec is matched to a class tool by the toolspec's TOOLKIT name; matching on the "+
			"toolspec's own name yields a plausible-looking name that matches nothing in the transcript")
}

// The prefix must survive even when the class/toolspec were never gathered.
// Dropping it would make every call to that bundle's tools read as unroutable,
// which points the reader at the wrong problem.
func TestSandboxTools_AnUngatheredBundleKeepsItsPrefixWithNoTools(t *testing.T) {
	in := liveFixture(t, withSandboxBundles)
	in.SandboxClasses = nil
	in.Toolspecs = nil

	got := steelthread.SandboxToolsForTest(in)
	require.Contains(t, got, "demo-discovery", "the prefix comes from the class's own toolBundles")
	assert.Empty(t, got["demo-discovery"],
		"nothing was gathered to resolve it to, and saying so is what distinguishes "+
			"sandbox-tool-unresolved from unroutable-tool-call")
}

func TestSandboxTools_EmptyForAnMCPOnlyClass(t *testing.T) {
	assert.Empty(t, steelthread.SandboxToolsForTest(liveFixture(t, nil)))
}

func TestRewriteFixture_Sandbox(t *testing.T) {
	rewritten, err := steelthread.RewriteFixture(liveFixture(t, withSandboxBundles))
	require.NoError(t, err, "RewriteFixture")
	files := rewritten.Files

	t.Run("all three CRs a sandbox tool is synthesized from are emitted", func(t *testing.T) {
		var names []string
		for _, f := range files {
			names = append(names, f.Name)
		}
		assert.Subset(t, names,
			[]string{"02b-spiceboxtoolkit.yaml", "02c-spiceboxtoolspec.yaml", "02d-spiceboxclass.yaml"},
			"without any one of these the replayed class offers no sandbox tool at all")
	})

	t.Run("they sort before the AgentClass that declares the toolBundles", func(t *testing.T) {
		// Located by KIND, not by filename: a rename that broke the ordering
		// would leave a filename-keyed assertion looking for a file that no
		// longer exists and passing vacuously.
		agentAt := applyOrderOfKind(t, files, "AgentClass")
		for _, kind := range []string{"SpiceboxToolkit", "SpiceboxToolspec", "SpiceboxClass"} {
			assert.Less(t, applyOrderOfKind(t, files, kind), agentAt,
				"the harness applies a directory's *.yaml in sorted order, so an AgentClass applied "+
					"before the %s its toolBundles resolve through would validate against an object "+
					"that does not exist yet", kind)
		}
	})

	t.Run("the toolkit rides through: synthesis TOLERATES a missing one and silently downgrades", func(t *testing.T) {
		var tk spiceboxv1alpha1.SpiceboxToolkit
		findDoc(t, files, "SpiceboxToolkit", "demo", &tk)
		assert.Equal(t, "demo-tool", tk.Spec.Target.Binary,
			"the toolkit carries the per-subcommand permission model the captured authorization came from")
	})

	t.Run("the toolspec's narrowing rides through", func(t *testing.T) {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		findDoc(t, files, "SpiceboxToolspec", "demo-list", &ts)
		assert.Equal(t, []string{"list"}, ts.Spec.AllowSubcommands,
			"allowSubcommands is where a captured session's authorization behavior comes from")
		assert.Equal(t, "demo", ts.Spec.Toolkit.Name)
	})

	t.Run("a producer's secretOutput is DROPPED", func(t *testing.T) {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		findDoc(t, files, "SpiceboxToolspec", "demo-get", &ts)
		assert.Nil(t, ts.Spec.SecretOutput,
			"the value came from a file a real tool wrote in a real pod; a canned stdout cannot produce "+
				"one, so a toolspec still declaring the output composes an error in place of the result")
		assert.Equal(t, []string{"get"}, ts.Spec.AllowSubcommands,
			"dropping the output must not disturb the narrowing")
	})

	t.Run("the class tool catalog rides through: the driver dispatches on its argv", func(t *testing.T) {
		var cls spiceboxv1alpha1.SpiceboxClass
		findDoc(t, files, "SpiceboxClass", "demo-sandbox", &cls)
		assert.Equal(t, []spiceboxv1alpha1.SpiceboxTool{
			{Name: "demo", Command: []string{"/usr/local/bin/demo-tool"}},
		}, cls.Spec.Tools)
	})
}

// RewriteFixture must not reach back into the caller's objects: Capture hands
// the same FixtureInput to the rewrite, to sandboxTools and to the self-check,
// and a mutation would make them disagree about what the session ran.
func TestRewriteFixture_SandboxDoesNotMutateTheCallersToolspecs(t *testing.T) {
	in := liveFixture(t, withSandboxBundles)
	_, err := steelthread.RewriteFixture(in)
	require.NoError(t, err)

	for _, ts := range in.Toolspecs {
		if ts.Name == "demo-get" {
			assert.NotNil(t, ts.Spec.SecretOutput,
				"RewriteFixture must not strip the secret output from the input it was handed")
		}
	}
}

// An unnamed CR would marshal to a nameless document and fail the apply many
// steps later, with nothing pointing back at the capture that emitted it.
func TestRewriteFixture_SandboxRefusesANamelessCR(t *testing.T) {
	in := liveFixture(t, withSandboxBundles)
	in.Toolspecs[0].Name = ""

	_, err := steelthread.RewriteFixture(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SpiceboxToolspec")
	assert.Contains(t, err.Error(), "metadata.name")
}
