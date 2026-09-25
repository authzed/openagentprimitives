package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

func TestToolSessionRegistry_RegisterFeedAndUnregister(t *testing.T) {
	reg := newToolSessionRegistry()
	var got [][]byte
	cancel := reg.register("alice-1-x", func(b []byte) error {
		got = append(got, append([]byte(nil), b...))
		return nil
	})

	assert.True(t, reg.feed("alice-1-x", []byte("hi")), "feed should route to registered ref")
	require.Len(t, got, 1)
	assert.Equal(t, []byte("hi"), got[0])

	cancel()
	assert.False(t, reg.feed("alice-1-x", []byte("after-cancel")),
		"feed after cancel should report no live bridge")
	assert.Len(t, got, 1, "no further deliveries after cancel")
}

func TestToolSessionRegistry_FeedUnknownRef(t *testing.T) {
	reg := newToolSessionRegistry()
	assert.False(t, reg.feed("nobody", []byte("x")),
		"unknown ref should return false (caller logs and drops)")
}

func TestToolSessionInputSubjectIsSessionScoped(t *testing.T) {
	got := toolSessionInputSubject("ns1", "sess1")
	assert.Equal(t, "ap.session.ns1.sess1.in.tool_session_input", got)
	assert.NotContains(t, got, "*", "must not be a wildcard subject")
}

func TestSubagentSendFunc_PublishesSignedEnvelope(t *testing.T) {
	seed := bytes.Repeat([]byte{0x33}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := channelevents.NewEnvelopeSigner(priv, provenance.SessionPublisher("demo-ns", "root-1"), "uid-1")
	require.NoError(t, err)

	var gotSubject string
	var got channelevents.Envelope
	publish := func(subject string, data []byte) error {
		gotSubject = subject
		return json.Unmarshal(data, &got)
	}
	send := subagentSendPublish(publish, "demo-ns", "root-1", signer)
	require.NoError(t, send(context.Background(), "demo-ns", "child-1", "do the thing"))

	assert.Equal(t,
		channelevents.SubjectIn(channelevents.SubjectPrefix("demo-ns", "root-1"), channelevents.KindAgentMessageSend),
		gotSubject)
	assert.Equal(t, "session:demo-ns/root-1", got.Publisher)
	assert.NoError(t, channelevents.VerifyEnvelopeSig(gotSubject, got, priv.Public().(ed25519.PublicKey)))
}
