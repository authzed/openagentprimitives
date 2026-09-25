package installcmd

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// detectedAccept and detectedReview are the two answers to the rail's first
// screen: take every detected value as final, or walk each screen.
const (
	detectedAccept = "accept"
	detectedReview = "review"
)

// newDetectedScreen is the rail's first step: it only records the user's
// choice between accepting every detected setting and reviewing each one.
// The orchestrator reads st.Get(keyDetected) and, when detectedAccept, calls
// seedStateFromDetected so the following screens short-circuit — this screen
// must not (and does not) mutate any key but its own.
func newDetectedScreen(d DetectedSettings) tui.Screen {
	guidance := func(*tui.State) string {
		if !d.Existing {
			return "Fresh cluster — the steps below use detected cloud + recommended defaults."
		}
		spicedbSummary := ""
		if d.ExternalSpiceDB {
			spicedbSummary = "external " + d.ExternalSpiceDBEndpoint
		}
		var b strings.Builder
		b.WriteString("Existing install — current settings:\n")
		for _, kv := range [][2]string{
			{"email", d.ACMEEmail}, {"hostname", d.TrustedHostname}, {"workspace", d.WorkspaceClass},
			{"idp", d.IdPKind}, {"monitoring", d.MonitoringChannel}, {"spicedb", spicedbSummary},
		} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "  %-11s %s\n", kv[0], kv[1])
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	def := detectedReview
	if d.Existing {
		def = detectedAccept
	}
	return tui.NewChoice(tui.ChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID: keyDetected, Label: "Detected", Key: keyDetected,
			Title:    "How do you want to proceed?",
			Guidance: guidance,
		},
		Options: []tui.Choice{
			{Label: "Accept all → install", Value: detectedAccept},
			{Label: "Review each setting", Value: detectedReview},
		},
		Default: func() string { return def },
	})
}
