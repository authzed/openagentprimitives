package meta_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

// The recorded result of a real captured session's artifact_prepare call, with
// its four warnings AND its two tags in the arbitrary orders that run's map
// iterations produced. Values are fixtures; the SHAPE is what matters.
//
// Both collections are here deliberately. Each was found by replaying a real
// capture, one change apart, and a fixture carrying only the first would have
// let the second regress the same way — the canonicalizer is only worth having
// if it covers every unordered collection in the result, not the one that bit
// most recently.
const recordedPrepareResult = `{"handle":"ar-demo-agent-9999-cd752b",` +
	`"artifact_id":"artifact-1111111111111111","revision_id":"artrev-2222222222222222","seq":1,` +
	`"tags":["latest","b0f3d1c2"],"status":"ready","mime":"text/html","size":5291,"filename":"review.html",` +
	`"warnings":[{"kind":"tag","name":"title","action":"removed","count":1},` +
	`{"kind":"tag","name":"meta","action":"unwrapped","count":1,"note":"content kept"},` +
	`{"kind":"attr","name":"lang","action":"stripped","count":1},` +
	`{"kind":"attr","name":"charset","action":"stripped","count":1}]}`

func TestCanonicalizeResult_OnlyReordersCollections(t *testing.T) {
	got, ok := meta.CanonicalizeResult("artifact_prepare", []byte(recordedPrepareResult))
	require.True(t, ok, "artifact_prepare's result is one this package owns")

	var before, after map[string]any
	require.NoError(t, json.Unmarshal([]byte(recordedPrepareResult), &before))
	require.NoError(t, json.Unmarshal(got, &after))

	// Every SCALAR survives untouched. Asserted field by field rather than as
	// one blob, so a failure names what moved.
	for _, k := range []string{"handle", "artifact_id", "revision_id", "seq", "status", "mime", "size", "filename"} {
		assert.Equal(t, before[k], after[k], "canonicalization must not touch %q", k)
	}

	// The tags are the same SET, in ascending name order. "latest" is not
	// privileged — see artifacts.tagsPointingAt for why the order is
	// arbitrary-but-stable rather than semantic.
	assert.Equal(t, []any{"b0f3d1c2", "latest"}, after["tags"],
		"tags must come out in the order artifacts.tagsPointingAt now produces")

	ws, _ := after["warnings"].([]any)
	require.Len(t, ws, 4, "no warning may be dropped")
	names := make([]string, 0, len(ws))
	for _, w := range ws {
		m, _ := w.(map[string]any)
		names = append(names, m["kind"].(string)+"/"+m["name"].(string))
	}
	assert.Equal(t, []string{"attr/charset", "attr/lang", "tag/meta", "tag/title"}, names,
		"kind, then name — the order channelassets.SortWarnings defines")

	// The note rides along with its warning rather than staying at its old
	// index, which is the way a naive sort of a parallel slice would break.
	for _, w := range ws {
		m, _ := w.(map[string]any)
		if m["name"] == "meta" {
			assert.Equal(t, "content kept", m["note"])
		}
	}
}

func TestCanonicalizeResult_IsIdempotent(t *testing.T) {
	once, ok := meta.CanonicalizeResult("artifact_prepare", []byte(recordedPrepareResult))
	require.True(t, ok)
	twice, ok := meta.CanonicalizeResult("artifact_prepare", once)
	require.True(t, ok)
	assert.Equal(t, string(once), string(twice),
		"a result already in canonical form is unchanged, so a fresh capture and an old one agree")
}

func TestCanonicalizeResult_AwaitSharesTheEncoder(t *testing.T) {
	// artifact_await returns the same shape from the same marshaller; if it did
	// not canonicalize too, a session whose prepare returned `pending` and whose
	// await returned the revision would pin an uncanonical order at the step
	// that actually carries the values.
	got, ok := meta.CanonicalizeResult("artifact_await", []byte(recordedPrepareResult))
	require.True(t, ok, "artifact_await returns artifact_prepare's result type")

	want, _ := meta.CanonicalizeResult("artifact_prepare", []byte(recordedPrepareResult))
	assert.Equal(t, string(want), string(got))
}

func TestCanonicalizeResult_UnknownToolIsUntouched(t *testing.T) {
	in := []byte(`{"anything":"at all"}`)
	got, ok := meta.CanonicalizeResult("respond_to_user", in)
	assert.False(t, ok, "a tool this package does not own is not canonicalized")
	assert.Equal(t, string(in), string(got), "and its payload is returned unchanged")
}

func TestCanonicalizeResult_RefusesRatherThanDropping(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{
			// The load-bearing refusal: re-encoding a payload carrying a field
			// the result type does not know would DROP it, and the canonical
			// form would then be a weaker claim than the recording — silently.
			name: "a field the result type does not know",
			in:   `{"handle":"ar-x-aaaaaa","status":"ready","future_field":42}`,
		},
		{
			name: "a refusal message, which is not a result at all",
			in:   `artifact_prepare: kind "pdf" is not available on this channel.`,
		},
		{name: "empty", in: ``},
		{name: "a JSON array", in: `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := meta.CanonicalizeResult("artifact_prepare", []byte(tc.in))
			assert.False(t, ok, "an undecodable or lossy payload is refused")
			assert.Equal(t, tc.in, string(got), "and is handed back byte-identical")
		})
	}
}

func TestCanonicalizeResult_NoWarningsIsUnchanged(t *testing.T) {
	// The pending reply, which every prepare that outran its render returns.
	const pending = `{"handle":"ar-x-aaaaaa","artifact_id":"artifact-1111111111111111",` +
		`"status":"pending","message":"Render still running."}`
	got, ok := meta.CanonicalizeResult("artifact_prepare", []byte(pending))
	require.True(t, ok)
	assert.JSONEq(t, pending, string(got))
}
