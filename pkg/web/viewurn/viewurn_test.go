package viewurn

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatParseRoundTrip(t *testing.T) {
	cases := []struct {
		name         string
		typ, id, sub string
		want         string
	}{
		{name: "artifact head", typ: "artifact", id: "artifact-3f2a1b8c", want: "urn:ap:view:artifact:artifact-3f2a1b8c"},
		{name: "artifact pinned revision", typ: "artifact", id: "artifact-3f2a1b8c", sub: "artrev-9d4e0117", want: "urn:ap:view:artifact:artifact-3f2a1b8c/artrev-9d4e0117"},
		{name: "chat (no id)", typ: "chat", want: "urn:ap:view:chat"},
		{name: "tui (no id)", typ: "tui", want: "urn:ap:view:tui"},
		{name: "session (ns/name pair)", typ: "session", id: "demo-ns/demo-sess", want: "urn:ap:view:session:demo-ns/demo-sess"},
		{name: "session with sub facet", typ: "session", id: "demo-ns/demo-sess", sub: "annotations", want: "urn:ap:view:session:demo-ns/demo-sess/annotations"},
		{name: "session with widget facet", typ: TypeSession, id: "demo-ns/demo-sess", sub: SubWidget, want: "urn:ap:view:session:demo-ns/demo-sess/widget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Format(tc.typ, tc.id, tc.sub)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			u, err := Parse(got)
			require.NoError(t, err, "the URN we just formatted must parse")
			assert.Equal(t, tc.typ, u.Type)
			assert.Equal(t, tc.id, u.ID)
			assert.Equal(t, tc.sub, u.Sub)
		})
	}
}

func TestParseFailsClosed(t *testing.T) {
	cases := []string{
		"",                                                 // empty is not a URN (callers tolerate "" separately)
		"urn:ap:view:",                                     // no type
		"urn:ap:view:artifact:",                            // type but empty id
		"urn:ap:view:UNKNOWN:x",                            // unregistered type
		"urn:ap:view:artifact:Artifact-3f",                 // uppercase not allowed by grammar
		"urn:ap:view:artifact:a b",                         // space
		"urn:ap:view:artifact:x/y/z",                       // more than one sub-segment
		"http://evil/urn:ap:view:artifact:x",               // prefix must be exact
		"urn:ap:view:artifact:<script>",                    // markup — must never parse
		"urn:ap:view:chat:x",                               // bare type must not carry a value
		"urn:ap:view:session:",                             // type but empty id
		"urn:ap:view:session:demo-ns",                      // single segment — a pair type needs ns AND name
		"urn:ap:view:session:demo-ns/demo-sess/extra/more", // more than one sub-segment past the pair
		"urn:ap:view:session:../demo-sess",                 // path traversal
		"urn:ap:view:session:demo-ns/..",                   // path traversal in the name segment
		"urn:ap:view:session:demo-ns/<script>",             // markup — must never parse
		"urn:ap:view:session:demo ns/demo-sess",            // space
		"urn:ap:view:session:Demo-Ns/demo-sess",            // uppercase not allowed by grammar
	}
	for _, s := range cases {
		t.Run("rejects "+s, func(t *testing.T) {
			_, err := Parse(s)
			require.Error(t, err, "a malformed/unsafe URN must fail closed")
		})
	}
}

func TestDescribe(t *testing.T) {
	assert.Equal(t, "the artifact view of artifact-3f2a1b8c", Describe("urn:ap:view:artifact:artifact-3f2a1b8c"))
	assert.Equal(t, "the artifact view of artifact-3f2a1b8c/artrev-9d4e0117", Describe("urn:ap:view:artifact:artifact-3f2a1b8c/artrev-9d4e0117"))
	assert.Equal(t, "the web chat", Describe("urn:ap:view:chat"))
	assert.Equal(t, "the terminal", Describe("urn:ap:view:tui"))
	// Describe never panics on garbage — it is called on stored data that
	// passed Parse at write time, but be robust.
	assert.Equal(t, "", Describe(""))
	assert.Equal(t, "", Describe("not-a-urn"))
}

func TestFormatRejectsUnsafeInputs(t *testing.T) {
	_, err := Format("artifact", "Bad Id", "")
	require.Error(t, err, "Format must reject an id that violates the grammar")
	_, err = Format("unknown-type", "x", "")
	require.Error(t, err, "Format must reject an unregistered type")
	_, err = Format("artifact", "ab/cd", "")
	require.Error(t, err, "a slash inside id must be rejected, not reinterpreted as id/sub")
	_, err = Format("artifact", "ab", "cd/ef")
	require.Error(t, err, "a slash inside sub must be rejected")

	_, err = Format("session", "demo-ns", "")
	require.Error(t, err, "session id must be a ns/name pair, not a single segment")
	_, err = Format("session", "demo-ns/demo-sess/extra", "")
	require.Error(t, err, "session id must be exactly two segments; extra segments belong in sub")
	_, err = Format("session", "Demo-Ns/demo-sess", "")
	require.Error(t, err, "session id segments must satisfy the same lowercase grammar as any other segment")
	_, err = Format("session", "demo-ns/demo-sess", "cd/ef")
	require.Error(t, err, "a slash inside sub must be rejected for pair-id types too")
}

func TestIsAnnotations(t *testing.T) {
	assert.True(t, IsAnnotations("urn:ap:view:artifact:art-1/annotations"), "artifact/annotations is an annotation batch")
	assert.False(t, IsAnnotations("urn:ap:view:artifact:art-1"), "plain artifact Via (no sub) is not")
	assert.False(t, IsAnnotations("urn:ap:view:artifact:art-1/other"), "a different sub is not")
	assert.False(t, IsAnnotations("urn:ap:view:chat"), "a bare type is not")
	assert.False(t, IsAnnotations("not-a-urn"), "garbage fails closed")
	assert.False(t, IsAnnotations(""), "empty fails closed")
}

func TestDescribeAnnotationsSub(t *testing.T) {
	assert.Equal(t, "annotations on the artifact view of art-1", Describe("urn:ap:view:artifact:art-1/annotations"))
}

// The session view is where interact/handlers.go mints a Via on every send, so
// its phrase is the one both consumers (the runner's LLM context and the Slack
// user echo) render most. The phrase deliberately carries no ns/name: it is
// rendered into a Slack message and into the agent's context, neither of which
// should carry internal resource coordinates.
func TestDescribeSession(t *testing.T) {
	assert.Equal(t, "the session view", Describe("urn:ap:view:session:demo-ns/demo-sess"))
	assert.Equal(t, "a widget action in the session view", Describe("urn:ap:view:session:demo-ns/demo-sess/widget"))
	assert.Equal(t, "the session view", Describe("urn:ap:view:session:demo-ns/demo-sess/other"),
		"an unrecognized sub degrades to the plain phrase, never to empty")
}

// Guard against the next registered type shipping without a Describe arm — the
// exact way TypeSession stayed silent. Derived from registeredTypes rather than
// a transcribed list, so a new entry is covered the moment it is added.
func TestDescribeCoversEveryRegisteredType(t *testing.T) {
	for typ, shape := range registeredTypes {
		t.Run(typ+": Describe returns a phrase", func(t *testing.T) {
			id := ""
			switch shape {
			case shapeSingle:
				id = "demo-id"
			case shapePair:
				id = "demo-ns/demo-name"
			}
			urn, err := Format(typ, id, "")
			require.NoError(t, err, "Format must accept the shape registeredTypes declares")
			assert.NotEmpty(t, Describe(urn), "every registered type needs a human phrase in Describe")
		})
	}
}
