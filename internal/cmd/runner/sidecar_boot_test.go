package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestSidecarBootDecision pins what the boot pass does with each shape of
// resolved sidecar. The load-bearing rows are the terminally failed ones: a
// regression there turns a recoverable sidecar crash back into a failed
// session, either at once or after the full IP poll.
func TestSidecarBootDecision(t *testing.T) {
	crashed := &spiceboxv1alpha1.SidecarPodFailure{Reason: "CrashLoopBackOff", Message: "boom"}

	cases := []struct {
		name        string
		rt          spiceboxv1alpha1.ResolvedSidecarToolbox
		urlResolved bool
		want        sidecarBootAction
	}{
		{
			name:        "separate-pod with an IP: probe",
			rt:          sepPod("k8s", false, "10.0.0.5"),
			urlResolved: true,
			want:        sidecarBootProbe,
		},
		{
			name: "separate-pod with an IP and a stale PodFailure: probe",
			rt: func() spiceboxv1alpha1.ResolvedSidecarToolbox {
				rt := sepPod("k8s", false, "10.0.0.5")
				rt.PodFailure = crashed
				return rt
			}(),
			urlResolved: true,
			want:        sidecarBootProbe,
		},
		{
			name: "separate-pod terminally failed with no IP: skip, not a failed session",
			rt:   failedPod("k8s", crashed.Reason, crashed.Message),
			want: sidecarBootSkip,
		},
		{
			name: "separate-pod still starting (no IP, no failure): poll",
			rt:   sepPod("k8s", false, ""),
			want: sidecarBootPoll,
		},
		{
			name: "separate-pod awaiting its secret: skip",
			rt:   sepPod("k8s", true, ""),
			want: sidecarBootSkip,
		},
		{
			name:        "in-pod with its port env set: probe",
			rt:          spiceboxv1alpha1.ResolvedSidecarToolbox{Name: "echo", RunMode: "in-pod"},
			urlResolved: true,
			want:        sidecarBootProbe,
		},
		{
			name: "in-pod with no port env: boot failure",
			rt:   spiceboxv1alpha1.ResolvedSidecarToolbox{Name: "echo", RunMode: "in-pod"},
			want: sidecarBootFail,
		},
		{
			name: "unset run mode with no port env: boot failure, same as in-pod",
			rt:   spiceboxv1alpha1.ResolvedSidecarToolbox{Name: "echo"},
			want: sidecarBootFail,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sidecarBootDecision(tc.rt, tc.urlResolved))
		})
	}
}
