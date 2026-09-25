package installcmd

import "github.com/authzed/openagentprimitives/pkg/cli/tui"

// newSpiceDBScreen asks whether to use an existing external SpiceDB instance
// instead of installing the bundled one. It resolves only the decision: the
// endpoint/token/insecure follow-ups (resolveExternalSpiceDB, init.go) are a
// second, conditional group invoked by the orchestrator when this confirms
// true and no --external-spicedb-endpoint flag was given.
func newSpiceDBScreen(d DetectedSettings) tui.Screen {
	return tui.NewConfirm(tui.ConfirmOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        keyExternalSpiceDB,
			Label:     "SpiceDB",
			Key:       keyExternalSpiceDB,
			Title:     "Use an existing external SpiceDB instance instead of installing one?",
			NoteLabel: "SpiceDB",
			NoteValue: func(v string) string {
				if v == "yes" {
					return "external"
				}
				return "bundled"
			},
		},
		Default: d.ExternalSpiceDB,
	})
}
