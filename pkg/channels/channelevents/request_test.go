package channelevents

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestInPublishesOnTheInSubjectAndReturnsTheReply(t *testing.T) {
	var gotSubject string
	var gotEnv Envelope

	req := func(subject string, data []byte, _ time.Duration) ([]byte, error) {
		gotSubject = subject
		require.NoError(t, json.Unmarshal(data, &gotEnv))
		return []byte(`{"outcome":"routed"}`), nil
	}

	reply, err := RequestIn(req, "ns", "sess", KindViewMessage,
		ViewMessagePayload{Text: "hi"}, 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, "ap.session.ns.sess.in.view_message", gotSubject)
	assert.Equal(t, KindViewMessage, gotEnv.Kind)
	assert.Equal(t, "ns", gotEnv.Session.Namespace)
	assert.Equal(t, "sess", gotEnv.Session.Name)
	assert.JSONEq(t, `{"outcome":"routed"}`, string(reply))
}

func TestRequestInPropagatesTransportError(t *testing.T) {
	wantErr := errors.New("nats: timeout")
	req := func(string, []byte, time.Duration) ([]byte, error) { return nil, wantErr }

	_, err := RequestIn(req, "ns", "sess", KindViewMessage, ViewMessagePayload{}, time.Second)
	require.Error(t, err, "a transport failure must never be swallowed")
	assert.ErrorIs(t, err, wantErr)
	// ErrorIs alone cannot tell a wrapped error from a bare `return nil, err`.
	// AGENTS.md requires the wrap to name the failing operation.
	assert.Contains(t, err.Error(), "channelevents: RequestIn",
		"the error must name the operation that failed, not just propagate the cause")
}

func TestRequestInRejectsUnimplementedKind(t *testing.T) {
	req := func(string, []byte, time.Duration) ([]byte, error) {
		t.Fatal("must not reach the transport for an unimplemented kind")
		return nil, nil
	}
	_, err := RequestIn(req, "ns", "sess", Kind("not_a_real_kind"), struct{}{}, time.Second)
	require.Error(t, err)
}

func TestRequestInRejectsNilRequester(t *testing.T) {
	_, err := RequestIn(nil, "ns", "sess", KindViewMessage, ViewMessagePayload{}, time.Second)
	require.Error(t, err, "a nil requester is a wiring bug and must fail loudly, not nil-panic")
}

// TestRequestInRejectsReservedKindBeforeTheTransport pins the Implemented()
// guard, which TestRequestInRejectsUnimplementedKind does not: that test's
// kind name is also not Valid(), so BuildEnvelope's Envelope.Validate would
// reject it even with the guard deleted.
//
// A RESERVED kind is the case the guard exists for — Valid (so Validate lets
// it through) but with no responder subscribed, so a request would block for
// the full timeout instead of failing fast.
func TestRequestInRejectsReservedKindBeforeTheTransport(t *testing.T) {
	require.True(t, KindPermissionGrant.Valid(), "precondition: reserved kind is Valid")
	require.False(t, KindPermissionGrant.Implemented(), "precondition: reserved kind is not Implemented")

	req := func(string, []byte, time.Duration) ([]byte, error) {
		t.Fatal("must not reach the transport for a reserved kind: no responder, would hang until timeout")
		return nil, nil
	}
	_, err := RequestIn(req, "ns", "sess", KindPermissionGrant, struct{}{}, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unimplemented kind")
}
