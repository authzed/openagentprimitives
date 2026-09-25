package skillfetch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFakeFetcher(t *testing.T) {
	f := &Fake{
		Result: Result{
			SHA:   "abc123",
			Files: map[string][]byte{"skills/x/SKILL.md": []byte("---\nname: x\n---\nbody")},
		},
	}
	got, err := f.Fetch(context.Background(), Request{RepoURL: "github.com/o/r", Ref: "main"})
	require.NoError(t, err)
	assert.Equal(t, "abc123", got.SHA)
	assert.Contains(t, got.Files, "skills/x/SKILL.md")
	assert.Equal(t, Request{RepoURL: "github.com/o/r", Ref: "main"}, f.LastRequest)
}
