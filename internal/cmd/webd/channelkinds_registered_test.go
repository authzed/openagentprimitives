package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry/crdenumtest"
)

// webdUnservedKinds are the channel kinds webd deliberately does NOT link.
//
// This is an EXEMPTION list, not a check list, and the difference is the whole
// point. The operator's version of this guard once carried a list of kinds to
// CHECK; it silently omitted "github" and kept passing while every kind=github
// Channel was permanently Valid=False. A list of kinds to check under-checks
// when it goes stale. A list of kinds to EXEMPT fails closed: every kind the
// CRD admits is required here unless it is named below with a reason, so a new
// kind added to the enum breaks this test until someone decides which it is.
//
// Each entry costs webd nothing and is justified by webd never resolving that
// kind: the browser-facing binary serves the webhook route (a registry sweep
// over Kind.WebhookReceiver — see pkg/web/webui/channelwebhook) and hosts
// browser sessions. It never relays for a cron input.
var webdUnservedKinds = map[string]string{
	"bento": "cron-driven inputs are relayed by channelsd; webd resolves no bento Channel, " +
		"and linking the kind would pull ~170 packages (the whole Bento/CUE tree) into the browser-facing binary",
	"local": "the TUI kind is hosted by the oap CLI in-process; webd never resolves it",
}

// TestChannelKindsRegistered guards that webd blank-imports every channel kind
// it can be asked to serve — the same derived guard internal/cmd/operator and
// cmd/oap carry, on the binary the inbound webhook path actually runs through.
//
// Without it, deleting webd's github blank import is invisible to every suite:
// channelwebhook's registry.Get misses, the route answers 404 "unknown kind"
// to every delivery, GitHub retries and then disables the hook, and nothing on
// the Channel says why. Nothing else in the tree fails.
//
// The requirement is spelled as enum-minus-exemptions rather than the
// operator's plain full enum because webd's job is narrower than the
// operator's: the operator VALIDATES a Channel of any kind, so it needs them
// all; webd only serves the ones it hosts a surface for. Both halves of that
// are asserted — see webdUnservedKinds and the staleness check below.
func TestChannelKindsRegistered(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t)
	require.NotEmpty(t, enum, "spec.kind enum not found in the embedded install bundle")

	var required int
	for _, kind := range enum {
		if _, exempt := webdUnservedKinds[kind]; exempt {
			continue
		}
		required++
		if _, ok := registry.Get(kind); !ok {
			t.Errorf("channel kind %q is not registered in the webd binary — add a blank import to internal/cmd/webd/main.go, "+
				"or, if webd genuinely never serves it, add it to webdUnservedKinds with the reason", kind)
		}
	}
	require.NotZero(t, required, "every enum kind is exempt; this guard would assert nothing")
}

// TestWebdUnservedKindsAreRealKinds keeps the exemption list from rotting in
// the other direction: an entry naming a kind the CRD no longer admits is dead
// weight that would silently excuse a future kind reusing that name.
func TestWebdUnservedKindsAreRealKinds(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t)
	require.NotEmpty(t, enum)

	inEnum := make(map[string]bool, len(enum))
	for _, k := range enum {
		inEnum[k] = true
	}
	for kind, why := range webdUnservedKinds {
		assert.Truef(t, inEnum[kind],
			"webdUnservedKinds names %q (%q) but the Channel CRD no longer admits that kind; remove the exemption", kind, why)
		assert.NotEmptyf(t, why, "every exemption must record WHY webd does not serve %q", kind)
	}
}

// TestWebhookRoutableKindsAreRegistered is the sharper half: whatever else webd
// does or does not link, a kind that receives deliveries over HTTP MUST be
// registered here, because webd is the only process that serves that route.
//
// It asks the live registry rather than the enum, so it cannot be satisfied by
// an exemption: any registered kind answering WebhookReceiver != nil proves
// itself routable, and the assertion is that the route can resolve it by the
// same name a delivery URL carries. A kind dropped from main.go disappears
// from the registry and is caught by the enum guard above instead.
func TestWebhookRoutableKindsAreRegistered(t *testing.T) {
	all := registry.All()
	require.NotEmpty(t, all, "no channel kinds registered in the webd binary at all")

	var routable []string
	for _, k := range all {
		if k.WebhookReceiver(channelkinds.Deps{}) != nil {
			routable = append(routable, k.Name())
		}
	}
	require.NotEmpty(t, routable,
		"webd mounts /webhooks/<kind>/... by sweeping the registry for a WebhookReceiver; "+
			"if no registered kind has one, every inbound delivery 404s")

	for _, name := range routable {
		got, ok := registry.Get(name)
		require.Truef(t, ok, "webhook-routable kind %q is not resolvable by name — the route Gets it by the name in the URL", name)
		assert.Equal(t, name, got.Name())
	}
}
