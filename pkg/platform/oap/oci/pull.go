package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"oras.land/oras-go/v2"
	ocilayout "oras.land/oras-go/v2/content/oci"
)

// Pull copies the manifest graph at ref from the remote registry into a
// temporary OCI-layout directory store, then re-serializes that layout into a
// .oap tar (oci-layout + index.json + blobs/sha256/<hex>) — the shape
// oap.Unpack and oap.Digest expect.
//
// The returned bytes need NOT be byte-identical to whatever was Push'd: the
// outer tar packaging is not itself content-addressed, only the OCI
// manifest/blobs inside it. The invariant to check is that oap.Digest(pulled)
// equals the returned digest (and the digest Push returned for the same ref),
// and that oap.Unpack(pulled) deep-equals oap.Unpack of the locally packed bytes.
func Pull(ctx context.Context, ref string, opts Options) ([]byte, string, error) {
	repo, err := repository(ref, opts)
	if err != nil {
		return nil, "", err
	}

	dir, err := os.MkdirTemp("", "oap-pull-*")
	if err != nil {
		return nil, "", fmt.Errorf("pull %s: create temp layout dir: %w", ref, err)
	}
	defer os.RemoveAll(dir)

	dst, err := ocilayout.New(dir)
	if err != nil {
		return nil, "", fmt.Errorf("pull %s: init oci-layout store: %w", ref, err)
	}

	srcRef := repo.Reference.ReferenceOrDefault()
	desc, err := oras.Copy(ctx, repo, srcRef, dst, "", oras.DefaultCopyOptions)
	if err != nil {
		return nil, "", fmt.Errorf("pull %s: copy from registry: %w", ref, err)
	}

	oapBytes, err := tarDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("pull %s: re-tar oci-layout: %w", ref, err)
	}
	return oapBytes, desc.Digest.String(), nil
}

// tarDir tars the OCI-layout directory tree at root (oci-layout, index.json,
// blobs/sha256/<hex>) into an in-memory .oap-shaped tar. It is the inverse of
// content/oci's directory-store expectations: Push reads a .oap tar as a
// directory via ocilayout.NewFromTar; Pull writes a directory via
// ocilayout.New and needs it back as a tar, which this does.
func tarDir(root string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("rel path for %s: %w", p, err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		hdr := &tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %s: %w", rel, err)
		}
		_, err = tw.Write(data)
		return err
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	return buf.Bytes(), nil
}
