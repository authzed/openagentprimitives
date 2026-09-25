package local

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecordingSink_CapturesEvents(t *testing.T) {
	s := &RecordingSink{}
	s.Emit(MsgUserMessage{Text: "hello"})
	s.Emit(MsgNotification{Text: "thinking…"})
	got := s.Events()
	assert.Len(t, got, 2)
	assert.Equal(t, MsgUserMessage{Text: "hello"}, got[0])
	assert.IsType(t, MsgNotification{}, got[1])
}

func TestRenderEvents_HaveSessionRef(t *testing.T) {
	// Every render event carries the session it concerns so the TUI can
	// drop events for a stale session if the user ever drives more than
	// one (future). v1 has exactly one session, but the field is set.
	e := MsgPlanUpdate{Session: SessionRef{Namespace: "default", Name: "s1"}}
	assert.Equal(t, "default", e.Session.Namespace)
	assert.Equal(t, "s1", e.Session.Name)
}
