package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func titleSess() *tool.SessionContext {
	return &tool.SessionContext{Namespace: "default", Name: "sess1"}
}

func TestSetThreadTitle_Name(t *testing.T) {
	assert.Equal(t, "set_thread_title", meta.NewSetThreadTitle(meta.SetThreadTitleConfig{}).Name())
}

func TestSetThreadTitle_PublishesEnvelope(t *testing.T) {
	pub := &fakeNATSPublish{}
	tl := meta.NewSetThreadTitle(meta.SetThreadTitleConfig{NATSPublish: pub.publish})

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"title":"Refactoring the auth layer","emoji":"🔧"}`), titleSess())
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.True(t, res.Trusted)

	require.Len(t, pub.subjects, 1)
	assert.Contains(t, string(pub.subjects[0]), "ap.session.default.sess1.out.thread_title")

	env := decodeEnvelope(t, pub.subjects[0])
	var pl channelevents.ThreadTitlePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "Refactoring the auth layer", pl.Title)
	assert.Equal(t, "🔧", pl.Emoji)
}

func TestSetThreadTitle_EmojiOptional(t *testing.T) {
	pub := &fakeNATSPublish{}
	tl := meta.NewSetThreadTitle(meta.SetThreadTitleConfig{NATSPublish: pub.publish})

	_, err := tl.Execute(context.Background(), json.RawMessage(`{"title":"Q3 revenue"}`), titleSess())
	require.NoError(t, err)
	require.Len(t, pub.subjects, 1)
	env := decodeEnvelope(t, pub.subjects[0])
	var pl channelevents.ThreadTitlePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "Q3 revenue", pl.Title)
	assert.Empty(t, pl.Emoji)
}

func TestSetThreadTitle_EmptyTitle_ErrorNoPublish(t *testing.T) {
	pub := &fakeNATSPublish{}
	tl := meta.NewSetThreadTitle(meta.SetThreadTitleConfig{NATSPublish: pub.publish})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"title":"   "}`), titleSess())
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted)
	assert.Contains(t, res.Content, "title")
	assert.Empty(t, pub.subjects, "no publish on validation failure")
}

func TestSetThreadTitle_NoNATSPublish_ErrorTrusted(t *testing.T) {
	res, err := meta.NewSetThreadTitle(meta.SetThreadTitleConfig{}).
		Execute(context.Background(), json.RawMessage(`{"title":"x"}`), titleSess())
	require.Error(t, err)
	assert.True(t, res.Trusted)
}

func TestSetThreadTitle_EmojiLengthBounded(t *testing.T) {
	pub := &fakeNATSPublish{}
	tl := meta.NewSetThreadTitle(meta.SetThreadTitleConfig{NATSPublish: pub.publish})
	// A stray sentence in the emoji slot is dropped (bounded to <=16 runes).
	long := `{"title":"x","emoji":"this is clearly not an emoji it is a whole sentence"}`
	_, err := tl.Execute(context.Background(), json.RawMessage(long), titleSess())
	require.NoError(t, err)
	require.Len(t, pub.subjects, 1)
	env := decodeEnvelope(t, pub.subjects[0])
	var pl channelevents.ThreadTitlePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Empty(t, pl.Emoji, "over-long emoji is treated as unset")
}
