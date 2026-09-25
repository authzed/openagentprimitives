package files

import (
	"bytes"
	"context"
	"io"

	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// StoreReader reads artifact bytes (optionally a range) from an
// artifactstore.Store. Implements modality.ArtifactReader.
type StoreReader struct {
	Store artifactstore.Store
}

// ReadRange returns bytes [start, start+length) of ref. length<=0 means to EOF.
func (r StoreReader) ReadRange(ctx context.Context, ref artifactstore.Ref, start, length int64) ([]byte, int64, error) {
	rc, err := r.Store.Get(ctx, ref)
	if err != nil {
		return nil, 0, err
	}
	return sliceReadCloser(rc, start, length)
}

// ArtifactGetter is the minimal ref-string reader the runner's HTTP artifact
// client (pkg/agent/tool/sandbox.ArtifactClient) satisfies structurally. Kept
// local so this package does not import sandbox.
type ArtifactGetter interface {
	Get(ctx context.Context, ref string) (io.ReadCloser, error)
}

// ReaderFromClient adapts a ref-string artifact client to modality.ArtifactReader.
func ReaderFromClient(c ArtifactGetter) modality.ArtifactReader { return clientReader{c: c} }

type clientReader struct{ c ArtifactGetter }

func (r clientReader) ReadRange(ctx context.Context, ref artifactstore.Ref, start, length int64) ([]byte, int64, error) {
	rc, err := r.c.Get(ctx, string(ref))
	if err != nil {
		return nil, 0, err
	}
	return sliceReadCloser(rc, start, length)
}

// sliceReadCloser reads rc fully and returns bytes [start, start+length),
// length<=0 meaning to EOF, plus the count returned. It closes rc.
//
// NOTE: this materializes the whole object then slices — the in-mem/blob store
// has no range Get. When a range-capable store backend lands, push the range
// into Store.Get instead of reading to EOF here. (TODO)
func sliceReadCloser(rc io.ReadCloser, start, length int64) ([]byte, int64, error) {
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil {
		return nil, 0, err
	}
	buf := all
	if start > 0 {
		if start >= int64(len(buf)) {
			return []byte{}, 0, nil
		}
		buf = buf[start:]
	}
	if length > 0 && length < int64(len(buf)) {
		buf = buf[:length]
	}
	out := bytes.Clone(buf)
	return out, int64(len(out)), nil
}
