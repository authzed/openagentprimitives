// in.metaagent_request has two publishers in two packages nobody edits
// together: the runner's cold-start scope hook (pkg/authz/hooks) and a channel
// kind's metaagent-mention listener. authzd is the only consumer. A per-package
// test cannot see that join — each side stays green while the two disagree
// about a field name — so this asserts the decode against bytes built exactly
// the way each publisher builds them.
package main

import (
	"encoding/json"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// receiveOneRequest pre-registers a session queue on the worker so Handle
// delivers into a channel the test owns instead of a goroutine that consumes
// and discards. It is the smallest seam that shows what the handler decoded.
func receiveOneRequest(t *testing.T, w *MetaagentWorker, sessionRef string) chan metaagentRequest {
	t.Helper()
	ch := make(chan metaagentRequest, 1)
	w.mu.Lock()
	w.sessions[sessionRef] = ch
	w.mu.Unlock()
	return ch
}

// TestMetaagentRequestDecodesEveryPublishersWireShape feeds authzd the bytes
// each publisher actually emits and requires every field to arrive.
func TestMetaagentRequestDecodesEveryPublishersWireShape(t *testing.T) {
	const sessionRef = "ns1/sess-a"
	subject := channelevents.SubjectIn(
		channelevents.SubjectPrefix("ns1", "sess-a"), channelevents.KindMetaagentRequest)

	classEnvelope := hooks.ColdStartEnvelope(
		[]scope.EnvelopeBoundEntity{{ResourceType: "document", Permission: "view"}},
		[]string{"code_gh"},
	)
	// Byte-for-byte the map literal pkg/authz/hooks marshals. Written out here
	// rather than reusing a shared constructor precisely so a rename on either
	// side shows up as a failure instead of moving in lockstep.
	runnerBytes, err := json.Marshal(map[string]any{
		"requester":       "alice-canonical",
		"text":            "summarize the incident doc",
		"coldStart":       true,
		"envelope":        classEnvelope,
		"approvalTimeout": (7 * time.Minute).String(),
	})
	require.NoError(t, err, "marshal the runner's cold-start payload")

	// The channel-kind listener's shape, built through the shared helper.
	var listenerBytes []byte
	require.NoError(t, channelevents.PublishMetaagentIn(
		func(_ string, data []byte) error { listenerBytes = data; return nil },
		"ns1", "sess-a", channelevents.KindMetaagentRequest,
		channelevents.MetaagentRequestPayload{Requester: "user:alice", Text: "widen scope to the runbooks repo"},
	), "publish the mention payload")

	cases := []struct {
		name string
		data []byte
		want metaagentRequest
	}{
		{
			name: "runner cold-start payload: requester, text, coldStart, envelope and timeout all survive",
			data: runnerBytes,
			want: metaagentRequest{
				requester:       "alice-canonical",
				text:            "summarize the incident doc",
				coldStart:       true,
				envelope:        classEnvelope,
				approvalTimeout: 7 * time.Minute,
			},
		},
		{
			name: "channel-kind mention payload: mid-session request with no envelope or timeout",
			data: listenerBytes,
			want: metaagentRequest{
				requester: "user:alice",
				text:      "widen scope to the runbooks repo",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewMetaagentWorker(buildFakeMetaagent(), approval.New(), nil, nil)
			ch := receiveOneRequest(t, w, sessionRef)

			metaagentRequestHandler(approvedCtx(), w)(&natsgo.Msg{Subject: subject, Data: tc.data})

			select {
			case got := <-ch:
				assert.Equal(t, sessionRef, got.scopeRef.ID, "the session comes from the subject")
				assert.Equal(t, tc.want.requester, got.requester)
				assert.Equal(t, tc.want.text, got.text)
				assert.Equal(t, tc.want.coldStart, got.coldStart)
				assert.Equal(t, tc.want.envelope, got.envelope,
					"the AgentClass envelope must survive the raw-JSON hop through channelevents")
				assert.Equal(t, tc.want.approvalTimeout, got.approvalTimeout)
			default:
				t.Fatal("the handler decoded nothing; no request reached the worker")
			}
		})
	}
}

// TestMetaagentRequestSurvivesAMalformedEnvelope pins the degrade: the class
// envelope only widens what classification can keep in-envelope, so a
// publisher that sends a broken one must still get its scope request reviewed
// rather than silently dropped on the floor.
func TestMetaagentRequestSurvivesAMalformedEnvelope(t *testing.T) {
	const sessionRef = "ns1/sess-a"
	w := NewMetaagentWorker(buildFakeMetaagent(), approval.New(), nil, nil)
	ch := receiveOneRequest(t, w, sessionRef)

	metaagentRequestHandler(approvedCtx(), w)(&natsgo.Msg{
		Subject: channelevents.SubjectIn(
			channelevents.SubjectPrefix("ns1", "sess-a"), channelevents.KindMetaagentRequest),
		Data: []byte(`{"requester":"user:alice","text":"widen","envelope":"not-an-object"}`),
	})

	select {
	case got := <-ch:
		assert.Equal(t, "user:alice", got.requester)
		assert.Equal(t, "widen", got.text)
		assert.Equal(t, scope.AgentClassEnvelope{}, got.envelope,
			"an undecodable envelope degrades to empty, it does not abort the request")
	default:
		t.Fatal("a malformed envelope must not drop the whole request")
	}
}
