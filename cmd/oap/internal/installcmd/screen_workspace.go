package installcmd

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// workspaceIsolatedValue is the Workspace screen's answer meaning "no shared
// RWX workspace — pod-local /work only" (N3). It mirrors the linear reselect
// picker's always-offered "isolated /work" choice (workspace_picker.go), which
// the wizard's Workspace screen otherwise has no equivalent for. It is a
// synthetic sentinel, never a real StorageClass name: the orchestrator
// (wizard_run.go) checks for it explicitly and folds it into
// WorkspaceResolveOptions.NoRWX rather than forwarding it as ExplicitClass,
// which would otherwise send resolveWorkspaceDecision off probing a
// nonexistent class called "isolated".
const workspaceIsolatedValue = "isolated"

// newWorkspaceScreen asks which RWX storage class backs the shared workspace.
// classes is the live set gathered by the orchestrator via
// cloud.ListRWXClasses; recommended is the orchestrator's recommendation for
// a fresh install (used as the default when nothing was detected).
//
// Labelling is delegated entirely to workspaceClassLabel
// (workspace_picker.go) so this screen and the classic picker never drift on
// how a class is described. The isolated /work choice is always appended
// last (N3), after any RWX classes and the not-found-marker entry, so it
// never becomes the "silence chooses this" first option — ChoiceOpts.Default
// documents that a nil/unmatched Default falls through to opts[0], and
// silently degrading a workspace to isolated on an unattended EOF would be
// exactly the kind of answer nobody gave.
func newWorkspaceScreen(classes []cloud.RWXClassInfo, d DetectedSettings, recommended string) tui.Screen {
	opts := make([]tui.Choice, 0, len(classes)+2)
	currentSeen := false
	for _, c := range classes {
		if c.Name == d.WorkspaceClass {
			currentSeen = true
		}
		opts = append(opts, tui.Choice{
			Label: workspaceClassLabel(c, d.WorkspaceClass, recommended),
			Value: c.Name,
		})
	}
	// A prior install may have pinned a custom RWX class whose provisioner the
	// heuristic (knownRWXProvisioners) doesn't recognize, so ListRWXClasses omits
	// it. Include it anyway, listed first — it is always a valid choice — so
	// accept-all's seeded value resolves and review-each can keep it, mirroring
	// the classic picker's "current — not found on cluster"
	// (workspace_picker.go). Without this, applyChoice fails closed on the seeded
	// marker class and the install errors out.
	if !currentSeen && d.WorkspaceClass != "" {
		opts = append([]tui.Choice{{
			Label: fmt.Sprintf("%s  (current — not found on cluster)", d.WorkspaceClass),
			Value: d.WorkspaceClass,
		}}, opts...)
	}
	opts = append(opts, tui.Choice{
		Label: "none — isolated /work (no shared workspace; multi-bundle agents lose shared state)",
		Value: workspaceIsolatedValue,
	})
	return tui.NewChoice(tui.ChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        keyWorkspaceClass,
			Label:     "Workspace",
			Key:       keyWorkspaceClass,
			Title:     "Workspace storage class (shared RWX)",
			NoteLabel: "Workspace",
		},
		Options: opts,
		Default: func() string {
			if d.WorkspaceClass != "" {
				return d.WorkspaceClass
			}
			return recommended
		},
	})
}
