package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// TestCacheNamespaces covers the --watch-namespaces → cache.DefaultNamespaces
// folding: empty stays cluster-wide (nil), a non-empty set is scoped and always
// includes the operator's own namespace so fixed-infra Secrets/ConfigMaps stay
// cached, and blank entries are trimmed away.
func TestCacheNamespaces(t *testing.T) {
	cases := []struct {
		name     string
		watch    []string
		systemNS string
		want     []string // expected map keys; nil means a nil map (cluster-wide)
	}{
		{
			name:     "empty watch: nil map (cluster-wide)",
			watch:    nil,
			systemNS: "agentprimitives-system",
			want:     nil,
		},
		{
			name:     "scoped watch folds in the operator's own namespace",
			watch:    []string{"tenant-a", "tenant-b"},
			systemNS: "agentprimitives-system",
			want:     []string{"tenant-a", "tenant-b", "agentprimitives-system"},
		},
		{
			name:     "system namespace already listed is not duplicated",
			watch:    []string{"tenant-a", "agentprimitives-system"},
			systemNS: "agentprimitives-system",
			want:     []string{"tenant-a", "agentprimitives-system"},
		},
		{
			name:     "blank and whitespace-only entries are dropped",
			watch:    []string{" tenant-a ", "", "   "},
			systemNS: "agentprimitives-system",
			want:     []string{"tenant-a", "agentprimitives-system"},
		},
		{
			name:     "empty system namespace is not added",
			watch:    []string{"tenant-a"},
			systemNS: "",
			want:     []string{"tenant-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheNamespaces(tc.watch, tc.systemNS)
			if tc.want == nil {
				assert.Nil(t, got, "empty watch must yield a nil map (cluster-wide cache)")
				return
			}
			keys := make([]string, 0, len(got))
			for k, v := range got {
				keys = append(keys, k)
				assert.Equal(t, cache.Config{}, v, "namespace %q must map to a zero cache.Config", k)
			}
			assert.ElementsMatch(t, tc.want, keys, "scoped namespace set")
		})
	}
}
