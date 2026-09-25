package triggerdelivery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
)

func newMem(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	return memory.WithSystemApproval(context.Background(), "test"),
		memory.NewLocal(inmem.NewBackend()),
		memory.Scope{Kind: "session", ID: "default/demo-session"}
}

// TestRecordGet_RoundTrip is the happy path: what channelsd wrote is what a
// capture reads back, byte-for-byte. Byte-for-byte matters because the capture
// re-signs this body with the fixture's placeholder secret and posts it at the
// production webhook route — a body that differs by a byte fails HMAC.
func TestRecordGet_RoundTrip(t *testing.T) {
	ctx, m, scope := newMem(t)
	body := json.RawMessage(`{"action":"opened","number":7}`)

	require.NoError(t, triggerdelivery.Record(ctx, m, scope, triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "demoforge:acme/widgets#7", Body: body,
	}))

	got, err := triggerdelivery.Get(ctx, m, scope)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "demoforge", got.Kind)
	assert.Equal(t, "pull_request", got.Event)
	assert.Equal(t, "demoforge:acme/widgets#7", got.ChannelKey)
	assert.JSONEq(t, string(body), string(got.Body))
	assert.False(t, got.Truncated)
}

// TestGet_NoRecordIsNotAnError pins that a session with no trigger reads as
// (nil, nil). Every conversational session is in this state, so an error here
// would make the capture's normal path an error path.
func TestGet_NoRecordIsNotAnError(t *testing.T) {
	ctx, m, scope := newMem(t)
	got, err := triggerdelivery.Get(ctx, m, scope)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestRecord_TruncatesOversizeBodyAndSaysSo pins that an oversize body is CUT
// and MARKED rather than either stored whole or dropped.
//
// The marker is the point. A truncated body cannot be re-signed into a valid
// delivery, so a capture must refuse it — and refusing requires knowing. Storing
// a silent prefix would produce a trigger bundle that fails HMAC verification
// at replay with no explanation.
func TestRecord_TruncatesOversizeBodyAndSaysSo(t *testing.T) {
	ctx, m, scope := newMem(t)
	huge := json.RawMessage(bytes.Repeat([]byte("x"), triggerdelivery.MaxBodyBytes+1))

	require.NoError(t, triggerdelivery.Record(ctx, m, scope, triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "k", Body: huge,
	}))

	got, err := triggerdelivery.Get(ctx, m, scope)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Truncated, "an oversize body must be MARKED, or a capture cannot know to refuse it")
	assert.LessOrEqual(t, len(got.Body), triggerdelivery.MaxBodyBytes)
}

// TestRecord_EmptyBodyIsRefused pins that "there was a trigger but we kept
// nothing" is never written. A row with an empty body reads to a capture as a
// recordable delivery and produces a bundle whose payload is `null`.
func TestRecord_EmptyBodyIsRefused(t *testing.T) {
	ctx, m, scope := newMem(t)
	err := triggerdelivery.Record(ctx, m, scope, triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "k",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestRecord_ReRecordingTheSameDeliveryIsIdempotent pins that a provider
// redelivering the identical webhook (its own retry, or a restarted channelsd
// re-processing the same NATS message) does not fail the second Record call.
// The entry id is the FIXED string "trigdel-opening" — not content-derived —
// so Record must prove the bodies match before treating a conflict as a
// no-op; this pins the "they matched" half of that contract.
func TestRecord_ReRecordingTheSameDeliveryIsIdempotent(t *testing.T) {
	ctx, m, scope := newMem(t)
	c := triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "k",
		Body: json.RawMessage(`{"action":"opened"}`),
	}

	require.NoError(t, triggerdelivery.Record(ctx, m, scope, c))
	require.NoError(t, triggerdelivery.Record(ctx, m, scope, c), "byte-identical re-record must be a no-op")

	got, err := triggerdelivery.Get(ctx, m, scope)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.JSONEq(t, string(c.Body), string(got.Body))
}

// TestRecord_DifferingReRecordErrors pins the other half of the idempotency
// contract: the entry id is a fixed string, so a conflict on it does NOT by
// itself prove the delivery matches (unlike systemprompt, whose id IS its
// digest). A second delivery claiming to open the same session with a
// DIFFERENT body must surface as an error naming the session, not vanish into
// the "already recorded" no-op path — this is an append-only audit trail and
// silently dropping a genuinely different delivery would falsify it.
func TestRecord_DifferingReRecordErrors(t *testing.T) {
	ctx, m, scope := newMem(t)

	require.NoError(t, triggerdelivery.Record(ctx, m, scope, triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "k",
		Body: json.RawMessage(`{"action":"opened"}`),
	}))

	err := triggerdelivery.Record(ctx, m, scope, triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "k",
		Body: json.RawMessage(`{"action":"reopened"}`),
	})
	require.Error(t, err, "a different delivery claiming to open the same session must not be swallowed as idempotent")
	assert.Contains(t, err.Error(), scope.ID, "the error must name the session")

	got, getErr := triggerdelivery.Get(ctx, m, scope)
	require.NoError(t, getErr)
	require.NotNil(t, got)
	assert.JSONEq(t, `{"action":"opened"}`, string(got.Body), "the original record must be untouched — the rejected write must not have landed")
}

// failingQueryMemory wraps a real Memory and fails every Query while leaving
// Put alone. It is the only way to reach Record's read-back failure: the
// conflict has to be genuine (so Put must work) and the comparison has to be
// impossible (so Query must not).
type failingQueryMemory struct {
	memory.Memory
	err error
}

func (f failingQueryMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, f.err
}

// TestRecord_UnverifiableConflictDoesNotClaimTwoDeliveries pins that "could not
// verify" is reported as itself, never as "genuinely different".
//
// The delivery re-recorded here is BYTE-IDENTICAL, so the truthful answer is
// "already recorded, no-op" — a conflict still fires because the entry carries
// a fresh CreatedAt, which is exactly why Record reads back instead of assuming.
// With the read-back error discarded, the caller was told two different
// deliveries claimed to open one session, which is false; channelsd logs that
// sentence verbatim, so it is what an operator greps for and then hunts a
// second delivery that never existed.
func TestRecord_UnverifiableConflictDoesNotClaimTwoDeliveries(t *testing.T) {
	ctx, m, scope := newMem(t)
	c := triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "k",
		Body: json.RawMessage(`{"action":"opened"}`),
	}
	require.NoError(t, triggerdelivery.Record(ctx, m, scope, c))

	broken := failingQueryMemory{Memory: m, err: errors.New("backend unreachable")}
	err := triggerdelivery.Record(ctx, broken, scope, c)

	require.Error(t, err, "an unverifiable conflict must not be swallowed as an idempotent no-op either")
	assert.Contains(t, err.Error(), "backend unreachable",
		"the read-back error is what makes the diagnosis honest; discarding it is the bug")
	assert.Contains(t, err.Error(), "UNKNOWN", "the error must say the comparison did not happen")
	assert.Contains(t, err.Error(), scope.ID, "the error must name the session")
	assert.NotContains(t, err.Error(), "two different deliveries",
		"the bodies are identical; claiming they differ is a confidently wrong diagnosis")
}
