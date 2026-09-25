package skillbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// TarGz builds a deterministic .tar.gz from files (path → content) and returns
// the archive bytes plus their sha256 hex digest. Determinism: paths are sorted
// and tar headers carry fixed mode/zero mtime so the same input always yields
// identical bytes (so identical bundles share a digest).
func TarGz(files map[string][]byte) (archive []byte, digest string, err error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		content := files[p]
		hdr := &tar.Header{Name: p, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	out := buf.Bytes()
	sum := sha256.Sum256(out)
	return out, hex.EncodeToString(sum[:]), nil
}
