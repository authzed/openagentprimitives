// pkg/channels/channelkinds/slack/testhooks.go
//
// Test-only injection seam so the e2e harness (package e2e, a different
// package) can drive the real Slack kind in-process: ONE fakeslack.Client
// backs BOTH the listener (newSlackAPIClientFull) and the sender
// (newSlackAPIClient), so inbound and outbound traffic share a single
// conversation model, plus a fake socketSource for the listener's event loop.
//
// InstallTestTransport takes VALUES of the unexported listenerAPIClient /
// socketSource interface types, not a factory function naming them: package
// e2e cannot write a function literal whose signature names an unexported
// type from this package, but it CAN pass a *fakeslack.Client / a fake source
// value that structurally satisfies the interface. The closure that adapts
// the source value into newSocketSource's `func(listenerAPIClient) socketSource`
// shape is written here, in package slack, where naming those types is fine.
package slack

// testClientOverride, when non-nil, is returned by newSlackAPIClientFull
// (listener_client.go) and the sender's newSlackAPIClient (sender.go) so a
// single fake instance backs both call sites. Nil in production and in every
// test that doesn't call InstallTestTransport, in which case both factories
// fall through to their normal token-based construction — production
// behavior is unchanged.
var testClientOverride listenerAPIClient

// InstallTestTransport injects a fake Slack client (backing BOTH the listener
// and sender client factories) and a fake socket source (backing
// newSocketSource). Returns a reset func for t.Cleanup that restores whatever
// was installed before (nil the first time, so tests can nest/compose).
// Test use only — never called from production code paths.
func InstallTestTransport(client listenerAPIClient, source socketSource) (reset func()) {
	prevClient, prevSource := testClientOverride, newSocketSource
	testClientOverride = client
	newSocketSource = func(listenerAPIClient) socketSource { return source }
	return func() {
		testClientOverride, newSocketSource = prevClient, prevSource
	}
}
