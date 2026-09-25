package runner_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
)

func TestLabelStore_PutGet(t *testing.T) {
	s := runner.NewLabelStore(0) // 0 → default cap

	got, ok := s.Get("crm_company", "1")
	assert.False(t, ok, "Get on empty store returns ok=false")
	assert.Equal(t, "", got)

	s.Put("crm_company", "1", "Acme Corp")
	got, ok = s.Get("crm_company", "1")
	assert.True(t, ok)
	assert.Equal(t, "Acme Corp", got)

	// Re-Put under same key overwrites.
	s.Put("crm_company", "1", "Renamed Corp")
	got, _ = s.Get("crm_company", "1")
	assert.Equal(t, "Renamed Corp", got)

	// Different resourceType is a different key.
	_, ok = s.Get("hubspot_owner", "1")
	assert.False(t, ok)
}

func TestLabelStore_FIFOEviction(t *testing.T) {
	s := runner.NewLabelStore(3)
	s.Put("crm_company", "1", "A")
	s.Put("crm_company", "2", "B")
	s.Put("crm_company", "3", "C")
	// 4th put — should evict id=1.
	s.Put("crm_company", "4", "D")

	_, ok := s.Get("crm_company", "1")
	assert.False(t, ok, "oldest entry should be evicted")
	got, ok := s.Get("crm_company", "4")
	assert.True(t, ok)
	assert.Equal(t, "D", got)

	// Re-put on an existing key updates the label in place and does
	// NOT touch FIFO position. State after Put(1..4)+evict(1) is
	// seq=[2,3,4]; re-putting id=2 must not move it; the next eviction
	// drops seq[0]=2.
	s.Put("crm_company", "2", "B2")
	got2, ok := s.Get("crm_company", "2")
	require.True(t, ok)
	assert.Equal(t, "B2", got2, "re-put should overwrite the stored label")

	s.Put("crm_company", "5", "E") // evicts seq[0]=2 (re-put did NOT move it)
	_, ok = s.Get("crm_company", "2")
	assert.False(t, ok, "id=2 was oldest after the no-op re-put; should now be evicted")
	_, ok = s.Get("crm_company", "3")
	assert.True(t, ok, "id=3 remains")
	_, ok = s.Get("crm_company", "5")
	assert.True(t, ok)
}

func TestLabelStore_SnapshotForArgs(t *testing.T) {
	s := runner.NewLabelStore(0)
	s.Put("crm_company", "100", "Acme Corp")
	s.Put("crm_company", "200", "Beta LLC")
	s.Put("hubspot_owner", "9", "Owner Nine")
	// Two-type collision setup: id "9" exists under hubspot_owner
	// (above) AND crm_company. The collision case below asserts the
	// primary's resourceType label wins regardless of which type's
	// entry was inserted first.
	s.Put("crm_company", "9", "Nine Corp")

	cases := []struct {
		name        string
		args        map[string]any
		primaryType string
		primaryID   string
		want        map[string]string
	}{
		{
			name:        "primary only, no args matches",
			args:        map[string]any{"limit": float64(50)},
			primaryType: "crm_company", primaryID: "100",
			want: map[string]string{"100": "Acme Corp"},
		},
		{
			name: "args contain a labeled id at depth (nested map + slice)",
			args: map[string]any{
				"filterGroups": []any{
					map[string]any{"associatedWith": []any{
						map[string]any{"objectIdValues": []any{"200"}},
					}},
				},
			},
			primaryType: "crm_company", primaryID: "100",
			want: map[string]string{"100": "Acme Corp", "200": "Beta LLC"},
		},
		{
			name:        "args contain a string that is not a labeled id: ignored",
			args:        map[string]any{"q": "100banana"},
			primaryType: "crm_company", primaryID: "100",
			want: map[string]string{"100": "Acme Corp"},
		},
		{
			name:        "primary not in store + no args matches: empty",
			args:        map[string]any{},
			primaryType: "crm_company", primaryID: "missing",
			want: map[string]string{},
		},
		{
			// Store holds BOTH hubspot_owner:9 ("Owner Nine") and
			// crm_company:9 ("Nine Corp"). With primary=hubspot_owner:9,
			// the primary's label wins for the shared bare id "9".
			// The crm_company:9 entry is dropped from the snapshot
			// (no separate output entry under another key shape).
			name:        "id-collision across types: primary's resourceType wins",
			args:        map[string]any{"refs": []any{"9"}},
			primaryType: "hubspot_owner", primaryID: "9",
			want: map[string]string{"9": "Owner Nine"},
		},
		{
			// Same store, flipped primary. The OTHER type's label must
			// now win — confirms the collision rule isn't accidentally
			// hardcoded to one type.
			name:        "id-collision across types: flipping primary flips the winner",
			args:        map[string]any{"refs": []any{"9"}},
			primaryType: "crm_company", primaryID: "9",
			want: map[string]string{"9": "Nine Corp"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.SnapshotForArgs(tc.args, tc.primaryType, tc.primaryID)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLabelStore_ConcurrentSafe(t *testing.T) {
	s := runner.NewLabelStore(0)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Put("type", string(rune('A'+i)), "label")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = s.Get("type", string(rune('A'+i)))
				_ = s.SnapshotForArgs(map[string]any{"x": string(rune('A' + i))}, "type", "Z")
			}
		}()
	}
	wg.Wait()
}
