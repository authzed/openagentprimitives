//go:build darwin && arm64

package desktopcmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// noNodesBundle wraps a controller-runtime fake client into the *kube.Bundle
// installOapFromPath now takes, with an empty fake Typed clientset (no
// nodes). None of this file's fixtures carry a SpiceboxClass, so the
// capacity hook always finds nothing to check regardless of the ceiling this
// produces (unknown, for lack of any node).
func noNodesBundle(c client.Client) *kube.Bundle {
	return &kube.Bundle{Typed: k8sfake.NewSimpleClientset(), Controller: c}
}

// boolPtr is a tiny local helper — oap.Question.Required is a *bool so its
// zero value ("unset") reads as required (see Question.IsRequired), and the
// fixtures below need to flip a question to explicitly-not-required.
func boolPtr(b bool) *bool { return &b }

// pmAgentBundleNoRequiredQuestions loads the shared oaptest fixture bundle and
// neuters its only unanswerable-without-a-form question — "demoToken", a
// required secret with no manifest Default — by marking it not required, then
// packs it back into real .oap bytes. This is case (a): a packed bundle with
// NO unmet required question, so installOapFromPath should install it outright.
func pmAgentBundleNoRequiredQuestions(t *testing.T) []byte {
	t.Helper()
	b, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err, "load demo-agent fixture folder")
	for i := range b.Manifest.Questions {
		if b.Manifest.Questions[i].Name == "demoToken" {
			b.Manifest.Questions[i].Required = boolPtr(false)
		}
	}
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack mutated demo-agent fixture")
	return packed
}

// pmAgentBundleWithUnmetRequiredQuestion packs the UNMODIFIED oaptest fixture:
// "demoToken" stays required with no Default, so this is case (b)'s
// unmet-required-question bundle.
func pmAgentBundleWithUnmetRequiredQuestion(t *testing.T) []byte {
	t.Helper()
	b, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err, "load demo-agent fixture folder")
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack demo-agent fixture")
	return packed
}

// bundleWithDisallowedKind packs a bundle carrying a core Pod alongside its
// AgentClass, mirroring pkg/platform/oap.TestBundleValidate_RejectsDisallowedKinds'
// fixture shape. installOapFromPath's own b.Validate() call (ahead of
// install.Preflight) must reject this before any cluster write — case (c).
func bundleWithDisallowedKind(t *testing.T) []byte {
	t.Helper()
	b := &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "evil-agent", Version: "1.0.0"},
		},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentClass\n" +
			"metadata:\n" +
			"  name: evil-agent\n" +
			"spec:\n" +
			"  systemPrompt:\n" +
			"    inline: hi\n" +
			"---\n" +
			"apiVersion: v1\n" +
			"kind: Pod\n" +
			"metadata:\n" +
			"  name: evil\n" +
			"spec:\n" +
			"  containers:\n" +
			"  - name: c\n" +
			"    image: busybox\n"),
	}
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack disallowed-kind fixture")
	return packed
}

func TestInstallOapFromPath(t *testing.T) {
	ctx := context.Background()

	t.Run("no unmet required questions -> installs, AgentClass exists", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).Build()

		result, err := installOapFromPath(ctx, noNodesBundle(c), io.Discard, pmAgentBundleNoRequiredQuestions(t), "default")
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "demo-class", result.Name)
		assert.Contains(t, result.AppliedKinds, "AgentClass")

		var got spiceboxv1alpha1.AgentClass
		assert.NoError(t, c.Get(ctx, types.NamespacedName{Name: "demo-class", Namespace: "default"}, &got))
	})

	t.Run("unmet required question -> errNeedsConfig, nothing installed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).Build()

		result, err := installOapFromPath(ctx, noNodesBundle(c), io.Discard, pmAgentBundleWithUnmetRequiredQuestion(t), "default")
		require.ErrorIs(t, err, errNeedsConfig)
		assert.Nil(t, result)

		var got spiceboxv1alpha1.AgentClass
		err = c.Get(ctx, types.NamespacedName{Name: "demo-class", Namespace: "default"}, &got)
		assert.True(t, apierrors.IsNotFound(err), "AgentClass must not have been created")
	})

	t.Run("disallowed Kind (Pod) -> error, nothing installed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).Build()

		result, err := installOapFromPath(ctx, noNodesBundle(c), io.Discard, bundleWithDisallowedKind(t), "default")
		require.Error(t, err)
		assert.NotErrorIs(t, err, errNeedsConfig)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "disallowed resource kind")

		var got spiceboxv1alpha1.AgentClass
		err = c.Get(ctx, types.NamespacedName{Name: "evil-agent", Namespace: "default"}, &got)
		assert.True(t, apierrors.IsNotFound(err), "AgentClass must not have been created")
	})

	t.Run("oversized SpiceboxClass -> auto-fit clamps it, install succeeds with a warning", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).Build()
		kb := &kube.Bundle{
			Typed:      k8sfake.NewSimpleClientset(aptest.NodeWithCapacity("desktop-node", "1Gi")),
			Controller: c,
		}
		var logged bytes.Buffer

		result, err := installOapFromPath(ctx, kb, &logged, oversizedSpiceboxClassBundle(t), "default")
		require.NoError(t, err, "Interactive=false + a default from every capacity question means the install must never block")
		require.NotNil(t, result)
		require.NotEmpty(t, result.Warnings, "the clamp is the single most consequential outcome here and must never be silent")
		assert.Contains(t, result.Warnings[0], "demo-sandbox")
		assert.Contains(t, logged.String(), "demo-sandbox", "the notice must also reach out immediately, not just Result.Warnings")

		var got spiceboxv1alpha1.SpiceboxClass
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "demo-sandbox"}, &got), "SpiceboxClass is cluster-scoped: no namespace on the key")
		assert.NotEqual(t, "4Gi", got.Spec.Resources.Memory.String(), "the applied class must be clamped, not installed as bundled")
		assert.True(t, got.Spec.Resources.Memory.Value() <= 1*1024*1024*1024, "the clamped value must actually fit the 1Gi node")
	})

	t.Run("pre-existing foreign AgentClass, adopt declined -> ConflictError, no doubled 'install:' prefix", func(t *testing.T) {
		restore := desktopAdoptDialog
		t.Cleanup(func() { desktopAdoptDialog = restore })
		desktopAdoptDialog = func(string, string) bool { return false } // decline

		preExisting := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-class", Namespace: "default"},
		}
		c := fake.NewClientBuilder().WithScheme(newDemoAgentClassScheme(t)).WithObjects(preExisting).Build()

		result, err := installOapFromPath(ctx, noNodesBundle(c), io.Discard, pmAgentBundleNoRequiredQuestions(t), "default")
		require.Error(t, err)
		assert.Nil(t, result)
		var ce *install.ConflictError
		require.ErrorAs(t, err, &ce, "declining adoption must surface the library's own *ConflictError, unwrapped")
		// installOapFromPath used to wrap this with a second "install: %w",
		// producing "install: install: refusing to overwrite…" — the error
		// already carries the library's own "install: " prefix (see
		// ConflictError.Error()), so a re-wrap here would double it.
		assert.NotContains(t, err.Error(), "install: install:", "installOapFromPath must not re-prefix an error install.Install already prefixed")
	})
}

// oversizedSpiceboxClassBundle packs a minimal AgentClass + a SpiceboxClass
// that requests far more memory (4Gi) than any node in these tests reports —
// the fixture for the desktop auto-fit path. A made-up fixture name, never an
// example's name.
func oversizedSpiceboxClassBundle(t *testing.T) []byte {
	t.Helper()
	b := &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: "demo-agent", Version: "1.0.0"},
		},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: AgentClass\n" +
			"metadata:\n" +
			"  name: demo-class\n" +
			"spec:\n" +
			"  systemPrompt:\n" +
			"    inline: hi\n" +
			"---\n" +
			"apiVersion: agentprimitives.authzed.com/v1alpha1\n" +
			"kind: SpiceboxClass\n" +
			"metadata:\n" +
			"  name: demo-sandbox\n" +
			"spec:\n" +
			"  resources:\n" +
			"    memory: \"4Gi\"\n"),
	}
	packed, err := oap.Pack(b)
	require.NoError(t, err, "pack oversized-SpiceboxClass fixture")
	return packed
}

func TestInstallDialogText_PrefersDisplayNameThenName(t *testing.T) {
	withName := &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "raw-name", Version: "1"}}}
	title, _ := installDialogText(withName)
	assert.Equal(t, "raw-name", title, "falls back to agent.name when displayName is empty")

	withDisplay := &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{
		Name: "raw-name", Version: "1", DisplayName: "Pretty Name", Description: "does things",
	}}}
	title, msg := installDialogText(withDisplay)
	assert.Equal(t, "Pretty Name", title)
	assert.Contains(t, msg, "does things")
}

func TestAdoptDialogText(t *testing.T) {
	title, msg := adoptDialogText([]install.Conflict{
		{Kind: "AgentClass", Namespace: "demo", Name: "demo-agent"},
		{Kind: "Secret", Namespace: "demo", Name: "demo-token", Secret: true},
	})
	assert.Contains(t, title, "Adopt")
	assert.Contains(t, msg, "AgentClass demo/demo-agent")
	assert.Contains(t, msg, "Secret demo/demo-token")
	assert.Contains(t, msg, "overwrites this Secret's data")
	assert.Contains(t, msg, "uninstall")
}

func TestDesktopAdoptDecision(t *testing.T) {
	conflicts := []install.Conflict{{Kind: "AgentClass", Namespace: "demo", Name: "demo-agent"}}

	t.Run("confirmed: every conflict is adopted", func(t *testing.T) {
		restore := desktopAdoptDialog
		t.Cleanup(func() { desktopAdoptDialog = restore })
		desktopAdoptDialog = func(string, string) bool { return true }

		got, err := desktopAdoptDecision(context.Background(), conflicts)
		require.NoError(t, err)
		assert.Equal(t, []string{"AgentClass/demo-agent"}, got)
	})

	t.Run("cancelled: nothing adopted, install aborts with a ConflictError", func(t *testing.T) {
		restore := desktopAdoptDialog
		t.Cleanup(func() { desktopAdoptDialog = restore })
		desktopAdoptDialog = func(string, string) bool { return false }

		got, err := desktopAdoptDecision(context.Background(), conflicts)
		require.NoError(t, err, "cancelling is not an error — returning no keys lets Install report the conflicts")
		assert.Empty(t, got)
	})
}

// TestReadmeTempPath_StaysInsideTempDir pins the confinement of the README
// preview path. The agent name comes from a .oap manifest that oap.Unpack
// never validates (it verifies blob digests and hardens layer extraction, but
// runs no Manifest.Validate), and the preview dialog reaches openReadme BEFORE
// installOapFromPath — the one step that does validate. filepath.Join Cleans,
// so a "../"-laden name would otherwise write a *-README.md into a directory
// of the bundle author's choosing, from the action that exists to be the safe
// look-before-you-install one.
func TestReadmeTempPath_StaysInsideTempDir(t *testing.T) {
	cases := []struct {
		name  string
		agent string
	}{
		{name: "parent traversal: confined to dir", agent: "../../../../etc/evil"},
		{name: "absolute path: confined to dir", agent: "/etc/evil"},
		{name: "nested separators: confined to dir", agent: "a/b/c"},
		{name: "bare dot-dot: confined to dir", agent: ".."},
		{name: "empty name: confined to dir", agent: ""},
		{name: "ordinary name: unchanged", agent: "demo-agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			got := readmeTempPath(dir, tc.agent)

			assert.Equal(t, dir, filepath.Dir(got), "the file lands directly in dir, never outside it")
			assert.True(t, strings.HasSuffix(got, "-README.md"), "the rendered-markdown suffix is preserved")
		})
	}

	t.Run("ordinary name keeps its own basename", func(t *testing.T) {
		assert.Equal(t, filepath.Join("/tmp", "demo-agent-README.md"), readmeTempPath("/tmp", "demo-agent"))
	})
}
