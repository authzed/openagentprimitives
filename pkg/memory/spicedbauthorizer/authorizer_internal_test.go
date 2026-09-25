package spicedbauthorizer

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/status"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func testEntry(id string) memory.Entry {
	return memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/sess"},
		Kind:  "fact",
		ID:    id,
	}
}

// pairFor builds the bulk-check pair SpiceDB returns for one entry, echoing
// the request item the way the real service does.
func pairFor(e memory.Entry, permitted bool) *v1.CheckBulkPermissionsPair {
	ship := v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
	if permitted {
		ship = v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}
	return &v1.CheckBulkPermissionsPair{
		Request: &v1.CheckBulkPermissionsRequestItem{
			Resource:   &v1.ObjectReference{ObjectType: "memory_entry", ObjectId: EntryResourceID(e)},
			Permission: "read",
		},
		Response: &v1.CheckBulkPermissionsPair_Item{
			Item: &v1.CheckBulkPermissionsResponseItem{Permissionship: ship},
		},
	}
}

func errPairFor(e memory.Entry) *v1.CheckBulkPermissionsPair {
	return &v1.CheckBulkPermissionsPair{
		Request: &v1.CheckBulkPermissionsRequestItem{
			Resource:   &v1.ObjectReference{ObjectType: "memory_entry", ObjectId: EntryResourceID(e)},
			Permission: "read",
		},
		Response: &v1.CheckBulkPermissionsPair_Error{
			Error: &status.Status{Code: 3, Message: "bad resource id"},
		},
	}
}

func entryIDs(t *testing.T, entries []memory.Entry) []string {
	t.Helper()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// The bulk-check response is matched to entries by the resource ID echoed on
// the pair, never by slice position: SpiceDB promises neither request order
// nor a same-length response, and every mismatch is an authorization failure
// (a panic, or the wrong entry handed to a caller with no read permission).
func TestFilterAllowed(t *testing.T) {
	a, b, c := testEntry("f-a"), testEntry("f-b"), testEntry("f-c")
	entries := []memory.Entry{a, b, c}

	cases := []struct {
		name  string
		pairs []*v1.CheckBulkPermissionsPair
		want  []string
	}{
		{
			name:  "request-ordered response: the permitted entries are returned",
			pairs: []*v1.CheckBulkPermissionsPair{pairFor(a, true), pairFor(b, false), pairFor(c, true)},
			want:  []string{"f-a", "f-c"},
		},
		{
			name:  "reordered response: permission still follows the resource, not the position",
			pairs: []*v1.CheckBulkPermissionsPair{pairFor(c, false), pairFor(a, true), pairFor(b, false)},
			want:  []string{"f-a"},
		},
		{
			name:  "short response: entries with no pair are denied, not admitted",
			pairs: []*v1.CheckBulkPermissionsPair{pairFor(b, true)},
			want:  []string{"f-b"},
		},
		{
			name: "longer response: extra pairs are ignored rather than panicking",
			pairs: []*v1.CheckBulkPermissionsPair{
				pairFor(a, true), pairFor(b, true), pairFor(c, true),
				pairFor(testEntry("f-ghost"), true),
			},
			want: []string{"f-a", "f-b", "f-c"},
		},
		{
			name:  "pair-level error: the entry is denied",
			pairs: []*v1.CheckBulkPermissionsPair{errPairFor(a), pairFor(b, true), pairFor(c, false)},
			want:  []string{"f-b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterAllowed(context.Background(), entries, tc.pairs)
			assert.Equal(t, tc.want, entryIDs(t, got))
		})
	}
}

// A caller with no read permission on any entry must get nothing back even
// when the response is mangled.
func TestFilterAllowed_NoPermittedPairAdmitsNothing(t *testing.T) {
	a, b := testEntry("f-a"), testEntry("f-b")
	got := filterAllowed(context.Background(), []memory.Entry{a, b},
		[]*v1.CheckBulkPermissionsPair{pairFor(b, false), pairFor(a, false)})
	require.Empty(t, got)
}
