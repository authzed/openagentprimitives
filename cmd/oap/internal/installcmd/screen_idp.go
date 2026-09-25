package installcmd

import "github.com/authzed/openagentprimitives/pkg/cli/tui"

// idpNone is the answer value that means "no identity provider — skip for
// now". It is not a registered idpregistry kind; it is this screen's own
// escape hatch.
const idpNone = "none"

// newIdPScreen offers the registered identity-provider kinds (gathered by the
// orchestrator from idpregistry.Names()), plus a trailing "none" choice.
// Choosing a real kind that differs from the current one runs
// identitycmd.RunIdpSetup, invoked by the orchestrator after this screen
// resolves.
func newIdPScreen(kinds []string, d DetectedSettings) tui.Screen {
	opts := make([]tui.Choice, 0, len(kinds)+1)
	for _, k := range kinds {
		label := k
		if k == d.IdPKind {
			label = k + " (current)"
		}
		opts = append(opts, tui.Choice{Label: label, Value: k})
	}
	opts = append(opts, tui.Choice{Label: "none — skip for now (run `oap idp setup <kind>` later)", Value: idpNone})
	return tui.NewChoice(tui.ChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        keyIdP,
			Label:     "Identity",
			Key:       keyIdP,
			Title:     "Connect an identity provider?",
			NoteLabel: "Identity provider",
		},
		Options: opts,
		Default: func() string {
			if d.IdPKind != "" {
				return d.IdPKind
			}
			return idpNone
		},
	})
}
