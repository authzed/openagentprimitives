package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestWorkspaceScreen_DefaultAndSeed(t *testing.T) {
	classes := []cloud.RWXClassInfo{
		{Name: "ap-workspace-rwx", Bundled: true},
		{Name: "enterprise-multishare-rwx", Filestore: true, Multishare: true},
	}
	st := tui.NewState()
	st.Set(keyWorkspaceClass, "ap-workspace-rwx") // accept-all seed
	g, err := newWorkspaceScreen(classes, DetectedSettings{}, "ap-workspace-rwx").Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.Nil(t, g)

	st2 := tui.NewState()
	g2, err := newWorkspaceScreen(classes, DetectedSettings{WorkspaceClass: "ap-workspace-rwx"}, "ap-workspace-rwx").Prepare(context.Background(), st2)
	require.NoError(t, err)
	assert.NotNil(t, g2)
}

// TestWorkspaceScreen_MarkerClassNotInList pins the m3 fix: a prior install may
// have pinned a custom RWX class ("my-nfs") whose provisioner the heuristic
// doesn't recognize, so ListRWXClasses omits it. The Workspace screen must still
// offer the current/marker class, or accept-all's seeded value fails closed in
// Apply's applyChoice ("my-nfs is not one of the choices") and review-each can't
// keep it. classes below deliberately EXCLUDE the marker.
func TestWorkspaceScreen_MarkerClassNotInList(t *testing.T) {
	classes := []cloud.RWXClassInfo{
		{Name: "enterprise-multishare-rwx", Filestore: true, Multishare: true},
	}
	d := DetectedSettings{WorkspaceClass: "my-nfs"}

	// Accept-all path: the marker class is seeded; Prepare short-circuits and
	// Apply must accept it because the screen included it as an option.
	st := tui.NewState()
	st.Set(keyWorkspaceClass, "my-nfs")
	screen := newWorkspaceScreen(classes, d, "")
	g, err := screen.Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.Nil(t, g, "a seeded marker class short-circuits the screen")
	require.NoError(t, screen.Apply(context.Background(), st),
		"the marker class must be a valid option even when ListRWXClasses omits it")
	assert.Equal(t, "my-nfs", st.Get(keyWorkspaceClass), "the kept marker class survives Apply")
}

// TestWorkspaceScreen_OffersIsolatedOption is N3's regression: the Workspace
// screen must offer a "no shared workspace" choice alongside the RWX
// classes, mirroring the linear reselect picker's always-present isolated
// /work entry. Proven by seeding the sentinel value and checking Apply
// accepts it (the same way TestWorkspaceScreen_MarkerClassNotInList proves an
// option exists) rather than inspecting the built tui.Choice slice directly.
func TestWorkspaceScreen_OffersIsolatedOption(t *testing.T) {
	classes := []cloud.RWXClassInfo{
		{Name: "ap-workspace-rwx", Bundled: true},
	}
	st := tui.NewState()
	st.Set(keyWorkspaceClass, workspaceIsolatedValue)
	screen := newWorkspaceScreen(classes, DetectedSettings{WorkspaceClass: "ap-workspace-rwx"}, "")
	g, err := screen.Prepare(context.Background(), st)
	require.NoError(t, err)
	assert.Nil(t, g, "a seeded isolated pick short-circuits the screen like any other seeded answer")
	require.NoError(t, screen.Apply(context.Background(), st),
		"the isolated sentinel must be a valid option so a seeded/kept isolated pick survives Apply")
	assert.Equal(t, workspaceIsolatedValue, st.Get(keyWorkspaceClass))
}

// TestWorkspaceScreen_IsolatedNeverTheSilentDefault guards the doc comment on
// newWorkspaceScreen: the isolated option is always appended LAST, so an
// unmatched/nil Default (an unattended EOF's fabricated pick,
// ChoiceOpts.Default) still resolves to the first REAL class, never to
// isolated. Degrading a shared workspace to isolated is a decision a human
// must make, not one silence should ever make for them.
func TestWorkspaceScreen_IsolatedNeverTheSilentDefault(t *testing.T) {
	classes := []cloud.RWXClassInfo{
		{Name: "ap-workspace-rwx", Bundled: true},
		{Name: "enterprise-multishare-rwx", Filestore: true, Multishare: true},
	}
	// Nothing detected and no recommendation: Default() resolves to "", which
	// matches no option, so Prepare's prepareValue falls through to opts[0].
	st := tui.NewState()
	screen := newWorkspaceScreen(classes, DetectedSettings{}, "")
	g, err := screen.Prepare(context.Background(), st)
	require.NoError(t, err)
	require.NotNil(t, g, "nothing is seeded, so the screen must present")

	// Apply reads the field's current value (set by prepareValue's fallback to
	// opts[0]) since nothing was interactively changed in this test.
	require.NoError(t, screen.Apply(context.Background(), st))
	assert.Equal(t, "ap-workspace-rwx", st.Get(keyWorkspaceClass),
		"silence must resolve to the first real class, never the isolated sentinel")
}
