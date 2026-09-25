package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmbeddedToolkitAsCR_CarriesPermissionTitles guards a silent-drop bug.
//
// An embedded toolkit reaches the controller by marshalling the LIBRARY struct
// (toolkit.SpiceDBPermission) and unmarshalling it into the CR spec
// (v1alpha1.SpiceDBPermission). Those are two different types, and `title` was
// added to the CR one first: the YAML declared titles, the marshal dropped them,
// nothing errored, and every card fell back to the detokenized handle exactly as
// though no title had ever been written.
//
// That failure is invisible — a green build, a green suite, and a card that is
// merely less good than it should be. This test is the thing that notices.
func TestEmbeddedToolkitAsCR_CarriesPermissionTitles(t *testing.T) {
	tk, found, err := embeddedToolkitAsCR("git")
	require.NoError(t, err, "the embedded git toolkit must convert to a CR")
	require.True(t, found, "the git toolkit ships embedded")
	require.NotNil(t, tk.Spec.SpiceDBSchema, "it declares a SpiceDB schema")

	titles := map[string]string{}
	for _, r := range tk.Spec.SpiceDBSchema.Resources {
		for _, p := range r.Permissions {
			titles[r.Name+"/"+p.Name] = p.Title
		}
	}

	assert.Equal(t, "Push commits to the remote repository", titles["git_repo/push"],
		"a declared title must survive the library->CR conversion, or every card silently falls back")
	assert.NotEmpty(t, titles["git_repo/read"], "read declares one too")
}

// TestEmbeddedToolkitAsCR_CarriesResourceDisplay guards the identical
// silent-drop bug for the resource-display field. `display` was added to the
// CR-side SpiceDBResource and, for a moment, only there: the YAML declared
// one, the library->CR marshal dropped it, nothing errored, and every card
// would have fallen back to the wire type name exactly as though no display
// had ever been written. See TestEmbeddedToolkitAsCR_CarriesPermissionTitles,
// which exists because this exact class of bug already happened once on this
// branch for Title.
func TestEmbeddedToolkitAsCR_CarriesResourceDisplay(t *testing.T) {
	tk, found, err := embeddedToolkitAsCR("git")
	require.NoError(t, err, "the embedded git toolkit must convert to a CR")
	require.True(t, found, "the git toolkit ships embedded")
	require.NotNil(t, tk.Spec.SpiceDBSchema, "it declares a SpiceDB schema")

	var display *struct{ Name, Icon, Label string }
	for _, r := range tk.Spec.SpiceDBSchema.Resources {
		if r.Name != "git_repo" {
			continue
		}
		require.NotNil(t, r.Display, "git_repo declares a display block")
		display = &struct{ Name, Icon, Label string }{r.Display.Name, r.Display.Icon, r.Display.Label}
	}
	require.NotNil(t, display, "git_repo must be present in the embedded schema")

	assert.NotEmpty(t, display.Name, "a display block declares a human name for the type")
	assert.NotEmpty(t, display.Icon,
		"a display block declares an icon; a missing one here means the marshal dropped it")
	assert.Equal(t, "url_path", display.Label,
		"git_repo derives its label from the instance URL's path")
}

// Standing must survive the embedded-toolkit round-trip.
//
// This is the field that had ALREADY been dropped for real: Standing lived only
// on the CR type, so no builtin toolkit could declare it, and git_repo's
// standing came from the CRD default rather than from git.yaml. Nothing
// errored, and the only symptom was an approval router that could not tell a
// governed type from an ungoverned one.
//
// With the default gone the drop is fatal instead of silent — an undeclared
// resource is refused — but a guard here names the cause directly rather than
// leaving someone to trace "declares no standing" back to a missing struct tag.
//
// Asserted against the REAL embedded git toolkit, so it also pins that
// git.yaml keeps saying what it means.
func TestEmbeddedToolkitAsCR_CarriesStanding(t *testing.T) {
	tk, found, err := embeddedToolkitAsCR("git")
	require.NoError(t, err)
	require.True(t, found, "the git toolkit is embedded")
	require.NotNil(t, tk.Spec.SpiceDBSchema)

	var seen bool
	for _, r := range tk.Spec.SpiceDBSchema.Resources {
		if r.Name != "git_repo" {
			continue
		}
		seen = true
		assert.Equal(t, "session-only", r.Standing,
			"git.yaml declares session-only; a standing lost in transit leaves the type undeclared and refused")
		assert.Empty(t, r.ApproverPermission,
			"session-only consults no permission and must not acquire one crossing the round-trip")
	}
	assert.True(t, seen, "git_repo must reach the CR at all")
}

// TestEmbeddedToolkitAsCR_CarriesPlanningNotes guards the same silent-drop bug
// as its two siblings above, for the third field to ride this conversion.
//
// Its failure is the quietest of the three: nothing errors, no card looks
// wrong, and the agent simply never receives the guidance.
func TestEmbeddedToolkitAsCR_CarriesPlanningNotes(t *testing.T) {
	tk, found, err := embeddedToolkitAsCR("git")
	require.NoError(t, err, "the embedded git toolkit must convert to a CR")
	require.True(t, found, "the git toolkit ships embedded")
	require.NotNil(t, tk.Spec.SpiceDBSchema, "it declares a SpiceDB schema")

	notes := map[string]string{}
	for _, r := range tk.Spec.SpiceDBSchema.Resources {
		for _, p := range r.Permissions {
			notes[r.Name+"/"+p.Name] = p.PlanningNote
		}
	}

	assert.NotEmpty(t, notes["git_repo/write"],
		"write declares the note that keeps a committing phase from omitting read")
	assert.Contains(t, notes["git_repo/write"], "read",
		"and it must actually name the permission it wants declared alongside")
	assert.NotEmpty(t, notes["git_repo/push"], "push declares one too — it names the remote, not the checkout")
}
