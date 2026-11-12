package main

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// sidecarBootAction is what the boot pass does with one resolved sidecar.
type sidecarBootAction int

const (
	// sidecarBootProbe: the probe URL resolved; probe and synthesize its tools.
	sidecarBootProbe sidecarBootAction = iota
	// sidecarBootSkip: boot without this sidecar. The mid-session refresher
	// picks it up if it later becomes ready, and reports a terminal failure.
	sidecarBootSkip
	// sidecarBootPoll: a separate-pod sidecar whose pod is still starting; wait
	// for the operator to reflect its PodIP.
	sidecarBootPoll
	// sidecarBootFail: the sidecar can never be reached; fail the session.
	sidecarBootFail
)

// sidecarBootDecision is the pure core of the boot pass's per-sidecar branch:
// given a resolved sidecar and whether sidecarProbeURL resolved a URL for it,
// what should boot do. The boot loop calls it again on each refreshed snapshot
// while polling, so a sidecar that turns terminal mid-poll is skipped rather
// than waited out and failed.
//
// A resolved URL wins over everything — a PodFailure left on a snapshot whose
// pod has an IP is stale. A separate-pod sidecar the operator recorded as
// terminally failed (a crash loop, an unpullable image) will not become Ready,
// so polling for its IP would only end in failing the whole session; boot
// without it instead. An in-pod sidecar with no URL is missing its MCP_PORT
// env, which nothing will supply later.
func sidecarBootDecision(rt spiceboxv1alpha1.ResolvedSidecarToolbox, urlResolved bool) sidecarBootAction {
	switch {
	case urlResolved:
		return sidecarBootProbe
	case rt.AwaitingSecret:
		return sidecarBootSkip
	case rt.RunMode != "separate-pod":
		return sidecarBootFail
	case rt.PodFailure != nil:
		return sidecarBootSkip
	default:
		return sidecarBootPoll
	}
}
