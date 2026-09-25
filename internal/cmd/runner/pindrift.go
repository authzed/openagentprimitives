// Package main — pindrift.go contains the pure session-start pin-drift
// enforcement decision helper. Logic is isolated here so it can be unit-tested
// without the full runner entrypoint.
package main

import spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

// pinDriftAction maps the effective pinning mode for a drifted dependency to
// the session-start action. No rule ("") and observe-only ("off") do nothing.
func pinDriftAction(mode string, drifted bool) (withhold, escalate, warn bool) {
	if !drifted {
		return false, false, false
	}
	switch mode {
	case spiceboxv1alpha1.PinModeBlock:
		return true, false, false
	case spiceboxv1alpha1.PinModeApprove:
		return false, true, true // escalated tools also carry the warning
	case spiceboxv1alpha1.PinModeWarn:
		return false, false, true
	default: // "" (no rule) or "off": observe only
		return false, false, false
	}
}
