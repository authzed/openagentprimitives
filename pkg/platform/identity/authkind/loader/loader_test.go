package loader_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/registry"
)

func TestAllKindsRegistered(t *testing.T) {
	prefixes := []string{}
	for _, k := range registry.All() {
		prefixes = append(prefixes, k.Prefix())
	}
	sort.Strings(prefixes)
	assert.Equal(t, []string{"cli", "mcp", "toolbox", "toolspec"}, prefixes, "registered prefixes")
}
