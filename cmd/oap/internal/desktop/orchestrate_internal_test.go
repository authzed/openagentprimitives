package desktop

import "testing"

// RegisterTestChannelKind registers a test-only entry into
// knownExternalChannelKinds for the duration of the calling test, so
// Config.Validate() will accept a Channel with that Kind. It exists so
// orchestrate tests can exercise the Channel != nil path (e.g. that
// InitLocal only runs when a channel is configured) without adding a
// placeholder for a not-yet-implemented backend (e.g. "whatsapp") to the
// production knownExternalChannelKinds map — that map stays the
// authoritative, fail-closed set of channel kinds with a real
// channelkinds backend. Exported (from a _test.go file, so it never
// ships in the production binary) for use by the external desktop_test
// package.
func RegisterTestChannelKind(t *testing.T, kind string) {
	t.Helper()
	knownExternalChannelKinds[kind] = struct{}{}
	t.Cleanup(func() { delete(knownExternalChannelKinds, kind) })
}
