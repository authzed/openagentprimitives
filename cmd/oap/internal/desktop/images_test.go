package desktop_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

func TestImportArgv(t *testing.T) {
	got := desktop.ImportArgv("/var/lib/rancher/k3s/agent/images/app.tar.zst")
	assert.Equal(t, []string{"k3s", "ctr", "images", "import", "/var/lib/rancher/k3s/agent/images/app.tar.zst"}, got)
}

func TestFullProfileImages(t *testing.T) {
	imgs := desktop.FullProfileImages()
	assert.Contains(t, imgs, "pgvector/pgvector:pg17")
	assert.NotEmpty(t, imgs)
}
