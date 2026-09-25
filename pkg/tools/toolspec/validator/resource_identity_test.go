package validator

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// resolveResourceID reuses the toolkit's own resourceIDExpr + transforms
// (github_repo_id, normalize_url) but drops spicedb_escape, so call.resourceId
// is the canonical HUMAN form — a host-pinned, ascii-lowercased github URL — an
// allowlist can be written against, not a SpiceDB-escaped key. The value differs
// from the authz SpiceDB key only by that escaping (asserted below).
func TestResolveResourceIDForGitClone(t *testing.T) {
	tk := loadGitToolkit(t)
	call, err := (&parser.Declarative{}).Parse(tk, []string{"clone", "https://github.com/Demo-Org/Demo-Repo"})
	require.NoError(t, err)

	id := resolveResourceID(tk, call)
	assert.Equal(t, "https://github.com/demo-org/demo-repo", id,
		"clean, ascii-lowercased, host-pinned canonical URL (no spicedb escaping)")

	// It differs from the authz SpiceDB key ONLY by the escaping step — same
	// repository, same canonicalization otherwise.
	check, ok := checkForCall(tk, call)
	require.True(t, ok)
	escaped, err := authz.ResolveResourceID(check, namedArgs(call))
	require.NoError(t, err)
	assert.NotEqual(t, escaped, id, "the CEL value is unescaped; the SpiceDB key is escaped")
	assert.Contains(t, escaped, "=3A", "sanity: the SpiceDB key is in fact escaped")
}

// A local/workspace op keys on the "workspace" sentinel template — it names no
// argument-derived external instance — so it must resolve to "" and stay
// ungated by a resourceId-based allowlist.
func TestResolveResourceIDEmptyForLocalOp(t *testing.T) {
	tk := loadGitToolkit(t)
	call, err := (&parser.Declarative{}).Parse(tk, []string{"status"})
	require.NoError(t, err)

	assert.Equal(t, "", resolveResourceID(tk, call),
		"a workspace-sentinel op resolves to no external instance")
}

func loadGitToolkit(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	tk, err := toolkit.Load(filepath.Join("..", "..", "..", "..", "toolkits", "git.yaml"))
	require.NoError(t, err, "load git toolkit")
	return tk
}
