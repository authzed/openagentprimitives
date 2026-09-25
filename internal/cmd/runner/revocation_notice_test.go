package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// capturedPublish records everything published on the session's subjects so a
// test can assert what a participant would actually have seen.
type capturedPublish struct{ envs []channelevents.Envelope }

func (c *capturedPublish) fn() channelevents.PublishFunc {
	return func(_ string, data []byte) error {
		var env channelevents.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return err
		}
		c.envs = append(c.envs, env)
		return nil
	}
}

// notices decodes the interaction_request envelopes (a notice IS an interaction
// with zero actions) so assertions read against the payload a surface renders.
func (c *capturedPublish) notices(t *testing.T) []channelevents.InteractionRequestPayload {
	t.Helper()
	var out []channelevents.InteractionRequestPayload
	for _, env := range c.envs {
		if env.Kind != channelevents.KindInteractionRequest {
			continue
		}
		var pl channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		out = append(out, pl)
	}
	return out
}

// failingSecretInvalidator is a credential holder that cannot be invalidated.
// It stands in for the class of failure the merged hook-skipping fix created a
// blind spot for: the invalidation is refused, so no Revoked record is written
// and nothing downstream reports it.
type failingSecretInvalidator struct{ err error }

func (f *failingSecretInvalidator) InvalidateSecret(_, _ string) error { return f.err }

// wireRevocationForTest builds the production registry + subscriber over a fake
// bus, with the production failure notifier pointed at a capturing publisher.
func wireRevocationForTest(t *testing.T, captured *capturedPublish, credInv *failingSecretInvalidator) *fakeBus {
	t.Helper()
	bus := &fakeBus{}
	notifier := newRevocationFailureNotifier(captured.fn(), nil,
		channelevents.SessionRef{Namespace: "default", Name: "demo-session"})
	reg, err := newRevocationRegistry(toolorigin.New(), notifier, credInv)
	require.NoError(t, err)
	require.NoError(t, subscribeRevocationOnBus(context.Background(), bus, reg, "default", nil))
	require.NotNil(t, bus.h, "bus.Subscribe must have been called")
	return bus
}

// A revocation the runner could not apply must reach the people in the session,
// not just the log. The subscriber deliberately runs no hook on failure (running
// it would record the credential as revoked while a live copy keeps
// authenticating), which leaves this the only path a user learns anything at
// all. AGENTS.md §Never silently drop errors, clause 3.
func TestRevocationInvalidateFailure_PublishesNoticeToParticipants(t *testing.T) {
	captured := &capturedPublish{}
	bus := wireRevocationForTest(t, captured, &failingSecretInvalidator{err: assertErr("broker refused")})

	bus.h(mkRevocationEnv(t, "credential", "id-ns/gh-pat", ""))

	notices := captured.notices(t)
	require.Len(t, notices, 1, "a failed revocation must surface exactly one notice to the session")
	n := notices[0]
	assert.Equal(t, revocationFailureCategory, n.Category)
	assert.Equal(t, channelevents.AudienceParticipants, n.Audience.Scope,
		"everyone in the conversation is affected, not just the revoker")
	assert.NotEmpty(t, n.NextStep, "the reader must be told what to do about it")
	assert.NotEmpty(t, n.Lead)

	// The revoke key is bus-supplied operator vocabulary and must travel in the
	// inert excerpt, never in the fields a surface renders as trusted markup.
	require.NotNil(t, n.Excerpt, "the reference must ride the inert excerpt")
	assert.Contains(t, n.Excerpt.Content, "id-ns/gh-pat")
	for _, trusted := range []string{n.Lead, n.Body, n.NextStep} {
		assert.NotContains(t, trusted, "id-ns/gh-pat",
			"the raw revoke key must not be interpolated into trusted copy")
	}

	// Nothing here may claim the revocation happened: only the notice goes out.
	for _, env := range captured.envs {
		assert.Equal(t, channelevents.KindInteractionRequest, env.Kind,
			"a failed revocation must publish nothing but the notice")
	}
}

// The reader is told WHAT was withdrawn, not handed the menu of everything the
// system can withdraw. A notice that lists both kinds makes the reader guess
// which one applies to them, on the one message where guessing is expensive.
//
// The noun comes off the Invalidator (revocation.Invalidator.Noun), so naming
// it costs no `if kind == "credential"` in this package — the switch AGENTS.md
// forbids outside the kind's own package.
func TestRevocationInvalidateFailure_NoticeNamesWhatWasWithdrawn(t *testing.T) {
	captured := &capturedPublish{}
	bus := wireRevocationForTest(t, captured, &failingSecretInvalidator{err: assertErr("broker refused")})

	bus.h(mkRevocationEnv(t, "credential", "id-ns/gh-pat", ""))

	notices := captured.notices(t)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Body, "a connected account",
		"the credential invalidator's own noun must reach the copy")
	assert.NotContains(t, notices[0].Body, "a set of tools",
		"a credential revocation must not offer the reader the tool-origin noun as well")
}

// Core NATS Subscribe has no ack, and a restart replays: the same failed
// revocation can arrive many times. The reader gets one message, the operator
// still gets a log line per delivery.
func TestRevocationInvalidateFailure_RepeatedDeliveryPublishesOnce(t *testing.T) {
	captured := &capturedPublish{}
	bus := wireRevocationForTest(t, captured, &failingSecretInvalidator{err: assertErr("broker refused")})

	env := mkRevocationEnv(t, "credential", "id-ns/gh-pat", "")
	bus.h(env)
	bus.h(env)
	bus.h(env)
	assert.Len(t, captured.notices(t), 1, "a replayed delivery of the same failure must not re-notify")

	// A DIFFERENT credential is a different fact and is reported on its own.
	bus.h(mkRevocationEnv(t, "credential", "id-ns/other-pat", ""))
	assert.Len(t, captured.notices(t), 2, "a distinct failed revocation must still be surfaced")
}

// The notice is a failure report, so a revocation that lands must be silent —
// the success path already has its own durable record and its own audit event.
func TestRevocationInvalidateSuccess_PublishesNothing(t *testing.T) {
	captured := &capturedPublish{}
	bus := wireRevocationForTest(t, captured, &failingSecretInvalidator{err: nil})

	bus.h(mkRevocationEnv(t, "tool-origin", "mcpserver/code-tools", "default"))
	bus.h(mkRevocationEnv(t, "credential", "id-ns/gh-pat", ""))

	assert.Empty(t, captured.envs, "a successful invalidation must post nothing")
}

// A publish that failed told nobody, so the dedup set must not remember it as
// told: the next delivery of the same revoke gets another chance at the reader.
func TestRevocationInvalidateFailure_FailedPublishIsRetriedOnRedelivery(t *testing.T) {
	var attempts int
	var accepted []channelevents.Envelope
	flaky := func(_ string, data []byte) error {
		attempts++
		if attempts == 1 {
			return assertErr("nats down")
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(data, &env))
		accepted = append(accepted, env)
		return nil
	}
	notifier := newRevocationFailureNotifier(flaky, nil,
		channelevents.SessionRef{Namespace: "default", Name: "demo-session"})
	reg, err := newRevocationRegistry(toolorigin.New(), notifier,
		&failingSecretInvalidator{err: assertErr("broker refused")})
	require.NoError(t, err)
	bus := &fakeBus{}
	require.NoError(t, subscribeRevocationOnBus(context.Background(), bus, reg, "default", nil))

	env := mkRevocationEnv(t, "credential", "id-ns/gh-pat", "")
	bus.h(env)
	require.Empty(t, accepted, "the first publish failed, so nothing reached the session")
	bus.h(env)
	assert.Len(t, accepted, 1, "the redelivery must re-attempt the notice")
	bus.h(env)
	assert.Len(t, accepted, 1, "once delivered, further redeliveries stay quiet")
}

// The notice's category must be a registered NOTICE row, or notice.Payload
// refuses to build it and the user sees nothing — the exact failure this whole
// path exists to prevent.
//
// The tone is asserted here rather than only at the registry because this is
// where the borrowing would happen again: pointing this constant at a
// convenient existing row is a one-character change, and the row that fits an
// unapplied withdrawal is the one about who can reach whose data — not the one
// that promises a retry will fix it.
func TestRevocationFailureCategory_IsARegisteredNoticeRow(t *testing.T) {
	cat, ok := channelinteractions.Get(revocationFailureCategory)
	require.True(t, ok, "category %q must be registered", revocationFailureCategory)
	assert.True(t, cat.Notice, "a revocation failure report has no decision leg")
	assert.False(t, cat.Terminal, "the session continues; only the withdrawal did not land")
	assert.Equal(t, channelinteractions.TonePrivacy, cat.Tone,
		"an access withdrawal that did not land is about who can reach whose data; "+
			"degraded would promise that retrying is the remedy, and it is not")
}

// The copy must be readable by whoever was talking to the agent: no cluster
// vocabulary in the fields surfaces render as trusted markup.
func TestRevocationFailureNotice_CopyCarriesNoOperatorVocabulary(t *testing.T) {
	args := revocationFailureNotice("a connected account", "credential", "id-ns/gh-pat").Args()
	for _, banned := range []string{"kubectl", "spicedb", "crd", "reconcile", "configmap", "secret", "namespace"} {
		for _, trusted := range []string{args.Lead, args.Body, args.NextStep} {
			assert.NotContains(t, strings.ToLower(trusted), banned,
				"operator vocabulary %q must not reach a chat surface", banned)
		}
	}
}

// revocation.Invalidator documents an empty Noun as legal, so the substitution
// site has to survive one. The failure this guards is not a missing word — it
// is "Someone withdrew  this conversation is using", a visibly broken sentence
// posted into a chat thread at the moment the reader most needs to trust it.
//
// Table-driven over the shipped nouns plus the degenerate ones, because the
// property under test ("the sentence reads") is the same for all of them and
// the next kind added should be one row.
func TestRevocationFailureNotice_BodyIsGrammaticalForEveryNoun(t *testing.T) {
	cases := []struct {
		name string
		noun string
		want string
	}{
		{
			name: "credential's noun: names the account",
			noun: "a connected account",
			want: "Someone withdrew a connected account this conversation is using,",
		},
		{
			name: "tool-origin's noun: names the toolset",
			noun: "a set of tools",
			want: "Someone withdrew a set of tools this conversation is using,",
		},
		{
			name: "no noun: falls back to vaguer copy, still a sentence",
			noun: "",
			want: "Someone withdrew access this conversation is using,",
		},
		{
			name: "blank noun: treated as absent, not substituted verbatim",
			noun: "   ",
			want: "Someone withdrew access this conversation is using,",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := revocationFailureNotice(tc.noun, "credential", "id-ns/gh-pat").Args().Body
			assert.Contains(t, body, tc.want)
			assert.NotContains(t, body, "  ", "a missing noun must not leave a double space behind")
		})
	}
}

// assertErr is a tiny error value so the tests read as scenarios rather than as
// errors.New ceremony.
type assertErr string

func (e assertErr) Error() string { return string(e) }
