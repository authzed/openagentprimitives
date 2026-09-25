package channelevents

import "testing"

func TestChannelHistorySubjectRoundTrip(t *testing.T) {
	subj := ChannelHistoryRequestSubject("ap.session.default.foo")
	if subj != "ap.session.default.foo.channel_history.request" {
		t.Fatalf("got %q", subj)
	}
	ns, name, ok := ParseChannelHistorySubject(subj)
	if !ok || ns != "default" || name != "foo" {
		t.Fatalf("parse got ns=%q name=%q ok=%v", ns, name, ok)
	}
	if _, _, ok := ParseChannelHistorySubject("ap.session.x.y.history.request"); ok {
		t.Fatal("thread-history subject must not parse as channel-history")
	}
}
