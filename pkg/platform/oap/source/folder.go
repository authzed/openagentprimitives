package source

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// OpenFolder returns a Source backed by an on-disk folder (see oap.FromFolder).
func OpenFolder(dir string) Source { return &folderSource{dir: dir} }

type folderSource struct{ dir string }

func (s *folderSource) Bundle(ctx context.Context) (*oap.Bundle, error) {
	return oap.FromFolder(s.dir)
}
