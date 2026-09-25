package all_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// TestIndexedFields_AreContentKeys_NotGoFieldNames asserts the invariant over
// every registered Kind at once, which is the only place it can be asserted:
// a per-Kind test can only repeat whatever the declaration says.
//
// The names in IndexedFields() are what an accessor author reads before
// writing a FieldFilter, and a FieldFilter's Path is resolved against the
// SERIALIZED entry (content->>'<path>'). Twelve Kinds once declared Go field
// names here, and two accessors copied them into queries that extracted NULL
// on postgres and sqlite and so matched nothing — invisibly, because inmem
// dropped the predicate. Declaring a name no entry is stored under is
// therefore not a cosmetic slip; it is a query that can never be true.
func TestIndexedFields_AreContentKeys_NotGoFieldNames(t *testing.T) {
	kinds := memory.RegisteredKinds()
	require.NotEmpty(t, kinds, "kinds/all must register the Kinds")

	for _, k := range kinds {
		fields := k.IndexedFields()
		if len(fields) == 0 {
			continue
		}
		t.Run(k.Name(), func(t *testing.T) {
			keys := memory.ContentKeys(k.ContentSchema())
			require.NotNil(t, keys,
				"IndexedFields is only meaningful with a ContentSchema to name keys in")
			for _, f := range fields {
				assert.Contains(t, keys, f,
					"IndexedFields entry %q is not a key %s entries are stored under; "+
						"use the `json` tag, not the Go field name", f, k.Name())
			}
		})
	}
}
