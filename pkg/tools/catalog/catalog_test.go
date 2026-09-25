package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolkitNames builds a set of toolkit names from a catalog for
// presence assertions.
func toolkitNames(t *testing.T, c *Catalog) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, tk := range c.Toolkits {
		names[tk.Name] = true
	}
	return names
}

func TestLoadDir_Happy(t *testing.T) {
	c, err := LoadDir("testdata/valid")
	require.NoError(t, err, "LoadDir")
	require.Len(t, c.Toolkits, 2, "len(Toolkits)")
	names := toolkitNames(t, c)
	assert.True(t, names["gh"], "missing 'gh'; got %v", names)
	assert.True(t, names["kubectl"], "missing 'kubectl'; got %v", names)
}

// TestLoadBuiltin_HasGhAndGit covers the embedded built-in catalog —
// the gen-agent depends on this set always being non-empty.
func TestLoadBuiltin_HasGhAndGit(t *testing.T) {
	c := LoadBuiltin()
	require.NotEmpty(t, c.Toolkits, "built-in catalog should not be empty")
	names := toolkitNames(t, c)
	for _, want := range []string{"gh", "git"} {
		assert.True(t, names[want], "built-in catalog missing %q; got %v", want, names)
	}
	// FullYAML must work for built-ins.
	data, err := c.FullYAML("gh")
	require.NoError(t, err, "FullYAML(\"gh\")")
	assert.NotEmpty(t, data, "FullYAML(\"gh\") returned empty bytes")
}

// TestLoadBuiltinPlusDir_NoOverlay returns the built-in set when the
// overlay dir is empty.
func TestLoadBuiltinPlusDir_NoOverlay(t *testing.T) {
	c, err := LoadBuiltinPlusDir("")
	require.NoError(t, err, "LoadBuiltinPlusDir(\"\")")
	assert.GreaterOrEqual(t, len(c.Toolkits), 2, "expected built-in toolkits")
}

// TestLoadBuiltinPlusDir_OverlayMerges loads the built-in set, then
// overlays testdata/valid (which has gh + kubectl). gh is in both —
// the disk version should win. kubectl gets added on top.
func TestLoadBuiltinPlusDir_OverlayMerges(t *testing.T) {
	c, err := LoadBuiltinPlusDir("testdata/valid")
	require.NoError(t, err, "LoadBuiltinPlusDir")
	names := toolkitNames(t, c)
	for _, want := range []string{"gh", "git", "kubectl"} {
		assert.True(t, names[want], "missing %q after overlay; got %v", want, names)
	}
	// No duplicate gh entries — overlay replaced built-in.
	count := 0
	for _, tk := range c.Toolkits {
		if tk.Name == "gh" {
			count++
		}
	}
	assert.Equal(t, 1, count, "gh appears %d times in merged catalog; want 1", count)
}

// TestLoadBuiltinPlusDir_MissingDir is non-fatal when the dir doesn't
// exist — built-ins still work.
func TestLoadBuiltinPlusDir_MissingDir(t *testing.T) {
	c, err := LoadBuiltinPlusDir("testdata/does-not-exist")
	require.NoError(t, err, "missing overlay dir should be non-fatal")
	assert.NotEmpty(t, c.Toolkits, "built-in toolkits should still load")
}

func TestLoadDir_MissingDir(t *testing.T) {
	_, err := LoadDir("testdata/does-not-exist")
	require.Error(t, err, "expected error for missing dir")
}

func TestLoadDir_EmptyDir(t *testing.T) {
	dir := t.TempDir() // empty
	c, err := LoadDir(dir)
	require.NoError(t, err, "LoadDir")
	assert.Empty(t, c.Toolkits, "expected empty catalog")
}

func TestLoadDir_MalformedFileSkipped(t *testing.T) {
	c, err := LoadDir("testdata/bad")
	require.NoError(t, err, "LoadDir returned error (should have skipped)")
	assert.Empty(t, c.Toolkits, "expected zero loaded from bad dir")
	assert.NotEmpty(t, c.Warnings, "expected at least one warning for malformed file")
}

func TestFullYAML(t *testing.T) {
	c, err := LoadDir("testdata/valid")
	require.NoError(t, err, "LoadDir")
	y, err := c.FullYAML("gh")
	require.NoError(t, err, "FullYAML")
	assert.Contains(t, string(y), "pr, merge", "FullYAML for gh missing 'pr, merge'")

	_, err = c.FullYAML("does-not-exist")
	assert.Error(t, err, "expected error for unknown toolkit")
}

func TestSummarize(t *testing.T) {
	c, err := LoadDir("testdata/valid")
	require.NoError(t, err, "LoadDir")
	sums := c.Summarize()
	require.Len(t, sums, 2, "len(Summarize)")
	var gh *ToolkitSummary
	for i := range sums {
		if sums[i].Name == "gh" {
			gh = &sums[i]
			break
		}
	}
	require.NotNil(t, gh, "no gh summary")
	assert.Equal(t, "gh", gh.Binary, "Binary")
	require.Len(t, gh.Subcommands, 2, "len(Subcommands)")
	prMerge := gh.Subcommands[0]
	// Find pr merge specifically
	for _, sc := range gh.Subcommands {
		if len(sc.Path) == 2 && sc.Path[0] == "pr" && sc.Path[1] == "merge" {
			prMerge = sc
		}
	}
	assert.True(t, prMerge.Destructive, "pr merge should be Destructive")
	assert.Equal(t, []string{"network"}, prMerge.Writes, "pr merge Writes")
}
