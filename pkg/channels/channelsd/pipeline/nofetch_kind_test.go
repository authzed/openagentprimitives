package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// noFetchKind is a registered kind that deliberately does NOT implement
// channelkinds.AttachmentFetcher, so the outcomeUnfetchable branch — a
// transport that structurally cannot download a file — has something to be
// tested against.
//
// Its non-implementation is the entire reason it exists, which is the property
// the test needs: a kind that merely happens to lack the interface stops
// carrying the test the moment it gains one, and does so silently — the gate
// opens and the case becomes a duplicate of the gate-open tests.
//
// fake.Kind is embedded for the ~25 methods of channelkinds.Kind — hand-
// delegating them would break every time that interface grows — and the two
// attachment methods it now promotes are SHADOWED below.
type noFetchKind struct{ fakekind.Kind }

func (k *noFetchKind) Name() string { return "no-fetch" }

// notAFetcher is an unexported parameter type no interface can name, which is
// what makes the shadowing below impossible to satisfy by accident.
type notAFetcher struct{}

// AttachmentBounds and FetchAttachment shadow the methods embedding promotes
// from fake.Kind. Their signatures deliberately do not match
// channelkinds.AttachmentFetcher, so *noFetchKind does not satisfy it and the
// pipeline takes the outcomeUnfetchable path.
//
// Shadowing rather than not-embedding: Go has no way to remove a promoted
// method, and the alternative — delegating all ~25 Kind methods by hand — makes
// this file break whenever Kind grows, for no benefit to the property under
// test. The compile-time assertion below is what keeps the trick honest.
func (k *noFetchKind) AttachmentBounds(notAFetcher) {}
func (k *noFetchKind) FetchAttachment(notAFetcher)  {}

func init() { chregistry.Register(&noFetchKind{}) }

// TestNoFetchKindIsNotAnAttachmentFetcher guards the property this whole file
// exists to provide, rather than trusting the shadowing to keep working.
//
// Without it, a change that made *noFetchKind a fetcher again would turn
// TestProcessAttachments_GateClosed_KindNotAttachmentFetcher_Unfetchable into a
// second, silent copy of the gate-open tests — which is exactly how the
// second, silent copy of the gate-open tests.
func TestNoFetchKindIsNotAnAttachmentFetcher(t *testing.T) {
	var k any = &noFetchKind{}
	_, isFetcher := k.(channelkinds.AttachmentFetcher)
	assert.False(t, isFetcher,
		"noFetchKind must NOT implement AttachmentFetcher; the unfetchable branch has nothing else to be tested against")

	// And the positive control: the kind it embeds IS one, so the shadowing is
	// what makes the difference rather than the interface having moved.
	var inner any = &fakekind.Kind{}
	_, innerIsFetcher := inner.(channelkinds.AttachmentFetcher)
	assert.True(t, innerIsFetcher, "fake.Kind is a fetcher; if it stops being one this file's premise changed")
}
