package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func phases(effs []Effect) (append, project, stop, notify bool) {
	for _, e := range effs {
		switch e.(type) {
		case AppendLog:
			append = true
		case ProjectStatus:
			project = true
		case StopLoop:
			stop = true
		case Notify:
			notify = true
		}
	}
	return
}

func TestTransitionProvisioning(t *testing.T) {
	cases := []struct {
		name        string
		in          State
		ev          Event
		wantPhase   Phase
		wantRegion  Region
		wantProject bool
		wantNotify  bool
		wantReason  string
	}{
		{"settings accepted: stays Pending", State{Phase: PhasePending}, SettingsAccepted{}, PhasePending, RegionOperatorPre, true, false, ""},
		{"creds missing: → AwaitingCredentials", State{Phase: PhasePending}, CredsMissing{}, PhaseAwaitingCredentials, RegionOperatorPre, true, false, ""},
		{"creds linked: → Pending", State{Phase: PhaseAwaitingCredentials}, CredsLinked{}, PhasePending, RegionOperatorPre, true, false, ""},
		{"creds timeout: → Failed", State{Phase: PhaseAwaitingCredentials}, CredsTimeout{}, PhaseFailed, RegionOperatorPre, true, false, "CredentialLinkTimeout"},
		{"unschedulable: notifies, stays Pending", State{Phase: PhasePending}, Unschedulable{}, PhasePending, RegionOperatorPre, true, true, ""},
		{"provably unschedulable: → Failed", State{Phase: PhasePending}, ProvablyUnschedulable{}, PhaseFailed, RegionOperatorPre, true, false, "Unschedulable"},
		{"pod ready: log only, stays Pending", State{Phase: PhasePending}, PodReady{}, PhasePending, RegionOperatorPre, false, false, ""},
		{"runner claims: → Running, region Runner", State{Phase: PhasePending}, RunnerClaimed{}, PhaseRunning, RegionRunner, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, effs := Transition(tc.in.OrDefault(), tc.ev)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantRegion, got.Region)
			ap, pr, _, no := phases(effs)
			assert.True(t, ap, "every transition appends to the log")
			assert.Equal(t, tc.wantProject, pr, "ProjectStatus emitted")
			assert.Equal(t, tc.wantNotify, no, "Notify emitted")
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, got.FailureReason)
			}
		})
	}
}
