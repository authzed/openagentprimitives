package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateRoundTripsEachType(t *testing.T) {
	st := NewState()
	require.False(t, st.Has("agent"), "a fresh State holds nothing")

	st.Set("agent", "demo-agent")
	st.SetBool("hasApp", true)
	st.SetAll("features", []string{"history.thread", "user.lookup"})

	assert.Equal(t, "demo-agent", st.Get("agent"))
	assert.True(t, st.Bool("hasApp"))
	assert.Equal(t, []string{"history.thread", "user.lookup"}, st.All("features"))
	assert.True(t, st.Has("agent"))
	assert.True(t, st.Has("hasApp"))
	assert.True(t, st.Has("features"))
}

func TestStateZeroValuesForAbsentKeys(t *testing.T) {
	st := NewState()
	assert.Equal(t, "", st.Get("nope"))
	assert.False(t, st.Bool("nope"))
	assert.Nil(t, st.All("nope"))
	assert.False(t, st.Has("nope"))
}

func TestStateHasIsTrueForAnExplicitEmptyString(t *testing.T) {
	// "answered, and the answer was empty" must be distinguishable from
	// "unanswered" — non-interactive mode fails closed on the latter only.
	st := NewState()
	st.Set("note", "")
	assert.True(t, st.Has("note"))
	assert.Equal(t, "", st.Get("note"))
}

func TestStateNotesPreserveOrder(t *testing.T) {
	st := NewState()
	st.Note("Agent", "demo-agent")
	st.Note("Channel", "demo-channel")
	assert.Equal(t, []Note{
		{Label: "Agent", Value: "demo-agent"},
		{Label: "Channel", Value: "demo-channel"},
	}, st.Notes())
}

func TestStateAllReturnsACopy(t *testing.T) {
	st := NewState()
	st.SetAll("features", []string{"a", "b"})
	got := st.All("features")
	got[0] = "mutated"
	assert.Equal(t, []string{"a", "b"}, st.All("features"),
		"All must not hand out an aliased slice callers can corrupt")
}
