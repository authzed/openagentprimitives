package channelevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHistorySubject_RoundTrip(t *testing.T) {
	subj := HistoryRequestSubject(SubjectPrefix("default", "sess-1"))
	assert.Equal(t, "ap.session.default.sess-1.history.request", subj)

	ns, name, ok := ParseHistorySubject(subj)
	assert.True(t, ok)
	assert.Equal(t, "default", ns)
	assert.Equal(t, "sess-1", name)
}

func TestParseHistorySubject_Rejects(t *testing.T) {
	cases := []string{
		"ap.session.default.sess-1.out.user_message", // wrong suffix
		"ap.session.default.history.request",         // missing name token
		"ap.channelsd.history.request",               // not session-scoped
		"",
	}
	for _, c := range cases {
		_, _, ok := ParseHistorySubject(c)
		assert.False(t, ok, "subject %q must not parse", c)
	}
}
