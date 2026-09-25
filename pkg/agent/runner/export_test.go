// Exports unexported symbols for use by external (_test) packages.
package runner

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// WrapUntrustedToolOutputWithNonce is the exported test alias for the pure
// nonce-wrapping core. External test packages use this to drive unit tests
// without importing internal helpers.
var WrapUntrustedToolOutputWithNonce = wrapUntrustedToolOutputWithNonce

// WrapUntrustedToolOutput is the exported test alias for the nonce-generating
// wrapper. External test packages use this to verify per-call nonce uniqueness.
var WrapUntrustedToolOutput = wrapUntrustedToolOutput

// HeldInbox is the exported test alias for (*Loop).heldInbox, letting the
// external runner_test package assert what the mid-loop drain gate held vs.
// placed (heldInbox itself stays unexported — it's an internal read used by
// drainInbox and the yield-boundary sites, not part of Loop's public API).
func (l *Loop) HeldInbox(ctx context.Context) ([]memory.Turn, error) {
	return l.heldInbox(ctx)
}

// FireInterrupt is the exported test alias for l.interrupts.fire(), letting
// the external runner_test package simulate a user-initiated Interrupt firing
// mid-dispatch (interrupts itself stays an unexported field — it's internal
// dispatch-fan-out state, not part of Loop's public API).
func (l *Loop) FireInterrupt() {
	l.interrupts.fire()
}

// MaxNativeBytesPerRequest is the exported test alias for the hydration
// pass's per-request native-payload budget, so tests can size a fixture
// against the real ceiling instead of restating the number.
const MaxNativeBytesPerRequest = maxNativeBytesPerRequest

// HydrateAttachmentsForTest is the exported test alias for
// (*Loop).hydrateAttachments, letting the external runner_test package
// assert the post-condition (no attachment ref survives hydration) without
// driving a full turn (hydrateAttachments itself stays unexported — it's an
// internal pre-send pass, not part of Loop's public API). Like the pass
// itself it returns the request's own view of the conversation and leaves
// msgs untouched, so tests must assert against the RETURNED slice.
func HydrateAttachmentsForTest(l *Loop, ctx context.Context, msgs []llm.Message) []llm.Message {
	return l.hydrateAttachments(ctx, msgs)
}

// EmptyContentPlaceholderTextForTest exposes the placeholder hydration and
// contentBlocksFromMemory both fall back to when stripping blocks would
// leave a message with no content at all. Tests assert on it exactly, so
// that "the placeholder fired" stays distinguishable from "some other note
// filled the message" — the two are both non-empty text blocks.
const EmptyContentPlaceholderTextForTest = emptyContentPlaceholderText

// SuppressNativeBlocksForTest flips the session-scoped native-block
// suppression the provider-error path sets, so hydration behavior under
// suppression can be tested without driving a real provider rejection.
func SuppressNativeBlocksForTest(l *Loop) { l.nativeSuppressed.Store(true) }

// PinAttachmentForTest pins a handle without the memory-validation round
// trip PinAttachment does, so hydration behavior can be tested without a
// wired store.
func PinAttachmentForTest(l *Loop, handle string) {
	l.pinnedMu.Lock()
	defer l.pinnedMu.Unlock()
	if l.pinnedAttachments == nil {
		l.pinnedAttachments = map[string]bool{}
	}
	l.pinnedAttachments[handle] = true
}
