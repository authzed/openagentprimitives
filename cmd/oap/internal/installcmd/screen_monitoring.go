package installcmd

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// monitoringKeepValue, monitoringSetupValue, and monitoringSkipValue are the
// answer values for the monitoring screen. monitoringKeepValue is also what
// seedStateFromDetected (accept_existing.go) seeds when an existing
// monitoring channel was detected, so this screen owns the constant every
// caller of that seed relies on.
const (
	monitoringKeepValue  = "keep"
	monitoringSetupValue = "setup"
	monitoringSkipValue  = "skip"
)

// newMonitoringScreen asks whether to keep, (re)configure, or skip a
// monitoring channel. Options mirror monitoringOfferOptions (init.go): when a
// channel already exists, keep is offered and is the default; otherwise the
// only choices are setup and skip, with skip default. Choosing setup runs
// channelcmd.RunMonitoringCreate, invoked by the orchestrator after this
// screen resolves.
func newMonitoringScreen(existing []spiceboxv1alpha1.Channel, d DetectedSettings) tui.Screen {
	var opts []tui.Choice
	def := monitoringSkipValue
	if len(existing) > 0 {
		opts = append(opts, tui.Choice{Label: "keep existing monitoring channel", Value: monitoringKeepValue})
		def = monitoringKeepValue
	}
	opts = append(opts,
		tui.Choice{Label: "set up / reconfigure a monitoring channel", Value: monitoringSetupValue},
		tui.Choice{Label: "none — skip for now (run `oap channel create` later)", Value: monitoringSkipValue},
	)
	return tui.NewChoice(tui.ChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        keyMonitoring,
			Label:     "Monitoring",
			Key:       keyMonitoring,
			Title:     "Monitoring channel?",
			NoteLabel: "Monitoring",
		},
		Options: opts,
		Default: func() string { return def },
	})
}
