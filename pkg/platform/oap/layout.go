package oap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Hardened extraction caps for the manifests/assets layer tars, whose
// contents may be attacker-influenced. The outer OCI-layout container tar is
// also attacker-chosen on the Unpack-from-disk path, so untarPlain enforces
// its own (looser) count + total caps below.
const (
	maxTarEntries   = 10_000    // refuse archives with an implausible entry count
	maxTarEntryByte = 64 << 20  // 64 MiB per entry
	maxTarTotalByte = 256 << 20 // 256 MiB total uncompressed (inner layers)

	// maxOuterTotalByte caps the total bytes untarPlain will buffer from the
	// uncompressed outer container tar. It's generous relative to the 256 MiB
	// inner cap because the outer tar holds every layer blob (each up to the
	// inner cap, compressed) plus config/index; it exists to bound memory
	// against a giant attacker-supplied file, not to amplification-protect
	// (the outer tar is uncompressed, so there is no decompression bomb).
	maxOuterTotalByte = 512 << 20 // 512 MiB total (outer container tar)
)

// Pack serializes a Bundle into a .oap: a tar of an OCI image layout
// (oci-layout + index.json + blobs/sha256/<hex>). Deterministic for equal input.
func Pack(b *Bundle) ([]byte, error) {
	if b == nil || b.Manifest == nil {
		return nil, fmt.Errorf("pack: nil bundle or manifest")
	}
	if len(b.Dependencies) > 0 || len(b.Manifest.Requires.Agents) > 0 {
		if err := ValidateDependencyGraph(b); err != nil {
			return nil, err
		}
	}
	return pack(b, nil, &graphBudget{rootName: b.Manifest.Agent.Name})
}

func pack(b *Bundle, dependencyPath DependencyPath, budget *graphBudget) (packed []byte, err error) {
	defer func() {
		if err != nil {
			err = qualifyDependencyError(budget.rootName, dependencyPath, err)
		}
	}()
	if err := budget.enter(dependencyPath); err != nil {
		return nil, err
	}
	blobs := map[digest.Digest][]byte{}
	add := func(data []byte) ocispec.Descriptor {
		d := digest.FromBytes(data)
		blobs[d] = data
		return ocispec.Descriptor{Digest: d, Size: int64(len(data))}
	}

	// Only the config copy is rewritten. Authored paths and dependency order
	// remain untouched in the caller's bundle.
	config := *b.Manifest
	config.Requires.Agents = nil
	children := slices.Clone(b.Dependencies)
	slices.SortFunc(children, func(a, b *Dependency) int {
		return strings.Compare(a.Bundle.Manifest.Agent.Name, b.Bundle.Manifest.Agent.Name)
	})
	var dependencyLayers []ocispec.Descriptor
	for _, child := range children {
		name := child.Bundle.Manifest.Agent.Name
		raw, err := pack(child.Bundle, appendDependencyPath(dependencyPath, name), budget)
		if err != nil {
			return nil, err
		}
		layer := add(raw)
		layer.MediaType = DependencyMediaType
		layer.Annotations = map[string]string{ocispec.AnnotationTitle: "agents/" + name + ".oap"}
		dependencyLayers = append(dependencyLayers, layer)
		config.Requires.Agents = append(config.Requires.Agents, RequiredAgent{
			Name: name, Version: child.Bundle.Manifest.Agent.Version,
			MediaType: DependencyMediaType, Digest: layer.Digest.String(),
		})
	}
	cfgJSON, err := json.Marshal(&config)
	if err != nil {
		return nil, fmt.Errorf("marshal oap manifest: %w", err)
	}
	cfg := add(cfgJSON)
	cfg.MediaType = ConfigMediaType

	var layers []ocispec.Descriptor

	manTar, err := gzipTar(map[string][]byte{"manifests.yaml": b.Manifests})
	if err != nil {
		return nil, err
	}
	ml := add(manTar)
	ml.MediaType = ManifestsMediaType
	ml.Annotations = map[string]string{ocispec.AnnotationTitle: "manifests.yaml"}
	layers = append(layers, ml)

	if len(b.Assets) > 0 {
		files := map[string][]byte{}
		for _, k := range b.assetPaths() {
			files[k] = b.Assets[k]
		}
		assetTar, err := gzipTar(files)
		if err != nil {
			return nil, err
		}
		al := add(assetTar)
		al.MediaType = AssetsMediaType
		layers = append(layers, al)
	}

	if len(b.Readme) > 0 {
		readmeTar, err := gzipTar(map[string][]byte{"README.md": b.Readme})
		if err != nil {
			return nil, err
		}
		rl := add(readmeTar)
		rl.MediaType = ReadmeMediaType
		rl.Annotations = map[string]string{ocispec.AnnotationTitle: "README.md"}
		layers = append(layers, rl)
	}
	layers = append(layers, dependencyLayers...)

	man := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       cfg,
		Layers:       layers,
	}
	manJSON, err := json.Marshal(man)
	if err != nil {
		return nil, fmt.Errorf("marshal oci manifest: %w", err)
	}
	manDesc := add(manJSON)
	manDesc.MediaType = ocispec.MediaTypeImageManifest
	manDesc.ArtifactType = ArtifactType
	manDesc.Annotations = map[string]string{ocispec.AnnotationRefName: b.Manifest.Agent.Version}

	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manDesc},
	}
	idxJSON, err := json.Marshal(idx)
	if err != nil {
		return nil, fmt.Errorf("marshal oci index: %w", err)
	}

	layoutJSON, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		return nil, fmt.Errorf("marshal oci-layout: %w", err)
	}
	files := map[string][]byte{
		ocispec.ImageLayoutFile: layoutJSON,
		ocispec.ImageIndexFile:  idxJSON,
	}
	for d, data := range blobs {
		files[ocispec.ImageBlobsDir+"/"+d.Algorithm().String()+"/"+d.Encoded()] = data
	}
	// Match unpack accounting: outer regular-entry payloads plus the inner
	// layer payloads, at every node. Packed bytes count each logical edge and
	// the root once, even though descendants also occur in ancestor archives.
	if err := budget.addExtracted(bundleExtractedBytes(b)); err != nil {
		return nil, err
	}
	for _, data := range files {
		if err := budget.addExtracted(int64(len(data))); err != nil {
			return nil, err
		}
	}
	packed, err = tarFiles(files)
	if err != nil {
		return nil, err
	}
	if err := budget.addPacked(int64(len(packed))); err != nil {
		return nil, err
	}
	return packed, nil
}

// Digest returns the OCI manifest digest of a packed .oap — the value a
// registry references and what `inspect`/pins report.
func Digest(oapBytes []byte) (string, error) {
	files, err := untarPlain(oapBytes)
	if err != nil {
		return "", err
	}
	idxRaw, ok := files[ocispec.ImageIndexFile]
	if !ok {
		return "", fmt.Errorf("not a .oap: missing index.json")
	}
	var idx ocispec.Index
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return "", fmt.Errorf("parse index.json: %w", err)
	}
	if len(idx.Manifests) != 1 {
		return "", fmt.Errorf("expected exactly one manifest in index, got %d", len(idx.Manifests))
	}
	// The returned digest is the trust anchor pins/OCI-push/cosign build on, so
	// verify the manifest blob actually hashes to the index's claim before
	// handing it back — a swapped manifest must not survive.
	claimed := idx.Manifests[0].Digest
	manBlob, ok := files[ocispec.ImageBlobsDir+"/"+claimed.Algorithm().String()+"/"+claimed.Encoded()]
	if !ok {
		return "", fmt.Errorf("missing manifest blob %s", claimed)
	}
	if got := digest.FromBytes(manBlob); got != claimed {
		return "", fmt.Errorf("manifest blob content digest mismatch: index claims %s, bytes hash to %s (tampered?)", claimed, got)
	}
	return claimed.String(), nil
}

// Unpack reads and verifies an OCI-layout tar and all embedded child archives.
// Outer containers and inner content layers retain their per-artifact caps,
// with one aggregate budget shared across the complete dependency graph.
func Unpack(oapBytes []byte) (*Bundle, error) {
	out, err := unpack(oapBytes, nil, &graphBudget{}, map[digest.Digest]bool{})
	if err != nil {
		return nil, err
	}
	if len(out.Dependencies) > 0 {
		if err := ValidateDependencyGraph(out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func unpack(oapBytes []byte, dependencyPath DependencyPath, budget *graphBudget, active map[digest.Digest]bool) (out *Bundle, err error) {
	defer func() {
		if err != nil {
			err = qualifyDependencyError(budget.rootName, dependencyPath, err)
		}
	}()
	if err := budget.enter(dependencyPath); err != nil {
		return nil, err
	}
	if err := budget.addPacked(int64(len(oapBytes))); err != nil {
		return nil, err
	}
	files, err := untarPlainBudget(oapBytes, budget)
	if err != nil {
		return nil, err
	}
	idxRaw, ok := files[ocispec.ImageIndexFile]
	if !ok {
		return nil, fmt.Errorf("not a .oap: missing %s", ocispec.ImageIndexFile)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ocispec.ImageIndexFile, err)
	}
	if len(idx.Manifests) != 1 {
		return nil, fmt.Errorf("expected exactly one manifest in index, got %d", len(idx.Manifests))
	}
	// blob fetches a content-addressed blob AND verifies its bytes hash to the
	// declared digest. This is the trust anchor: a swapped config/manifest/layer
	// is caught here even under an otherwise-valid manifest signature. Covers
	// config + manifest + every layer in one place.
	blob := func(d digest.Digest) ([]byte, error) {
		b, ok := files[ocispec.ImageBlobsDir+"/"+d.Algorithm().String()+"/"+d.Encoded()]
		if !ok {
			return nil, fmt.Errorf("missing blob %s", d)
		}
		if got := digest.FromBytes(b); got != d {
			return nil, fmt.Errorf("blob %s content digest mismatch: got %s (tampered?)", d, got)
		}
		return b, nil
	}

	manRaw, err := blob(idx.Manifests[0].Digest)
	if err != nil {
		return nil, err
	}
	manifestDigest := idx.Manifests[0].Digest
	if active[manifestDigest] {
		return nil, fmt.Errorf("dependency graph cycle at manifest %s", manifestDigest)
	}
	active[manifestDigest] = true
	defer delete(active, manifestDigest)
	var man ocispec.Manifest
	if err := json.Unmarshal(manRaw, &man); err != nil {
		return nil, fmt.Errorf("parse oci manifest: %w", err)
	}
	if man.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("unexpected artifactType %q (want %q)", man.ArtifactType, ArtifactType)
	}

	cfgRaw, err := blob(man.Config.Digest)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(cfgRaw)
	if err != nil {
		return nil, err
	}
	if len(dependencyPath) == 0 {
		budget.rootName = m.Agent.Name
	}
	required := make(map[string]RequiredAgent, len(m.Requires.Agents))
	for _, req := range m.Requires.Agents {
		if req.Path != "" {
			return nil, fmt.Errorf("packed dependency must not retain authored path %q", req.Path)
		}
		if _, exists := required[req.Name]; exists {
			return nil, fmt.Errorf("duplicate dependency descriptor %q", req.Name)
		}
		required[req.Name] = req
	}

	out = &Bundle{Manifest: m, Assets: map[string][]byte{}}
	resolved := make(map[string]*Dependency, len(required))
	var sawManifests bool
	for _, layer := range man.Layers {
		// Dependencies are raw OCI-layout tars, not gzip-compressed content
		// layers. Classify before decoding to fail closed on unknown types.
		switch layer.MediaType {
		case ManifestsMediaType, AssetsMediaType, ReadmeMediaType, DependencyMediaType:
		default:
			return nil, fmt.Errorf("unknown layer media type %q", layer.MediaType)
		}
		data, err := blob(layer.Digest)
		if err != nil {
			return nil, err
		}
		if layer.MediaType == DependencyMediaType {
			title := layer.Annotations[ocispec.AnnotationTitle]
			name := strings.TrimSuffix(strings.TrimPrefix(title, "agents/"), ".oap")
			req, exists := required[name]
			if !exists || title != "agents/"+req.Name+".oap" {
				return nil, fmt.Errorf("undeclared dependency layer %q", title)
			}
			childPath := appendDependencyPath(dependencyPath, name)
			if resolved[name] != nil {
				return nil, qualifyDependencyError(budget.rootName, childPath, fmt.Errorf("duplicate dependency layer %q", title))
			}
			if req.Digest != layer.Digest.String() || req.MediaType != layer.MediaType {
				return nil, qualifyDependencyError(budget.rootName, childPath, fmt.Errorf("dependency descriptor digest or media type does not match layer"))
			}
			if layer.Size != int64(len(data)) {
				return nil, qualifyDependencyError(budget.rootName, childPath, fmt.Errorf("dependency layer size mismatch"))
			}
			child, err := unpack(data, childPath, budget, active)
			if err != nil {
				return nil, err
			}
			dep := &Dependency{Descriptor: req, Path: childPath, Bundle: child, Packed: data}
			if err := validateDependencyEdge(req, dep, child.Manifest.Agent.Name); err != nil {
				return nil, qualifyDependencyError(budget.rootName, childPath, err)
			}
			resolved[name] = dep
			continue
		}
		entries, err := readTarGzBudget(data, maxTarEntries, maxTarEntryByte, maxTarTotalByte, budget)
		if err != nil {
			return nil, fmt.Errorf("read layer %s: %w", layer.MediaType, err)
		}
		switch layer.MediaType {
		case ManifestsMediaType:
			// Track presence explicitly: an empty (0-byte) manifests stream is
			// a present-but-empty layer, not a missing one.
			out.Manifests = entries["manifests.yaml"]
			sawManifests = true
		case AssetsMediaType:
			for k, v := range entries {
				out.Assets[k] = v
			}
		case ReadmeMediaType:
			out.Readme = entries["README.md"]
			out.Manifest.Content.Readme = &LayerRef{MediaType: layer.MediaType, Digest: layer.Digest.String()}
		default:
			return nil, fmt.Errorf("unknown layer media type %q", layer.MediaType)
		}
	}
	if !sawManifests {
		return nil, fmt.Errorf(".oap is missing its manifests layer")
	}
	for _, req := range m.Requires.Agents {
		dep := resolved[req.Name]
		if dep == nil {
			return nil, fmt.Errorf("missing dependency layer for %q", req.Name)
		}
		out.Dependencies = append(out.Dependencies, dep)
	}
	return out, nil
}

// EmbeddedResolver verifies and decodes a parent's embedded dependency bytes.
// It never trusts the cached Bundle, which callers may have modified.
type EmbeddedResolver struct{}

var _ Resolver = EmbeddedResolver{}

func (EmbeddedResolver) Resolve(ctx context.Context, parent *Bundle, req RequiredAgent) (*Bundle, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if parent == nil || parent.Manifest == nil {
		return nil, nil, fmt.Errorf("resolve embedded dependency: nil parent or manifest")
	}
	if req.Path != "" {
		return nil, nil, fmt.Errorf("embedded dependency requires a packed descriptor, not path %q", req.Path)
	}
	if err := req.validate(0); err != nil {
		return nil, nil, err
	}
	if !slices.Contains(parent.Manifest.Requires.Agents, req) {
		return nil, nil, fmt.Errorf("undeclared dependency descriptor %q", req.Name)
	}
	var found *Dependency
	for _, dep := range parent.Dependencies {
		if dep != nil && dep.Descriptor.Name == req.Name {
			if found != nil {
				return nil, nil, fmt.Errorf("duplicate dependency %q", req.Name)
			}
			found = dep
		}
	}
	if found == nil {
		return nil, nil, fmt.Errorf("missing dependency %q", req.Name)
	}
	if digest.FromBytes(found.Packed).String() != req.Digest {
		return nil, nil, fmt.Errorf("dependency %q digest mismatch", req.Name)
	}
	child, err := Unpack(found.Packed)
	if err != nil {
		return nil, nil, err
	}
	if err := child.Validate(); err != nil {
		return nil, nil, err
	}
	if err := validateDependencyEdge(req, &Dependency{Bundle: child}, child.Manifest.Agent.Name); err != nil {
		return nil, nil, err
	}
	return child, found.Packed, nil
}

// readTarGz gunzips then untars data with hardened extraction, using the
// production caps (maxTarEntries, maxTarEntryByte, maxTarTotalByte).
func readTarGz(data []byte) (map[string][]byte, error) {
	return readTarGzLimits(data, maxTarEntries, maxTarEntryByte, maxTarTotalByte)
}

// readTarGzLimits is readTarGz's implementation parameterized by cap, so
// tests can exercise the entry/total/count-cap rejection paths without
// constructing multi-hundred-megabyte fixtures. Recursive Unpack uses the
// same reader with the production caps and a shared graph budget.
//
// Rejections enforced, in order: entry count, non-regular entry type
// (symlink/hardlink/device/dir), unsafe path (absolute or path-traversal),
// per-entry size cap, running-total size cap.
func readTarGzLimits(data []byte, maxEntries int, maxEntryByte, maxTotalByte int64) (out map[string][]byte, err error) {
	return readTarGzBudget(data, maxEntries, maxEntryByte, maxTotalByte, nil)
}

func readTarGzBudget(data []byte, maxEntries int, maxEntryByte, maxTotalByte int64, budget *graphBudget) (out map[string][]byte, err error) {
	gr, gzErr := gzip.NewReader(bytes.NewReader(data))
	if gzErr != nil {
		return nil, fmt.Errorf("gunzip: %w", gzErr)
	}
	// A gzip Close error means a corrupt/truncated trailer (CRC or size
	// mismatch); surface it when the read otherwise succeeded.
	defer func() {
		if cerr := gr.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("gunzip close: %w", cerr)
		}
	}()
	tr := tar.NewReader(gr)
	out = map[string][]byte{}
	var total int64
	var count int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		count++
		if count > maxEntries {
			return nil, fmt.Errorf("archive has too many entries (> %d)", maxEntries)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("archive entry %q is non-regular (type %d) — refused", hdr.Name, hdr.Typeflag)
		}
		if !safeRelPath(hdr.Name) {
			return nil, fmt.Errorf("archive entry %q has an unsafe path — refused", hdr.Name)
		}
		if hdr.Size > maxEntryByte {
			return nil, fmt.Errorf("archive entry %q exceeds per-entry cap (%d > %d bytes)", hdr.Name, hdr.Size, maxEntryByte)
		}
		total += hdr.Size
		if total > maxTotalByte {
			return nil, fmt.Errorf("archive exceeds total uncompressed cap (> %d bytes)", maxTotalByte)
		}
		if budget != nil {
			if err := budget.addExtracted(hdr.Size); err != nil {
				return nil, err
			}
		}
		var b bytes.Buffer
		if _, err := io.CopyN(&b, tr, hdr.Size); err != nil {
			return nil, fmt.Errorf("read entry %q: %w", hdr.Name, err)
		}
		out[hdr.Name] = b.Bytes()
	}
	return out, nil
}

// safeRelPath rejects absolute paths and any path that escapes its root via
// ".." traversal (including nested traversal, e.g. "a/../../etc/passwd").
func safeRelPath(name string) bool {
	if name == "" || path.IsAbs(name) {
		return false
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	return true
}

// gzipTar tars the given files (sorted, zeroed mod-times) and gzip-compresses
// the result for byte-determinism.
func gzipTar(files map[string][]byte) ([]byte, error) {
	raw, err := tarFiles(files)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// tarFiles writes files into a tar in sorted name order with a fixed header so
// equal input yields identical bytes.
func tarFiles(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		data := files[n]
		hdr := &tar.Header{
			Name:     n,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR, // pin format so digests stay stable across Go versions
			// ModTime left as zero value for determinism.
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar header %s: %w", n, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("tar write %s: %w", n, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	return buf.Bytes(), nil
}

// untarPlain reads the uncompressed outer OCI-layout container tar into
// name→bytes. On the Unpack-from-disk path those bytes are attacker-chosen, so
// it rejects unsafe paths and non-regular entries other than safe directories,
// and enforces an entry-count cap (including skipped directories) and a
// total-size cap using bounded reads. There's no decompression amplification
// here (the outer tar is uncompressed); the inner manifests/assets layers use the
// stricter hardened reader (readTarGz). Content-address digest verification of
// the blobs read out of this map happens in Unpack/Digest.
func untarPlain(data []byte) (map[string][]byte, error) {
	return untarPlainBudget(data, nil)
}

func untarPlainBudget(data []byte, budget *graphBudget) (map[string][]byte, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	out := map[string][]byte{}
	var total int64
	var count int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		count++
		if count > maxTarEntries {
			return nil, fmt.Errorf("container tar has too many entries (> %d)", maxTarEntries)
		}
		if !safeRelPath(hdr.Name) {
			return nil, fmt.Errorf("container tar entry %q has an unsafe path — refused", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("container tar entry %q is non-regular (type %d) — refused", hdr.Name, hdr.Typeflag)
		}
		total += hdr.Size
		if total > maxOuterTotalByte {
			return nil, fmt.Errorf("container tar exceeds total size cap (> %d bytes)", maxOuterTotalByte)
		}
		if budget != nil {
			if err := budget.addExtracted(hdr.Size); err != nil {
				return nil, err
			}
		}
		var b bytes.Buffer
		if _, err := io.CopyN(&b, tr, hdr.Size); err != nil {
			return nil, fmt.Errorf("read tar entry %s: %w", hdr.Name, err)
		}
		out[hdr.Name] = b.Bytes()
	}
	return out, nil
}
