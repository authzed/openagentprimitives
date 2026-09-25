package subjects

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuiltSubjectsAreByteIdenticalToTheWire pins every subject this package
// builds to the exact literal that travels on the bus. These are live NATS
// subjects: a changed segment does not error anywhere, it silently stops
// delivery. The literals below are therefore written out longhand rather than
// composed from the package's own constants — a test that rebuilt the expected
// value the same way the code does would agree with any typo.
func TestBuiltSubjectsAreByteIdenticalToTheWire(t *testing.T) {
	const (
		ns   = "default"
		name = "foo"
	)
	p := Session(ns, name)

	cases := []struct {
		name string
		got  string
		want string
	}{
		// Per-session, concrete. Publishers.
		{"Session: the session prefix", p.String(), "ap.session.default.foo"},
		{"In: inbound leaf", p.In("user_message"), "ap.session.default.foo.in.user_message"},
		{"Out: outbound leaf", p.Out("user_message"), "ap.session.default.foo.out.user_message"},
		{"Out: dotted leaf keeps every token", p.Out("assistant.stream.delta"), "ap.session.default.foo.out.assistant.stream.delta"},
		{"History: thread-history request", p.History(), "ap.session.default.foo.history.request"},
		{"ChannelHistory: channel-history request", p.ChannelHistory(), "ap.session.default.foo.channel_history.request"},
		{"Tree: the whole per-session subtree", p.Tree(), "ap.session.default.foo.>"},
		{"OutTree: the outbound subtree", p.OutTree(), "ap.session.default.foo.out.>"},

		// Per-session, wildcarded. Subscribers.
		{"AnySessionPrefix: cluster-wide prefix", AnySessionPrefix, "ap.session.*.*"},
		{"AnyOutTree: cluster-wide outbound subscription", AnyOutTree, "ap.session.*.*.out.>"},
		{"AnyHistory: history responder subscription", AnyHistory, "ap.session.*.*.history.request"},
		{"AnyChannelHistory: channel-history responder subscription", AnyChannelHistory, "ap.session.*.*.channel_history.request"},

		// Every cluster-wide per-session subscription held in the tree today.
		{"AnySession In: view_message", AnySession().In("view_message"), "ap.session.*.*.in.view_message"},
		{"AnySession In: interrupt_request", AnySession().In("interrupt_request"), "ap.session.*.*.in.interrupt_request"},
		{"AnySession In: interaction_decision", AnySession().In("interaction_decision"), "ap.session.*.*.in.interaction_decision"},
		{"AnySession In: interaction_request", AnySession().In("interaction_request"), "ap.session.*.*.in.interaction_request"},
		{"AnySession In: interaction_applied", AnySession().In("interaction_applied"), "ap.session.*.*.in.interaction_applied"},
		{"AnySession In: resurface_request", AnySession().In("resurface_request"), "ap.session.*.*.in.resurface_request"},
		{"AnySession In: app_tool_call", AnySession().In("app_tool_call"), "ap.session.*.*.in.app_tool_call"},
		{"AnySession In: ui_data_binding", AnySession().In("ui_data_binding"), "ap.session.*.*.in.ui_data_binding"},
		{"AnySession In: ui_presence", AnySession().In("ui_presence"), "ap.session.*.*.in.ui_presence"},
		{"AnySession In: user_message", AnySession().In("user_message"), "ap.session.*.*.in.user_message"},
		{"AnySession In: metaagent_request", AnySession().In("metaagent_request"), "ap.session.*.*.in.metaagent_request"},
		{"AnySession In: metaagent_approval_applied", AnySession().In("metaagent_approval_applied"), "ap.session.*.*.in.metaagent_approval_applied"},
		{"AnySession In: tool_session_input", AnySession().In("tool_session_input"), "ap.session.*.*.in.tool_session_input"},
		{"AnySession Out: interrupt_applied", AnySession().Out("interrupt_applied"), "ap.session.*.*.out.interrupt_applied"},
		{"AnySession Out: interaction_applied", AnySession().Out("interaction_applied"), "ap.session.*.*.out.interaction_applied"},
		{"AnySession Out: metaagent_scope_approval", AnySession().Out("metaagent_scope_approval"), "ap.session.*.*.out.metaagent_scope_approval"},
		{"AnySession Out: metaagent_notice", AnySession().Out("metaagent_notice"), "ap.session.*.*.out.metaagent_notice"},
		{"AnySession Out: tool_session_delta", AnySession().Out("tool_session_delta"), "ap.session.*.*.out.tool_session_delta"},
		{"AnySession Out: tool_session_event", AnySession().Out("tool_session_event"), "ap.session.*.*.out.tool_session_event"},

		// Cluster-wide fixed subjects.
		{"AllTree: the whole-bus grant", AllTree, "ap.>"},
		{"Monitoring: framework health fanout", Monitoring, "ap.monitoring.events"},
		{"SessionAttached: outputChannel attach fanout", SessionAttached, "ap.channel.session_attached"},
		{"Revocation: in-flight revocation bus", Revocation, "ap.revocation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}

// TestPrefixOfAdoptsAStoredPrefixIdentically covers the AgentSession.status.
// natsSubjectPrefix path: a consumer handed the stored prefix must reach the
// same subjects a consumer that knows (ns, name) builds.
func TestPrefixOfAdoptsAStoredPrefixIdentically(t *testing.T) {
	built := Session("team-a", "sess-1")
	stored := PrefixOf("ap.session.team-a.sess-1")

	assert.Equal(t, built.String(), stored.String())
	assert.Equal(t, built.In("user_message"), stored.In("user_message"))
	assert.Equal(t, built.Out("notification"), stored.Out("notification"))
	assert.Equal(t, built.History(), stored.History())
	assert.Equal(t, built.ChannelHistory(), stored.ChannelHistory())
	assert.Equal(t, built.Tree(), stored.Tree())
	assert.Equal(t, built.OutTree(), stored.OutTree())
}

// TestWildcardFormIsTheConcreteFormWithTheSessionTokensWildcarded is the
// anti-drift property this package exists for: a subscriber's pattern and its
// publisher's subject must differ ONLY in the two session tokens. If they can
// differ anywhere else, the subscription can silently stop matching.
func TestWildcardFormIsTheConcreteFormWithTheSessionTokensWildcarded(t *testing.T) {
	leaves := []string{
		"user_message", "view_message", "interaction_request", "interaction_applied",
		"interaction_decision", "resurface_request", "app_tool_call", "ui_data_binding",
		"ui_action", "ui_presence", "interrupt_request", "interrupt_applied",
		"metaagent_request", "metaagent_notice", "assistant.stream.delta",
	}
	build := map[string]func(Prefix, string) string{
		"In":  Prefix.In,
		"Out": Prefix.Out,
	}
	for dir, fn := range build {
		for _, leaf := range leaves {
			t.Run(dir+"/"+leaf, func(t *testing.T) {
				concrete := strings.Split(fn(Session("team-a", "sess-1"), leaf), ".")
				wildcard := strings.Split(fn(AnySession(), leaf), ".")
				require.Len(t, wildcard, len(concrete), "wildcard and concrete must have the same token count")
				concrete[2], concrete[3] = Any, Any
				assert.Equal(t, wildcard, concrete)
			})
		}
	}

	// The two fixed leaves take the same treatment.
	fixed := map[string]struct{ concrete, wildcard string }{
		"History":        {Session("team-a", "sess-1").History(), AnyHistory},
		"ChannelHistory": {Session("team-a", "sess-1").ChannelHistory(), AnyChannelHistory},
	}
	for name, tc := range fixed {
		t.Run(name, func(t *testing.T) {
			concrete := strings.Split(tc.concrete, ".")
			concrete[2], concrete[3] = Any, Any
			assert.Equal(t, strings.Split(tc.wildcard, "."), concrete)
		})
	}
}

// TestParsersInvertTheirBuilders walks each builder/parser pair, since a parser
// that disagrees with its builder is the same silent-delivery-loss failure in
// the receive direction.
func TestParsersInvertTheirBuilders(t *testing.T) {
	const (
		ns   = "team-a"
		name = "sess-1"
	)
	p := Session(ns, name)

	t.Run("ParseIn round-trips a simple leaf", func(t *testing.T) {
		gotNS, gotName, leaf, ok := ParseIn(p.In("user_message"))
		require.True(t, ok)
		assert.Equal(t, ns, gotNS)
		assert.Equal(t, name, gotName)
		assert.Equal(t, "user_message", leaf)
	})

	t.Run("ParseOut round-trips a dotted leaf whole", func(t *testing.T) {
		gotNS, gotName, leaf, ok := ParseOut(p.Out("assistant.stream.delta"))
		require.True(t, ok)
		assert.Equal(t, ns, gotNS)
		assert.Equal(t, name, gotName)
		assert.Equal(t, "assistant.stream.delta", leaf)
	})

	t.Run("ParseHistory round-trips", func(t *testing.T) {
		gotNS, gotName, ok := ParseHistory(p.History())
		require.True(t, ok)
		assert.Equal(t, ns, gotNS)
		assert.Equal(t, name, gotName)
	})

	t.Run("ParseChannelHistory round-trips", func(t *testing.T) {
		gotNS, gotName, ok := ParseChannelHistory(p.ChannelHistory())
		require.True(t, ok)
		assert.Equal(t, ns, gotNS)
		assert.Equal(t, name, gotName)
	})
}

// TestParsersRejectSubjectsFromAnotherFamily proves each parser refuses the
// subjects its siblings own, so a handler pinned to one leaf cannot be driven
// by traffic addressed to another.
func TestParsersRejectSubjectsFromAnotherFamily(t *testing.T) {
	p := Session("team-a", "sess-1")

	cases := []struct {
		name    string
		subject string
		parse   func(string) bool
	}{
		{"ParseIn refuses an outbound subject", p.Out("user_message"), func(s string) bool { _, _, _, ok := ParseIn(s); return ok }},
		{"ParseIn refuses a history subject", p.History(), func(s string) bool { _, _, _, ok := ParseIn(s); return ok }},
		{"ParseIn refuses an empty namespace token", "ap.session..sess-1.in.user_message", func(s string) bool { _, _, _, ok := ParseIn(s); return ok }},
		{"ParseIn refuses an empty subject", "", func(s string) bool { _, _, _, ok := ParseIn(s); return ok }},
		{"ParseOut refuses an inbound subject", p.In("user_message"), func(s string) bool { _, _, _, ok := ParseOut(s); return ok }},
		{"ParseOut refuses a history subject", p.History(), func(s string) bool { _, _, _, ok := ParseOut(s); return ok }},
		{"ParseOut refuses an empty name token", "ap.session.team-a..out.user_message", func(s string) bool { _, _, _, ok := ParseOut(s); return ok }},
		{"ParseHistory refuses an outbound subject", p.Out("user_message"), func(s string) bool { _, _, ok := ParseHistory(s); return ok }},
		{"ParseHistory refuses a channel-history subject", p.ChannelHistory(), func(s string) bool { _, _, ok := ParseHistory(s); return ok }},
		{"ParseHistory refuses a missing name token", "ap.session.default.history.request", func(s string) bool { _, _, ok := ParseHistory(s); return ok }},
		{"ParseHistory refuses a non-session root", "ap.channelsd.history.request", func(s string) bool { _, _, ok := ParseHistory(s); return ok }},
		{"ParseHistory refuses an empty subject", "", func(s string) bool { _, _, ok := ParseHistory(s); return ok }},
		{"ParseChannelHistory refuses a thread-history subject", p.History(), func(s string) bool { _, _, ok := ParseChannelHistory(s); return ok }},
		{"ParseChannelHistory refuses an empty namespace token", "ap.session..y.channel_history.request", func(s string) bool { _, _, ok := ParseChannelHistory(s); return ok }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, tc.parse(tc.subject), "subject %q must not parse", tc.subject)
		})
	}
}
