package channelkinds_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestEveryRegisteredKindAnswersWebhookReceiver pins the contract that every
// registered kind answers Kind.WebhookReceiver without panicking. nil is a
// valid answer meaning "not webhook-routable" — a kind that panicked here
// would take webd's route table down at startup, since it sweeps the whole
// registry to mount receivers (see kind.go's WebhookReceiver doc comment).
//
// require.NotEmpty on the registry, plus one t.Run per kind, is what keeps
// this from passing vacuously: an empty registry (a missing blank import)
// would otherwise report a green parent test having swept nothing.
func TestEveryRegisteredKindAnswersWebhookReceiver(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by the blank imports above")

	for _, k := range kinds {
		t.Run(k.Name(), func(t *testing.T) {
			assert.NotPanics(t, func() { _ = k.WebhookReceiver(channelkinds.Deps{}) })
		})
	}
}

// TestNoRegisteredKindHasAWebhookSurfaceYet pinned, as of the WebhookReceiver
// seam landing, that every registered kind's answer was nil — none had an
// inbound HTTP surface yet. github is now the first (and, as of this test,
// only) exception: it receives GitHub pull-request deliveries by webhook, so
// its WebhookReceiver is deliberately non-nil (see
// pkg/channels/channelkinds/github's kind.go/receiver.go). Every OTHER
// registered kind is still expected to answer nil here; a future kind
// legitimately joining github is the signal to add it to the exceptions set
// below, not to delete this test.
func TestNoRegisteredKindHasAWebhookSurfaceYet(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by the blank imports above")

	webhookRoutable := map[string]bool{"github": true}

	for _, k := range kinds {
		t.Run(k.Name(), func(t *testing.T) {
			if webhookRoutable[k.Name()] {
				assert.NotNil(t, k.WebhookReceiver(channelkinds.Deps{}),
					"%q is a declared webhook-routable exception; it must answer non-nil", k.Name())
				return
			}
			assert.Nil(t, k.WebhookReceiver(channelkinds.Deps{}),
				"no registered kind other than the declared exceptions implements WebhookReceiver")
		})
	}
}

// TestWebhookReceiverNeverReturnsATypedNilPointer guards the classic Go
// nil-interface gotcha (AGENTS.md's "Nil interfaces: never assign a
// typed-nil pointer directly" rule) one level down from a struct-field
// assignment. channelsd's session-open path
// (pkg/channels/channelsd/pipeline.go's Deliver) type-asserts
// k.WebhookReceiver(Deps{}) to TriggerFactProvider and, on ok==true, calls a
// method on it UNCONDITIONALLY. A kind that returned a typed-nil pointer
// (`var r *someReceiver; return r`, with *someReceiver implementing
// WebhookReceiver) would make plain `!= nil` true — a typed-nil wrapped in
// an interface is never equal to a bare nil — so the assertion would succeed
// and the call would panic on a nil receiver the instant it touched a field.
//
// assert.Nil (used by the two tests above) will NOT catch this: testify's
// Nil check uses reflection and correctly reports a typed-nil pointer as
// nil, which is exactly the gap between what the test framework sees and
// what a plain Go `!= nil` in production code sees. This test checks the
// production-relevant property directly instead.
//
// Every registered kind today returns either a bare nil or a zero-size value
// type (github's receiver{}), which is why this has never fired — this test
// is what stops a FUTURE kind from reintroducing the hazard.
func TestWebhookReceiverNeverReturnsATypedNilPointer(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by the blank imports above")

	for _, k := range kinds {
		t.Run(k.Name(), func(t *testing.T) {
			rcv := k.WebhookReceiver(channelkinds.Deps{})
			if rcv == nil {
				return // the documented "not webhook-routable" answer
			}
			v := reflect.ValueOf(rcv)
			if v.Kind() == reflect.Ptr {
				assert.False(t, v.IsNil(),
					"%q's WebhookReceiver returned a typed-nil pointer wrapped in a non-nil interface — "+
						"a TriggerFactProvider type-assertion on it would succeed and a subsequent call "+
						"would panic on a nil receiver; return an untyped nil instead", k.Name())
			}
		})
	}
}
