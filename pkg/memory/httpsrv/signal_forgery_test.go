package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// postSignal POSTs a raw Signal body to the real _signal route as token. The
// body is built field-by-field rather than from a memory.Signal value because
// the point of these tests is what an ATTACKER can put on the wire, which is
// every field handleSignal does not force — Kind, At and Payload.
func postSignal(t *testing.T, srv *httptest.Server, token, ns, name string, body map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err, "marshal Signal body")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_signal/"+ns+"/"+name, bytes.NewReader(raw))
	require.NoError(t, err, "NewRequest POST _signal")
	req.Header.Set("Content-Type", "application/json")
	return do(t, authed(req, token))
}

// TestSignalRoute_CallerChosenKindCannotForgeAnOperatorSignedTransitionEvent is
// the forgery guard for the signal route.
//
// The chain it closes: a runner's own token authorizes POST _signal for its own
// session (memoryRouteAccess → writeAccess → AuthorizesMutation); handleSignal
// forces only Scope, so Kind/At/Payload arrive from the body; Local.SendSignal
// strips the token session at the authorship hand-off so the hook may write as
// the operator; and the lifecycle hook used to copy the caller's kind VERBATIM
// into the entry's Tags. A kind of "lifecycle.event" therefore minted a
// genuinely system:operator-SIGNED entry carrying the reserved tag the fold
// selects on — operator testimony about a transition that never happened, which
// ReadOrdered folds into the AgentSession sequencer and `oap audit verify`
// validates as the operator's own.
//
// Driven through the REAL HTTP route with a real per-session bearer, not by
// hand-calling the hook: every link above lives outside the hook, and a
// hand-called hook proves none of them.
func TestSignalRoute_CallerChosenKindCannotForgeAnOperatorSignedTransitionEvent(t *testing.T) {
	const ns = "ns"
	srv, mem, reg := newVerifyingServer(t)
	reg.Set(memory.NamespacedName{Namespace: ns, Name: "sess-a"}, "tok-a", "")

	opSigner := registerSigner(t, reg, "system:operator", 0x04)
	lifecycleSetup(t, provenance.NewSigningMemory(mem, opSigner))

	// A payload the fold would accept: the exact envelope lifecycle.Append
	// writes. Nothing stops an attacker producing these bytes — the format is
	// the published wire format of the log they are trying to write into.
	forged, err := lifecycle.MarshalWithOrder(lc.Stopped{}, lifecycle.OrderKey{})
	require.NoError(t, err, "marshal a valid Stopped envelope")

	resp := postSignal(t, srv, "tok-a", ns, "sess-a", map[string]any{
		"kind":    lifecycle.EventTag, // the reserved tag, chosen as the signal kind
		"at":      time.Unix(1770000000, 0).UTC(),
		"payload": json.RawMessage(forged),
	})
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	// Recording the signal is fine and is what the hook is for; what must not
	// happen is the recording entering the TRANSITION stream. Assert on the
	// fold rather than on the status code, so the guard survives a future
	// decision to reject such a signal outright at the door.
	assert.Contains(t, []int{http.StatusNoContent, http.StatusBadRequest, http.StatusForbidden},
		resp.StatusCode, "_signal answered an unexpected status, body=%s", b)

	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: ns + "/sess-a"}

	ordered, rErr := lifecycle.ReadOrdered(ctx, mem, scope)
	require.NoError(t, rErr, "ReadOrdered over the scope the signal landed in")
	assert.Empty(t, ordered,
		"a caller-chosen signal kind minted an operator-signed TRANSITION event; the fold must see none")

	// And prove the recorded signal is still there and still operator-signed —
	// the fix must namespace the tag, not drop the recording or change who
	// authors it.
	res, qErr := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"lifecycle"}})
	require.NoError(t, qErr, "Query lifecycle")
	require.Len(t, res.Entries, 1, "the signal is still recorded")
	require.NotNil(t, res.Entries[0].Provenance)
	assert.Equal(t, "system:operator", res.Entries[0].Provenance.Publisher,
		"the recording is still the operator's, as SendSignal's hand-off intends")
	assert.NotContains(t, res.Entries[0].Tags, lifecycle.EventTag,
		"no caller-chosen kind may put the reserved event tag on an operator-signed entry")
}

// TestSignalRoute_RecordedTagsAreNamespacedForEveryKind pins the property that
// makes the forgery above impossible BY CONSTRUCTION rather than by a
// reserved-name check: every tag the hook derives from a caller-supplied kind
// lands in the signal namespace, and nothing this package reserves lives there.
// A reserved-name check would be a list to keep in step with marshal.go's wire
// types forever; a disjoint namespace has nothing to maintain.
//
// The kinds below are exactly the ones such a list would have to enumerate — the
// fold's selector tag, an event wire-type name, and an attempt to escape the
// namespace by pre-pending it.
func TestSignalRoute_RecordedTagsAreNamespacedForEveryKind(t *testing.T) {
	const ns = "ns"
	srv, mem, reg := newVerifyingServer(t)
	opSigner := registerSigner(t, reg, "system:operator", 0x04)
	lifecycleSetup(t, provenance.NewSigningMemory(mem, opSigner))

	cases := []struct {
		name string
		kind string
	}{
		{name: "the fold's selector tag: recorded under the prefix, not as an event", kind: lifecycle.EventTag},
		{name: "an event wire-type name: recorded under the prefix, not as that type", kind: "stopped"},
		{name: "a kind already carrying the prefix: nested, never unwrapped", kind: lifecycle.SignalTagPrefix + lifecycle.EventTag},
		{name: "a kind with control characters: recorded, and kept out of the entry ID", kind: "evt-stopped\n\tinjected"},
		{name: "an ordinary signal kind: recorded under the prefix", kind: string(lifecycle.SigSessionStarted)},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A distinct scope per case, so each assertion sees exactly its own
			// entry; the hook records into whatever scope the URL names.
			name := "sess-" + string(rune('a'+i))
			reg.Set(memory.NamespacedName{Namespace: ns, Name: name}, "tok-"+name, "")

			resp := postSignal(t, srv, "tok-"+name, ns, name, map[string]any{
				"kind": tc.kind,
				"at":   time.Unix(1770000000, 0).UTC(),
			})
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			require.Equal(t, http.StatusNoContent, resp.StatusCode, "_signal → 204, body=%s", b)

			ctx := memory.WithSystemApproval(context.Background(), "test")
			scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
			res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"lifecycle"}})
			require.NoError(t, err, "Query lifecycle")
			require.Len(t, res.Entries, 1)

			assert.Equal(t, []string{lifecycle.SignalTag(memory.SignalKind(tc.kind))}, res.Entries[0].Tags,
				"the recorded tag is the caller's kind inside the signal namespace and nothing else")
			assert.True(t, strings.HasPrefix(res.Entries[0].ID, "lifecycle-sig-"),
				"a recording's ID is namespaced away from Append's lifecycle-evt- events, got %q", res.Entries[0].ID)
			assert.NotContains(t, res.Entries[0].ID, "\n",
				"caller bytes must not reach an ID the operator signs, got %q", res.Entries[0].ID)

			ordered, rErr := lifecycle.ReadOrdered(ctx, mem, scope)
			require.NoError(t, rErr, "ReadOrdered")
			assert.Empty(t, ordered, "a recorded signal is never a transition event")
		})
	}
}
