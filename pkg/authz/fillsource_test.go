package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestAllowsFill(t *testing.T) {
	cases := []struct {
		name     string
		fillFrom []string
		src      authz.FillSource
		want     bool
	}{
		{
			name:     "unset fillFrom narrows nothing: default allowed",
			fillFrom: nil,
			src:      authz.FillDefault,
			want:     true,
		},
		{
			name:     "unset fillFrom narrows nothing: every other source allowed too",
			fillFrom: nil,
			src:      authz.FillChannelThread,
			want:     true,
		},
		{
			name:     "empty (not nil) fillFrom narrows nothing either",
			fillFrom: []string{},
			src:      authz.FillQuery,
			want:     true,
		},
		{
			name:     "named source is allowed",
			fillFrom: []string{"default", "ask"},
			src:      authz.FillDefault,
			want:     true,
		},
		{
			name:     "unnamed source is refused once the list is written",
			fillFrom: []string{"ask"},
			src:      authz.FillDefault,
			want:     false,
		},
		{
			name:     "query spelling selects the extractor source",
			fillFrom: []string{"query"},
			src:      authz.FillQuery,
			want:     true,
		},
		{
			name:     "extract is an ALIAS of query, not a source that gates nothing",
			fillFrom: []string{"extract"},
			src:      authz.FillQuery,
			want:     true,
		},
		{
			name:     "naming query does not admit channel_thread",
			fillFrom: []string{"query"},
			src:      authz.FillChannelThread,
			want:     false,
		},
		{
			name:     "metaagent is refused when the list omits it",
			fillFrom: []string{"default"},
			src:      authz.FillMetaagent,
			want:     false,
		},
		{
			name:     "unset fillFrom narrows nothing: trigger allowed too",
			fillFrom: nil,
			src:      authz.FillTrigger,
			want:     true,
		},
		{
			name:     "trigger is refused when the list omits it",
			fillFrom: []string{"default"},
			src:      authz.FillTrigger,
			want:     false,
		},
		{
			name:     "trigger spelling selects the trigger source",
			fillFrom: []string{"trigger"},
			src:      authz.FillTrigger,
			want:     true,
		},
		{
			name:     "naming trigger does not admit default",
			fillFrom: []string{"trigger"},
			src:      authz.FillDefault,
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.AllowsFill(tc.fillFrom, tc.src))
			// The method form must agree with the free function; they are the
			// same rule reached from the two sides of the adapter boundary.
			spec := authz.BoundEntitySpec{FillFrom: tc.fillFrom}
			assert.Equal(t, tc.want, spec.AllowsFill(tc.src), "method form must match the free function")
		})
	}
}

// TestAllowsFill_OnlyEverNarrows is the property that makes the field safe to
// read at a glance: writing a fillFrom can remove ways an instance binds but
// can never add one, so no reader has to check whether a declaration widened
// something.
func TestAllowsFill_OnlyEverNarrows(t *testing.T) {
	every := []authz.FillSource{
		authz.FillDefault, authz.FillQuery, authz.FillAsk,
		authz.FillChannelThread, authz.FillMetaagent,
	}
	for _, src := range every {
		assert.True(t, authz.AllowsFill(nil, src), "unset must allow %s", src)
	}
	// Any non-empty list refuses at least one source — it cannot be a no-op in
	// the widening direction because there is nothing wider than unset.
	narrowed := []string{"ask"}
	refused := 0
	for _, src := range every {
		if !authz.AllowsFill(narrowed, src) {
			refused++
		}
	}
	assert.Equal(t, len(every)-1, refused, "a single-source list must refuse every other source")
}

// TestObservedFillSource pins the fourth mirror of the fillFrom vocabulary
// (pkg/authz/fillsource.go's fillSourceSpellings row): a slot declaring
// `observed` and nothing else must admit no other route, and must not go
// missing from the unset (narrows-nothing) case either.
func TestObservedFillSource(t *testing.T) {
	assert.True(t, authz.AllowsFill([]string{"observed"}, authz.FillObserved))

	// A slot that says `observed` and nothing else admits no other route: not
	// a class-pinned default, not a user-named instance, not a thread seed —
	// every one of which would put an instance in the slot with no observation
	// ever made about it.
	for _, other := range []authz.FillSource{
		authz.FillDefault, authz.FillQuery, authz.FillAsk,
		authz.FillChannelThread, authz.FillMetaagent,
	} {
		assert.Falsef(t, authz.AllowsFill([]string{"observed"}, other),
			"a slot filled only from observations must not admit %s", other)
	}

	// And an unset fillFrom still narrows nothing, as today.
	assert.True(t, authz.AllowsFill(nil, authz.FillObserved))
}

// TestExplicitlyAllowsFill pins the one property that makes it different
// from AllowsFill: an unset (or empty) fillFrom is FALSE here, for every
// source, not just for trigger — this function has no "unset narrows
// nothing" default at all. TriggerSlotRequestsFor is the only expected
// caller, gating FillTrigger specifically, but the function itself does not
// special-case the source.
func TestExplicitlyAllowsFill(t *testing.T) {
	cases := []struct {
		name     string
		fillFrom []string
		src      authz.FillSource
		want     bool
	}{
		{
			name:     "unset fillFrom is FALSE — the opposite of AllowsFill's default",
			fillFrom: nil,
			src:      authz.FillTrigger,
			want:     false,
		},
		{
			name:     "empty (not nil) fillFrom is FALSE too",
			fillFrom: []string{},
			src:      authz.FillTrigger,
			want:     false,
		},
		{
			name:     "trigger named explicitly is TRUE",
			fillFrom: []string{"trigger"},
			src:      authz.FillTrigger,
			want:     true,
		},
		{
			name:     "trigger named alongside another source is still TRUE",
			fillFrom: []string{"query", "trigger"},
			src:      authz.FillTrigger,
			want:     true,
		},
		{
			name:     "a list that omits trigger is FALSE",
			fillFrom: []string{"query", "ask"},
			src:      authz.FillTrigger,
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.ExplicitlyAllowsFill(tc.fillFrom, tc.src))
		})
	}
}

// TestAllowsExtractedBinding_AskAndQueryShareOnePath pins the decision that
// `ask` needs no mechanism of its own. query is the user naming an instance
// unprompted; ask is the agent prompting and waiting. They differ in who
// started the exchange, not in how the value binds — either way it arrives in a
// user message, the extractor proposes it, and it binds only on a Check.
//
// The sharp case is the third row: before this, a slot declaring only `ask` was
// excluded from every implemented source, so it bound NOTHING while its YAML
// read like a working control.
func TestAllowsExtractedBinding_AskAndQueryShareOnePath(t *testing.T) {
	cases := []struct {
		name     string
		fillFrom []string
		want     bool
	}{
		{name: "query admits the extractor", fillFrom: []string{"query"}, want: true},
		{name: "extract (the alias) admits it", fillFrom: []string{"extract"}, want: true},
		{name: "ask admits it — the agent solicited the same answer", fillFrom: []string{"ask"}, want: true},
		{name: "ask alongside another source still admits it", fillFrom: []string{"channel_thread", "ask"}, want: true},
		{name: "unset admits everything", fillFrom: nil, want: true},
		{name: "default alone does NOT admit the extractor", fillFrom: []string{"default"}, want: false},
		{name: "channel_thread alone does NOT admit it", fillFrom: []string{"channel_thread"}, want: false},
		{name: "metaagent alone does NOT admit it", fillFrom: []string{"metaagent"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authz.AllowsExtractedBinding(tc.fillFrom))
		})
	}
}
