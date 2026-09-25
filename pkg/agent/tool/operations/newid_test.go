package operations_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
)

// productionID is the shape generateID emits: "op-" plus 8 random bytes,
// hex-encoded. Asserted rather than assumed because the whole point of the
// newID seam is that a nil minter leaves this untouched.
var productionID = regexp.MustCompile(`^op-[0-9a-f]{16}$`)

// A nil newID must leave production behaviour EXACTLY as it was: the seam
// exists for the replay harness, and a registry built the way every binary
// builds it must not be able to tell the seam was added.
func TestNew_NilNewIDKeepsProductionMinting(t *testing.T) {
	r := operations.New(nil, nil)

	first := r.Begin("read the repo").ID
	second := r.Begin("read the repo").ID
	root := r.Root().ID

	for name, id := range map[string]string{"first Begin": first, "second Begin": second, "Root": root} {
		assert.Regexp(t, productionID, id, "%s must mint the production id shape", name)
	}
	assert.NotEqual(t, first, second, "two operations must not share an id")
	assert.NotEqual(t, first, root, "the root is its own operation")
}

// The seam itself: a registry given a minter hands out exactly what the minter
// returns, in call order, and the operation is addressable by that id.
func TestNew_BeginDrawsFromNewID(t *testing.T) {
	var i int
	r := operations.New(nil, func() string { i++; return fmt.Sprintf("op-%016x", i) })

	first := r.Begin("first")
	second := r.Begin("second")

	assert.Equal(t, "op-0000000000000001", first.ID)
	assert.Equal(t, "op-0000000000000002", second.ID)

	got, ok := r.Get("op-0000000000000002")
	require.True(t, ok, "an injected id must address its operation like any other")
	assert.Equal(t, "second", got.Description)
}

// Root is deliberately NOT drawn from the sequence.
//
// The root id is never returned to the model — resolveAmbientOperation mints it
// internally and nothing puts it in a tool result — so a capture cannot observe
// it and cannot record it. Drawing it from the sequence would consume an entry
// the capture never wrote, shifting every later id by one, and it would do so
// on the first unattributed call, whose position in the run depends on which
// tools the model happened to reach for.
func TestNew_RootDoesNotDrawFromNewID(t *testing.T) {
	var calls int
	r := operations.New(nil, func() string { calls++; return fmt.Sprintf("op-%016x", calls) })

	root := r.Root()
	assert.Regexp(t, productionID, root.ID, "the root mints its own id")
	assert.Zero(t, calls, "and must not consume an entry the capture could never have recorded")

	assert.Equal(t, "op-0000000000000001", r.Begin("first").ID,
		"so the first Begin still gets the sequence's first id")
}
