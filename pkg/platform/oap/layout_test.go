package oap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/oaptest"
)

// addOuterEntry preserves every existing OCI blob and inserts one extra outer
// tar header. Link entries must be written directly, rather than via tarFiles.
func addOuterEntry(t *testing.T, packed []byte, header tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	require.NoError(t, tw.WriteHeader(&header))
	tr := tar.NewReader(bytes.NewReader(packed))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.NoError(t, tw.WriteHeader(hdr))
		_, err = io.Copy(tw, tr)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return out.Bytes()
}

func TestUnpackRejectsUnsafeOuterEntriesAtEveryNode(t *testing.T) {
	cases := []struct {
		name   string
		header tar.Header
		want   string
	}{
		{"traversal", tar.Header{Name: "../escape", Typeflag: tar.TypeReg}, "unsafe path"},
		{"absolute", tar.Header{Name: "/escape", Typeflag: tar.TypeReg}, "unsafe path"},
		{"symlink", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "index.json"}, "non-regular"},
		{"hardlink", tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "index.json"}, "non-regular"},
		{"traversal directory", tar.Header{Name: "../escape/", Typeflag: tar.TypeDir}, "unsafe path"},
		{"absolute directory", tar.Header{Name: "/escape/", Typeflag: tar.TypeDir}, "unsafe path"},
	}
	for _, node := range []string{"root", "embedded"} {
		for _, tc := range cases {
			t.Run(node+"/"+tc.name, func(t *testing.T) {
				packed := embeddedArchiveFixture(t)
				var bad []byte
				if node == "root" {
					bad = addOuterEntry(t, packed, tc.header)
				} else {
					bad = repackOuter(t, packed, func(files map[string][]byte) {
						man := readManifest(t, files)
						layer := man.Layers[len(man.Layers)-1]
						raw := addOuterEntry(t, files[blobKey(layer.Digest)], tc.header)
						delete(files, blobKey(layer.Digest))
						layer.Digest = digest.FromBytes(raw)
						layer.Size = int64(len(raw))
						files[blobKey(layer.Digest)] = raw
						// Rehash every enclosing descriptor so only the child's
						// outer-entry policy, not a broken digest, causes rejection.
						mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents[0].Digest = layer.Digest.String() })
						mutateManifest(t, files, func(man *ocispec.Manifest) { man.Layers[len(man.Layers)-1] = layer })
					})
				}
				_, err := Unpack(bad)
				require.ErrorContains(t, err, tc.want)
				if node == "embedded" {
					assert.ErrorContains(t, err, "test-coordinator > reviewer")
				}
			})
		}
	}
}

func TestUnpackAcceptsSafeOuterDirectory(t *testing.T) {
	packed := addOuterEntry(t, embeddedArchiveFixture(t), tar.Header{Name: "blobs/sha256/", Typeflag: tar.TypeDir})
	b, err := Unpack(packed)
	require.NoError(t, err)
	require.Len(t, b.Dependencies, 1)
	assert.Equal(t, "reviewer", b.Dependencies[0].Bundle.Manifest.Agent.Name)
}

// mutateConfig preserves the OCI digest closure so malformed dependency
// declarations exercise matching rather than failing the config hash check.
func mutateConfig(t *testing.T, files map[string][]byte, fn func(*Manifest)) {
	t.Helper()
	mutateManifest(t, files, func(man *ocispec.Manifest) {
		var cfg Manifest
		require.NoError(t, json.Unmarshal(files[blobKey(man.Config.Digest)], &cfg))
		fn(&cfg)
		raw, err := json.Marshal(cfg)
		require.NoError(t, err)
		delete(files, blobKey(man.Config.Digest))
		man.Config.Digest = digest.FromBytes(raw)
		man.Config.Size = int64(len(raw))
		files[blobKey(man.Config.Digest)] = raw
	})
}

// embeddedArchiveFixture constructs the new wire format independently of
// recursive Pack, so Unpack rejection tests do not depend on its implementation.
func embeddedArchiveFixture(t *testing.T) []byte {
	t.Helper()
	b := dependencyBundle(t, "test-coordinator", dependencyBundle(t, "reviewer"))
	child, err := Pack(b.Dependencies[0].Bundle)
	require.NoError(t, err)
	b.Dependencies = nil
	b.Manifest.Requires.Agents = nil
	parent, err := Pack(b)
	require.NoError(t, err)
	return repackOuter(t, parent, func(files map[string][]byte) {
		d := digest.FromBytes(child)
		files[blobKey(d)] = child
		mutateConfig(t, files, func(cfg *Manifest) {
			cfg.Requires.Agents = []RequiredAgent{{Name: "reviewer", Version: "1.0.0", MediaType: DependencyMediaType, Digest: d.String()}}
		})
		mutateManifest(t, files, func(man *ocispec.Manifest) {
			man.Layers = append(man.Layers, ocispec.Descriptor{MediaType: DependencyMediaType, Digest: d, Size: int64(len(child)), Annotations: map[string]string{ocispec.AnnotationTitle: "agents/reviewer.oap"}})
		})
	})
}

func TestPackUnpackEmbeddedAgentsDeterministically(t *testing.T) {
	b := dependencyBundle(t, "test-coordinator", dependencyBundle(t, "reviewer"))
	original, err := json.Marshal(b.Manifest)
	require.NoError(t, err)
	first, err := Pack(b)
	require.NoError(t, err)
	second, err := Pack(b)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	firstDigest, err := Digest(first)
	require.NoError(t, err)
	secondDigest, err := Digest(second)
	require.NoError(t, err)
	assert.Equal(t, firstDigest, secondDigest)
	after, err := json.Marshal(b.Manifest)
	require.NoError(t, err)
	assert.Equal(t, original, after, "packing must not mutate authored manifest")

	got, err := Unpack(first)
	require.NoError(t, err)
	require.Len(t, got.Dependencies, 1)
	assert.Empty(t, got.Manifest.Requires.Agents[0].Path)
	assert.Equal(t, "reviewer", got.Manifest.Requires.Agents[0].Name)
	assert.Equal(t, DependencyPath{"reviewer"}, got.Dependencies[0].Path)
	assert.Equal(t, digest.FromBytes(got.Dependencies[0].Packed).String(), got.Manifest.Requires.Agents[0].Digest)
	require.NoError(t, got.Dependencies[0].Bundle.Validate())
	require.NoError(t, ValidateDependencyGraph(got))
	child, err := Unpack(got.Dependencies[0].Packed)
	require.NoError(t, err)
	assert.Equal(t, got.Dependencies[0].Bundle, child)
}

func TestUnpackRejectsMalformedDependency(t *testing.T) {
	packed := embeddedArchiveFixture(t)
	cases := []struct {
		name   string
		mutate func(map[string][]byte)
		want   string
	}{
		{"tampered blob", func(files map[string][]byte) {
			man := readManifest(t, files)
			files[blobKey(man.Layers[len(man.Layers)-1].Digest)][0] ^= 1
		}, "digest mismatch"},
		{"descriptor digest", func(files map[string][]byte) {
			mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents[0].Digest = digest.FromString("wrong").String() })
		}, "digest"},
		{"missing layer", func(files map[string][]byte) {
			mutateManifest(t, files, func(man *ocispec.Manifest) { man.Layers = man.Layers[:len(man.Layers)-1] })
		}, "missing dependency layer"},
		{"extra layer", func(files map[string][]byte) {
			mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents = nil })
		}, "undeclared dependency layer"},
		{"wrong media type", func(files map[string][]byte) {
			mutateManifest(t, files, func(man *ocispec.Manifest) { man.Layers[len(man.Layers)-1].MediaType = "application/x-wrong" })
		}, "unknown layer media type"},
		{"duplicate descriptor", func(files map[string][]byte) {
			mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents = append(cfg.Requires.Agents, cfg.Requires.Agents[0]) })
		}, "duplicate dependency descriptor"},
		{"duplicate layer", func(files map[string][]byte) {
			mutateManifest(t, files, func(man *ocispec.Manifest) { man.Layers = append(man.Layers, man.Layers[len(man.Layers)-1]) })
		}, "duplicate dependency layer"},
		{"wrong version", func(files map[string][]byte) {
			mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents[0].Version = "9.0.0" })
		}, "version"},
		{"wrong name", func(files map[string][]byte) {
			mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents[0].Name = "other" })
			mutateManifest(t, files, func(man *ocispec.Manifest) {
				man.Layers[len(man.Layers)-1].Annotations[ocispec.AnnotationTitle] = "agents/other.oap"
			})
		}, "descriptor name"},
		{"wrong layer size", func(files map[string][]byte) {
			mutateManifest(t, files, func(man *ocispec.Manifest) { man.Layers[len(man.Layers)-1].Size++ })
		}, "size mismatch"},
		{"wrong title", func(files map[string][]byte) {
			mutateManifest(t, files, func(man *ocispec.Manifest) {
				man.Layers[len(man.Layers)-1].Annotations[ocispec.AnnotationTitle] = "agents/other.oap"
			})
		}, "undeclared dependency layer"},
		{"authored path", func(files map[string][]byte) {
			mutateConfig(t, files, func(cfg *Manifest) { cfg.Requires.Agents[0] = RequiredAgent{Path: "dependencies/reviewer"} })
		}, "path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := repackOuter(t, packed, tc.mutate)
			_, err := Unpack(bad)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestPackUnpackEmbeddedAgentsEqualDigestEdges(t *testing.T) {
	shared := dependencyBundle(t, "shared")
	b := dependencyBundle(t, "root", dependencyBundle(t, "zulu", shared), dependencyBundle(t, "alpha", shared))
	packed, err := Pack(b)
	require.NoError(t, err)
	got, err := Unpack(packed)
	require.NoError(t, err)
	require.Len(t, got.Dependencies, 2)
	assert.Equal(t, "alpha", got.Dependencies[0].Descriptor.Name)
	assert.Equal(t, "zulu", got.Dependencies[1].Descriptor.Name)
	require.Len(t, got.Dependencies[0].Bundle.Dependencies, 1)
	require.Len(t, got.Dependencies[1].Bundle.Dependencies, 1)
	a := got.Dependencies[0].Bundle.Dependencies[0]
	z := got.Dependencies[1].Bundle.Dependencies[0]
	assert.Equal(t, a.Descriptor.Digest, z.Descriptor.Digest)
	assert.NotSame(t, a.Bundle, z.Bundle)
	assert.Equal(t, DependencyPath{"alpha", "shared"}, a.Path)
	assert.Equal(t, DependencyPath{"zulu", "shared"}, z.Path)
	require.NoError(t, ValidateDependencyGraph(got))
}

func TestEmbeddedResolverVerifiesBytes(t *testing.T) {
	parent, err := Unpack(embeddedArchiveFixture(t))
	require.NoError(t, err)
	req := parent.Manifest.Requires.Agents[0]
	parent.Dependencies[0].Bundle.Manifests = []byte("cached bundle must not be trusted")
	child, packed, err := (EmbeddedResolver{}).Resolve(context.Background(), parent, req)
	require.NoError(t, err)
	assert.Equal(t, "reviewer", child.Manifest.Agent.Name)
	require.NoError(t, child.Validate())
	assert.Equal(t, parent.Dependencies[0].Packed, packed)

	parent.Dependencies[0].Packed[0] ^= 1
	_, _, err = (EmbeddedResolver{}).Resolve(context.Background(), parent, req)
	assert.ErrorContains(t, err, "digest mismatch")
}

func TestEmbeddedResolverRejectsDependency(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Bundle, *RequiredAgent)
		want   string
	}{
		{"undeclared", func(_ *Bundle, req *RequiredAgent) { req.Name = "other" }, "undeclared"},
		{"missing", func(b *Bundle, _ *RequiredAgent) { b.Dependencies = nil }, "missing"},
		{"duplicate", func(b *Bundle, _ *RequiredAgent) { b.Dependencies = append(b.Dependencies, b.Dependencies[0]) }, "duplicate"},
		{"version mismatch", func(b *Bundle, req *RequiredAgent) { req.Version = "2.0.0"; b.Manifest.Requires.Agents[0] = *req }, "version"},
		{"invalid media type", func(_ *Bundle, req *RequiredAgent) { req.MediaType = "application/x-wrong" }, "mediaType"},
		{"authored path", func(_ *Bundle, req *RequiredAgent) { *req = RequiredAgent{Path: "child"} }, "path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Unpack(embeddedArchiveFixture(t))
			require.NoError(t, err)
			req := b.Manifest.Requires.Agents[0]
			tc.mutate(b, &req)
			_, _, err = (EmbeddedResolver{}).Resolve(context.Background(), b, req)
			assert.ErrorContains(t, err, tc.want)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := (EmbeddedResolver{}).Resolve(ctx, nil, RequiredAgent{})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestPackUnpackEmbeddedAgentsGraphBudgets(t *testing.T) {
	b := dependencyBundle(t, "test-coordinator", dependencyBundle(t, "reviewer"))
	packed, err := Pack(b)
	require.NoError(t, err)
	child, err := Pack(b.Dependencies[0].Bundle)
	require.NoError(t, err)
	packedSize := int64(len(packed) + len(child))
	// Independently tally every outer-entry payload and both inner manifests.
	extractedSize := int64(len(b.Manifests) + len(b.Dependencies[0].Bundle.Manifests))
	for _, archive := range [][]byte{packed, child} {
		files, err := untarPlain(archive)
		require.NoError(t, err)
		for _, data := range files {
			extractedSize += int64(len(data))
		}
	}
	for _, operation := range []string{"pack", "unpack"} {
		for _, overflow := range []int64{0, 1} {
			for _, limit := range []string{"packed", "extracted"} {
				t.Run(fmt.Sprintf("%s/%s/overflow=%d", operation, limit, overflow), func(t *testing.T) {
					budget := &graphBudget{}
					if limit == "packed" {
						budget.packedBytes = maxDependencyPackedBytes - packedSize + overflow
					} else {
						budget.extractedBytes = maxDependencyExtractedBytes - extractedSize + overflow
					}
					var err error
					if operation == "pack" {
						_, err = pack(b, nil, budget)
					} else {
						_, err = unpack(packed, nil, budget, map[digest.Digest]bool{})
					}
					if overflow == 0 {
						require.NoError(t, err)
					} else {
						assert.ErrorContains(t, err, "maximum aggregate "+limit+" bytes")
					}
				})
			}
		}
	}
}

func TestUnpackRejectsDependencyRecursionLimits(t *testing.T) {
	packed := embeddedArchiveFixture(t)
	_, err := unpack(packed, nil, &graphBudget{artifacts: maxDependencyArtifacts - 1}, map[digest.Digest]bool{})
	assert.ErrorContains(t, err, "maximum artifact count")
	assert.ErrorContains(t, err, "test-coordinator > reviewer")
	path := DependencyPath{"a", "b", "c", "d", "e", "f", "g", "h"}
	_, err = unpack(packed, path, &graphBudget{rootName: "root"}, map[digest.Digest]bool{})
	assert.ErrorContains(t, err, "maximum depth")
	assert.ErrorContains(t, err, "root > a > b > c > d > e > f > g > h > reviewer")
}

func TestUnpackRejectsDependencyActiveDigestCycle(t *testing.T) {
	packed := embeddedArchiveFixture(t)
	rootDigest, err := Digest(packed)
	require.NoError(t, err)
	active := map[digest.Digest]bool{digest.Digest(rootDigest): true}
	_, err = unpack(packed, DependencyPath{"repeat"}, &graphBudget{rootName: "root"}, active)
	assert.ErrorContains(t, err, "cycle")
	assert.ErrorContains(t, err, "root > repeat")
	assert.True(t, active[digest.Digest(rootDigest)], "ancestor's stack entry must remain")
}

func TestPack_DeterministicAndDigestable(t *testing.T) {
	b, err := FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)

	a1, err := Pack(b)
	require.NoError(t, err)
	a2, err := Pack(b)
	require.NoError(t, err)
	assert.Equal(t, a1, a2, "Pack must be byte-deterministic for equal input")

	dig, err := Digest(a1)
	require.NoError(t, err)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, dig)
}

func TestPackUnpack_RoundTrip(t *testing.T) {
	src, err := FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := Pack(src)
	require.NoError(t, err)

	got, err := Unpack(packed)
	require.NoError(t, err)
	assert.Equal(t, src.Manifest.Agent.Name, got.Manifest.Agent.Name)
	assert.Equal(t, src.Manifest.Agent.Version, got.Manifest.Agent.Version)
	assert.Equal(t, string(src.Manifests), string(got.Manifests))
	require.Contains(t, got.Assets, "assets/logo.txt")
	assert.Equal(t, src.Assets["assets/logo.txt"], got.Assets["assets/logo.txt"])

	crs, err := got.CRs()
	require.NoError(t, err)
	require.Len(t, crs, 1)
	assert.Equal(t, "demo-class", crs[0].GetName())
}

func TestReadTarGz_RejectsTraversal(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	body := []byte("x")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, _ = tw.Write(body)
	require.NoError(t, tw.Close())
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	require.NoError(t, gw.Close())

	_, err := readTarGz(gz.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe path")
}

func TestReadTarGz_RejectsAbsolutePath(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	body := []byte("x")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "/etc/passwd", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, _ = tw.Write(body)
	require.NoError(t, tw.Close())
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	require.NoError(t, gw.Close())

	_, err := readTarGz(gz.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe path")
}

func TestReadTarGz_RejectsSymlink(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
	require.NoError(t, tw.Close())
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	require.NoError(t, gw.Close())

	_, err := readTarGz(gz.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-regular")
}

// TestReadTarGz_RejectsOverEntryCap exercises the per-entry cap via
// readTarGzLimits (readTarGz's logic parameterized by cap) so the test
// doesn't need to build a real 64MiB+ fixture to trip the production cap.
func TestReadTarGz_RejectsOverEntryCap(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	body := bytes.Repeat([]byte("a"), 101) // > the 100-byte test cap below
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "big", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	require.NoError(t, gw.Close())

	_, err = readTarGzLimits(gz.Bytes(), maxTarEntries, 100, maxTarTotalByte)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds per-entry cap")
}

// TestReadTarGz_RejectsOverTotalCap exercises the total-uncompressed-bytes
// cap: three entries, each individually under the per-entry cap, whose sum
// exceeds the total cap.
func TestReadTarGz_RejectsOverTotalCap(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for i := 0; i < 3; i++ {
		body := bytes.Repeat([]byte("a"), 50) // each under the 100-byte entry cap
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f%d", i), Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	require.NoError(t, gw.Close())

	// 3 * 50 = 150 bytes, over a 120-byte total cap; each entry is under the
	// 100-byte per-entry cap so only the total check can trip.
	_, err := readTarGzLimits(gz.Bytes(), maxTarEntries, 100, 120)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds total uncompressed cap")
}

// TestReadTarGz_RejectsTooManyEntries exercises the entry-count cap.
func TestReadTarGz_RejectsTooManyEntries(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for i := 0; i < 4; i++ {
		body := []byte("x")
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f%d", i), Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	require.NoError(t, gw.Close())

	// 4 entries against a cap of 3.
	_, err := readTarGzLimits(gz.Bytes(), 3, maxTarEntryByte, maxTarTotalByte)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many entries")
}

// packFixture packs the shared oaptest bundle into a good .oap.
func packFixture(t *testing.T) []byte {
	t.Helper()
	src, err := FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := Pack(src)
	require.NoError(t, err)
	return packed
}

// repackOuter untars the outer OCI-layout tar, lets fn mutate the name→bytes
// map (drop or replace entries), and retars it — so a test can build a
// deliberately-broken .oap from the good fixture.
func repackOuter(t *testing.T, packed []byte, fn func(files map[string][]byte)) []byte {
	t.Helper()
	files, err := untarPlain(packed)
	require.NoError(t, err)
	fn(files)
	out, err := tarFiles(files)
	require.NoError(t, err)
	return out
}

// blobKey is the outer-tar path a content-addressed blob lives at.
func blobKey(d digest.Digest) string {
	return ocispec.ImageBlobsDir + "/" + d.Algorithm().String() + "/" + d.Encoded()
}

// readManifest reads the single OCI manifest blob out of an outer-tar file map.
func readManifest(t *testing.T, files map[string][]byte) ocispec.Manifest {
	t.Helper()
	var idx ocispec.Index
	require.NoError(t, json.Unmarshal(files[ocispec.ImageIndexFile], &idx))
	require.Len(t, idx.Manifests, 1)
	var man ocispec.Manifest
	require.NoError(t, json.Unmarshal(files[blobKey(idx.Manifests[0].Digest)], &man))
	return man
}

// mutateManifest lets fn edit the OCI manifest, then re-stores it under a fresh
// digest-named key AND updates index.json to point at it, keeping the archive
// self-consistent. Unpack content-verifies blobs, so an in-place manifest edit
// (old key, stale digest) would trip the digest-mismatch check first and mask
// the downstream behavior under test.
func mutateManifest(t *testing.T, files map[string][]byte, fn func(*ocispec.Manifest)) {
	t.Helper()
	var idx ocispec.Index
	require.NoError(t, json.Unmarshal(files[ocispec.ImageIndexFile], &idx))
	require.Len(t, idx.Manifests, 1)
	oldKey := blobKey(idx.Manifests[0].Digest)
	var man ocispec.Manifest
	require.NoError(t, json.Unmarshal(files[oldKey], &man))

	fn(&man)

	raw, err := json.Marshal(man)
	require.NoError(t, err)
	newDigest := digest.FromBytes(raw)
	delete(files, oldKey)
	files[blobKey(newDigest)] = raw
	idx.Manifests[0].Digest = newDigest
	idx.Manifests[0].Size = int64(len(raw))
	idxRaw, err := json.Marshal(idx)
	require.NoError(t, err)
	files[ocispec.ImageIndexFile] = idxRaw
}

func TestUnpack_MissingIndex_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		delete(files, ocispec.ImageIndexFile)
	})
	_, err := Unpack(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing index.json")
}

func TestUnpack_WrongArtifactType_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		mutateManifest(t, files, func(m *ocispec.Manifest) {
			m.ArtifactType = "application/vnd.bogus"
		})
	})
	_, err := Unpack(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected artifactType")
}

func TestUnpack_MissingBlob_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		// Drop the OCI manifest blob the index points to.
		var idx ocispec.Index
		require.NoError(t, json.Unmarshal(files[ocispec.ImageIndexFile], &idx))
		require.Len(t, idx.Manifests, 1)
		delete(files, blobKey(idx.Manifests[0].Digest))
	})
	_, err := Unpack(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing blob")
}

func TestUnpack_UnknownLayerMediaType_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		mutateManifest(t, files, func(m *ocispec.Manifest) {
			require.NotEmpty(t, m.Layers)
			// The layer blob stays a valid gzip'd tar with an unchanged digest;
			// only the declared media type is bogus, so blob-verify and
			// readTarGz both succeed and the switch hits default.
			m.Layers[0].MediaType = "application/vnd.bogus.layer"
		})
	})
	_, err := Unpack(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown layer media type")
}

func TestUnpack_MissingManifestsLayer_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		mutateManifest(t, files, func(m *ocispec.Manifest) {
			// Drop the manifests-media-type layer, keeping any others (assets),
			// so the bundle parses but never sees a manifests layer.
			var kept []ocispec.Descriptor
			for _, l := range m.Layers {
				if l.MediaType != ManifestsMediaType {
					kept = append(kept, l)
				}
			}
			m.Layers = kept
		})
	})
	_, err := Unpack(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".oap is missing its manifests layer")
}

// TestUnpack_TamperedLayerBlob_FailsClosed is the RED for the content-address
// digest check: a layer's bytes are swapped but its digest-named key and the
// manifest's claim are left untouched — exactly the "swapped layer survives a
// valid manifest signature" case. WITHOUT the digest verification in Unpack's
// blob() closure this test fails (Unpack would parse the tampered layer as if
// trusted); WITH it, Unpack rejects the content mismatch.
func TestUnpack_TamperedLayerBlob_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		man := readManifest(t, files)
		require.NotEmpty(t, man.Layers)
		key := blobKey(man.Layers[0].Digest)
		require.Contains(t, files, key)
		files[key] = append(append([]byte{}, files[key]...), '!') // swap bytes, keep the key
	})
	_, err := Unpack(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content digest mismatch")
}

func TestDigest_TamperedManifestBlob_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		var idx ocispec.Index
		require.NoError(t, json.Unmarshal(files[ocispec.ImageIndexFile], &idx))
		require.Len(t, idx.Manifests, 1)
		key := blobKey(idx.Manifests[0].Digest)
		files[key] = append(append([]byte{}, files[key]...), ' ') // tamper, don't re-digest
	})
	_, err := Digest(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest blob content digest mismatch")
}

func TestDigest_MissingManifestBlob_FailsClosed(t *testing.T) {
	bad := repackOuter(t, packFixture(t), func(files map[string][]byte) {
		var idx ocispec.Index
		require.NoError(t, json.Unmarshal(files[ocispec.ImageIndexFile], &idx))
		require.Len(t, idx.Manifests, 1)
		delete(files, blobKey(idx.Manifests[0].Digest))
	})
	_, err := Digest(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing manifest blob")
}

// TestUnpack_EmptyManifestsLayer_NotMissing guards the empty-but-present fix:
// a Bundle with a 0-byte manifests stream still writes a manifests layer, so
// Unpack must NOT misreport it as missing. Tested directly against Unpack
// (not via Validate).
func TestUnpack_EmptyManifestsLayer_NotMissing(t *testing.T) {
	src := &Bundle{
		Manifest:  &Manifest{OapFormatVersion: "1", Agent: Agent{Name: "empty", Version: "0.0.1"}},
		Manifests: []byte{},
	}
	packed, err := Pack(src)
	require.NoError(t, err)

	got, err := Unpack(packed)
	require.NoError(t, err)
	assert.Empty(t, got.Manifests)
}
