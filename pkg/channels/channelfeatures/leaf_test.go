package channelfeatures_test

import (
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPackageStaysALeaf asserts pkg/channels/channelfeatures directly imports exactly
// pkg/agent/agentcaps and pkg/apis/v1alpha1 among agentprimitives packages, no more
// and no less. It is imported by BOTH pkg/channels/channelkinds (which cannot import
// the capability registry) and pkg/agent/tool/meta/capability; any additional
// agentprimitives import risks re-creating the cycle between them.
//
// Checked via `go list -f '{{.Imports}}'` rather than `-deps`: `.Imports`
// reports only this package's own direct imports, so it neither includes the
// package itself nor expands into everything pkg/apis/v1alpha1 imports.
// TestAllowedTransitiveDepsExcludeTheCycleRiskPackages checks that graph.
func TestPackageStaysALeaf(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`,
		"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures").Output()
	require.NoError(t, err, "go list -f must succeed")

	var got []string
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(dep, "github.com/authzed/openagentprimitives/") {
			got = append(got, dep)
		}
	}
	sort.Strings(got)

	want := []string{
		"github.com/authzed/openagentprimitives/pkg/agent/agentcaps",
		"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1",
	}
	assert.Equal(t, want, got,
		"pkg/channels/channelfeatures must directly import exactly pkg/agent/agentcaps and pkg/apis/v1alpha1 among "+
			"agentprimitives packages; an unexpected addition and a silently-dropped permitted import both fail here")
}

// A different property than TestPackageStaysALeaf: the two packages
// channelfeatures may import must not themselves reach
// pkg/channels/channelkinds or pkg/agent/tool/meta/capability — the two this
// leaf exists to keep apart. If either permitted import's graph grew to include
// one of them, the cycle would return one layer down with channelfeatures' own
// direct imports unchanged, and TestPackageStaysALeaf could not notice.
func TestAllowedTransitiveDepsExcludeTheCycleRiskPackages(t *testing.T) {
	closure := map[string]bool{}
	for _, pkg := range []string{
		"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1",
		"github.com/authzed/openagentprimitives/pkg/agent/agentcaps",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		require.NoError(t, err, "go list -deps %s must succeed", pkg)
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			closure[dep] = true
		}
	}
	assert.False(t, closure["github.com/authzed/openagentprimitives/pkg/channels/channelkinds"],
		"pkg/apis/v1alpha1 or pkg/agent/agentcaps now transitively imports pkg/channels/channelkinds; "+
			"that would recreate the cycle pkg/channels/channelfeatures exists to break")
	assert.False(t, closure["github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"],
		"pkg/apis/v1alpha1 or pkg/agent/agentcaps now transitively imports the capability registry; "+
			"that would recreate the cycle pkg/channels/channelfeatures exists to break")
}
