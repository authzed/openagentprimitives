package permsurface

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzParseHandle asserts the parser is total and canonical: it never panics,
// and any input it ACCEPTS must re-format to a string that parses back to an
// equal Handle. That forbids two distinct spellings of one handle, which is
// how an overload would smuggle itself past an approved ceiling.
func FuzzParseHandle(f *testing.F) {
	for _, seed := range []string{
		"", "perm:", "tool:", "perm:write:github_repo", "tool:apply_workspace",
		"perm:write:github:repo", "cap:x:y", "perm:write:acme/tracker_issue",
		"tool:perm", "perm:\x00:y", "tool:аpply", "perm:a:b\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := ParseHandle(s)
		if err != nil {
			assert.True(t, h.IsZero(), "rejected input must yield the zero Handle")
			return
		}
		require.Equal(t, s, h.String(), "an accepted handle must be its own canonical form")

		again, err := ParseHandle(h.String())
		require.NoError(t, err, "re-parsing an accepted handle must succeed")
		assert.Equal(t, h, again, "re-parsing must be idempotent")
	})
}

// FuzzHandleInjectivity is THE anti-overloading property: distinct component
// inputs must never collapse onto the same Handle, and a tool handle can never
// equal a perm handle no matter how the tool is named.
func FuzzHandleInjectivity(f *testing.F) {
	f.Add("write", "github_repo", "apply_workspace")
	f.Add("read", "acme/tracker_issue", "perm")
	f.Add("a", "b", "perm:a:b")
	f.Fuzz(func(t *testing.T, permission, resourceType, toolName string) {
		ph, perr := NewPermHandle(permission, resourceType)
		th, terr := NewToolHandle(toolName)

		if perr == nil && terr == nil {
			assert.NotEqual(t, ph, th, "a tool handle must never equal a perm handle")
		}
		if perr != nil {
			assert.True(t, ph.IsZero())
		}
		if terr != nil {
			assert.True(t, th.IsZero())
		}

		// Injectivity on the perm side: swapping the components must not
		// produce the same handle unless the components were identical.
		if perr == nil {
			if swapped, err := NewPermHandle(resourceType, permission); err == nil && permission != resourceType {
				assert.NotEqual(t, ph, swapped, "component order must be significant")
			}
		}
	})
}
