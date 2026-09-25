package main

import "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

// clientHostedHere reports whether the named channel kind is registered in this
// channelsd process AND is client/host-side (RelayedByChannelsd()==false) — its
// inbound/outbound is surfaced by another process (webd for "browser" and
// "github", the CLI for "local"), not by channelsd. channelsd's proactive
// iterators (the channelManager over Channels, the sessionWatcher over
// AgentSessions) skip such objects: channelsd has no transport for them, and
// trying to resolve one only produces spurious `unknown kind`/nil-sender
// errors on every poll tick.
//
// It is deliberately false for an UNREGISTERED kind (a typo, or a kind this
// binary doesn't import) so a genuinely-unknown kind still surfaces as an error
// rather than being silently ignored, and false for an empty kind (test
// sessions carry none) so those are processed normally.
func clientHostedHere(kind string) bool {
	k, ok := registry.Get(kind)
	return ok && !k.RelayedByChannelsd()
}
