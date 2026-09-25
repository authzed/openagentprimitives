package main

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// TestChannelKindsRegistered guards that the runner blank-imports every channel
// kind it may resolve via resolve.ForSession — or, for github, off the INPUT
// binding's kind name. Those Kinds self-register in init(); a missing blank
// import makes registry.Get miss and silently disables all channel-sourced meta
// tools (mention_lookup / channel_history / the info-leakage gate) with only a
// runtime warning. This turns that regression into a build-time test failure
// instead.
//
// github is on this list even though the runner never SENDS on it. Its sessions
// are bound to it as INPUT, and the trigger-status capability resolves that
// binding's kind to decide whether to offer claim/conclude_trigger_status —
// so without the blank import a review agent silently loses its only way to
// answer the pull request that started it, which is the exact failure the seam
// exists to end.
func TestChannelKindsRegistered(t *testing.T) {
	for _, kind := range []string{browser.KindName, "slack", "local", "bento", "fake", "github", "agent"} {
		if _, ok := registry.Get(kind); !ok {
			t.Errorf("channel kind %q is not registered in the runner binary — add a blank import to internal/cmd/runner/main.go", kind)
		}
	}
}

// TestTriggerStatusReporterReachableFromTheRunner is the stronger claim behind
// the row above, stated as the behavior rather than the wiring: at least one
// kind the runner links must actually report trigger status, or the capability
// is dead code in this binary no matter what its own tests say.
func TestTriggerStatusReporterReachableFromTheRunner(t *testing.T) {
	var any bool
	for _, k := range registry.All() {
		if _, ok := registry.TriggerStatusReporterFor(k.Name()); ok {
			any = true
		}
	}
	if !any {
		t.Error("no channel kind linked into the runner reports trigger status — the trigger-status capability can never offer a tool here")
	}
}
