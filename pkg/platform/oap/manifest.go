// Package oap defines the .oap agent-container format: a single-file, OCI-native
// bundle describing one AgentClass and its dependency graph.
package oap

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"sigs.k8s.io/yaml"
)

// SupportedFormatMajor is the highest oapFormatVersion major this build understands.
const SupportedFormatMajor = 1

// OCI media types backing a .oap. Copied verbatim into manifests and registries.
const (
	ArtifactType        = "application/vnd.agentprimitives.authzed.com.agent.v1"
	ConfigMediaType     = "application/vnd.agentprimitives.authzed.com.agent.config.v1+json"
	ManifestsMediaType  = "application/vnd.agentprimitives.authzed.com.agent.manifests.v1.tar+gzip"
	AssetsMediaType     = "application/vnd.agentprimitives.authzed.com.agent.assets.v1.tar+gzip"
	ReadmeMediaType     = "application/vnd.agentprimitives.authzed.com.agent.readme.v1+markdown"
	DependencyMediaType = "application/vnd.agentprimitives.authzed.com.agent.dependency.v1"
)

// DefaultExtension is the default filename suffix for a packed bundle. It is
// the single source of truth for the suffix and is NEVER used to identify a
// bundle — the format is recognized by its OCI media type / content.
const DefaultExtension = ".oap"

// Manifest is the .oap config blob: the human/tooling-facing description.
type Manifest struct {
	// Format version; only the major is read, and a newer major is refused.
	OapFormatVersion string   `json:"oapFormatVersion"`
	Agent            Agent    `json:"agent"`
	Compat           Compat   `json:"compat,omitempty"`
	Requires         Requires `json:"requires,omitempty"`
	// Packed-layer digests, derived on unpack rather than authored.
	Content Content `json:"content,omitempty"`
	// Install-time prompts, asked in declaration order.
	Questions []Question `json:"questions,omitempty"`
}

// Agent is the bundled agent's own identity and version.
type Agent struct {
	Name string `json:"name"`
	// The bundle's own version: recorded as install provenance, never compared.
	Version     string `json:"version"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	// Assets key of the logo image; Validate rejects one the bundle omits.
	Logo        string   `json:"logo,omitempty"`
	Maintainers []string `json:"maintainers,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	License     string   `json:"license,omitempty"`
}

// Compat is the operator/CRD compatibility floor for installing this bundle.
type Compat struct {
	// Semver floor on the cluster's oap version; empty skips the check.
	MinApVersion string `json:"minApVersion,omitempty"`
	// The CRD apiVersion this bundle targets; nothing enforces it today.
	CRDApiVersion string `json:"crdApiVersion,omitempty"`
}

// Requires declares what the cluster must provide; never secret values.
type Requires struct {
	Secrets []RequiredSecret `json:"secrets,omitempty"`
	Images  []RequiredImage  `json:"images,omitempty"`
	// Canonical skill refs the agent uses; recorded for the reader, not fetched.
	Skills []RequiredSkill `json:"skills,omitempty"`
	// Shared cluster-scoped resources: adopted when present, never created here.
	ClusterDeps []RequiredClusterDep `json:"clusterDeps,omitempty"`
	// Channels the agent needs wired before it can send or receive.
	Channels []RequiredChannel `json:"channels,omitempty"`
	// Agents are complete private child bundles, authored by folder path and
	// represented in packed bundles by an immutable content descriptor.
	Agents []RequiredAgent `json:"agents,omitempty"`
}

// RequiredAgent identifies one complete private child bundle. Folder sources
// set Path only. Packed bundles replace Path with the remaining descriptor
// fields so machine-local paths never enter an artifact.
type RequiredAgent struct {
	Path      string `json:"path,omitempty"`
	Name      string `json:"name,omitempty"`
	Version   string `json:"version,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

func (r RequiredAgent) validate(index int) error {
	prefix := fmt.Sprintf("requires.agents[%d]", index)
	hasDescriptor := r.Name != "" || r.Version != "" || r.MediaType != "" || r.Digest != ""
	if r.Path != "" {
		if hasDescriptor {
			return fmt.Errorf("%s: cannot mix path with packed descriptor", prefix)
		}
		if filepath.IsAbs(r.Path) {
			return fmt.Errorf("%s: path must be relative", prefix)
		}
		clean := filepath.Clean(r.Path)
		if clean == "." {
			return fmt.Errorf("%s: path must name a dependency folder", prefix)
		}
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s: path escapes bundle root", prefix)
		}
		if clean != r.Path {
			return fmt.Errorf("%s: path must be clean", prefix)
		}
		return nil
	}
	if !hasDescriptor {
		return fmt.Errorf("%s: path is required for a folder dependency", prefix)
	}
	if r.Name == "" {
		return fmt.Errorf("%s: name is required for a packed descriptor", prefix)
	}
	if r.Version == "" {
		return fmt.Errorf("%s: version is required for a packed descriptor", prefix)
	}
	if r.MediaType == "" {
		return fmt.Errorf("%s: mediaType is required for a packed descriptor", prefix)
	}
	if r.MediaType != DependencyMediaType {
		return fmt.Errorf("%s: mediaType must be %s, got %q", prefix, DependencyMediaType, r.MediaType)
	}
	if r.Digest == "" {
		return fmt.Errorf("%s: digest is required for a packed descriptor", prefix)
	}
	if _, err := digest.Parse(r.Digest); err != nil {
		return fmt.Errorf("%s: invalid digest: %w", prefix, err)
	}
	return nil
}

// RequiredSecret names a Secret the installed CRs read. Names and keys only —
// a bundle never carries a secret value.
type RequiredSecret struct {
	Name string `json:"name"`
	// The data keys the CRs read; empty means the whole Secret, not one key.
	Keys []string `json:"keys,omitempty"`
	// Operator-facing prose saying what the credential is for.
	Purpose string `json:"purpose,omitempty"`
	// The Questions[] entry collecting this value; empty means install asks none.
	Question string `json:"question,omitempty"`
}

// RequiredImage is one container image the bundled CRs reference, with an
// optional recipe for building it when the cluster cannot already pull it.
type RequiredImage struct {
	// The ref as written in the CRs; install rewrites it to what it resolved.
	Ref string `json:"ref"`
	// Marks an image shipped alongside the bundle; nothing reads it today.
	Vendored bool `json:"vendored,omitempty"`
	// Recipe used when Ref is not pullable; nil makes an absent image fatal.
	Build *ImageBuild `json:"build,omitempty"`
}

// ImageBuild is the docker/buildx recipe for one RequiredImage. Dockerfile and
// Context reach docker verbatim, so both are refused when absolute or
// "../"-escaping: an untrusted bundle must not name a path outside itself.
type ImageBuild struct {
	// Dockerfile path relative to the bundle root, passed as docker -f.
	Dockerfile string `json:"dockerfile"`
	// Build-context path relative to the bundle root (docker's final argument).
	Context string `json:"context"`
	// Target platforms; nothing reads it — the installer's --platform decides.
	Platforms []string `json:"platforms,omitempty"`
	// BuildKit secret ids; the build is refused when one is not supplied.
	Secrets []string `json:"secrets,omitempty"`
	// Named --build-context inputs the installer supplies; these may be absolute.
	BuildContexts []string `json:"buildContexts,omitempty"`
	// --build-arg values, emitted in sorted key order so a build is reproducible.
	Args map[string]string `json:"args,omitempty"`
}

// RequiredSkill records one skill the agent uses, by canonical name.
type RequiredSkill struct {
	// The Skill's spec.canonicalName ("repo//subpath@ref"), not a CR name.
	Canonical string `json:"canonical"`
}

// RequiredClusterDep is one cluster-scoped resource shared across installs.
// EnsureClusterDeps adopts it when present and compatible; it never creates one.
type RequiredClusterDep struct {
	// The dependency's Kind, always in this project's own CRD group.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// What the manifest expects the live resource to record; nil asserts no pin.
	Pin *Pin `json:"pin,omitempty"`
}

// Pin is the baseline a RequiredClusterDep expects the live resource to record.
type Pin struct {
	// Compared to the live status.pin.digest; a disagreeing one is a conflict.
	Digest string `json:"digest,omitempty"`
	// Human-readable version alongside Digest; nothing compares it.
	Version string `json:"version,omitempty"`
}

// RequiredChannel is a Channel the installed agent needs to function. The
// bundle names the kind, the role and the CR's name; everything per-install —
// which workspace, which org, which credentials — is collected by that kind's
// wizard at install time.
//
// A Channel is never bundled (see allowedBundleKinds): it is deployment
// config, not portable content. This declares that one is REQUIRED, so install
// can create it from answers.
type RequiredChannel struct {
	// Kind is a registered channelkinds name: "github", "slack", …
	Kind string `json:"kind"`
	// Role is "input", "output", "both" or "monitoring", matching
	// ChannelSpec.Role.
	Role string `json:"role"`
	// Name is the Channel CR's name. Declared rather than prompted because
	// other bundled CRs reference resources derived from it — a github
	// channel's credentials Secret is "<name>-creds", which an AgentIdentity
	// in the same bundle names directly.
	Name string `json:"name"`
	// Purpose is operator-facing prose, shown before this channel's wizard.
	Purpose string `json:"purpose,omitempty"`
}

// Content mirrors the packed layer digests for offline inspection. Unpack
// derives it; a manifest author never writes it, and only Readme is filled in.
type Content struct {
	Manifests *LayerRef `json:"manifests,omitempty"`
	Assets    *LayerRef `json:"assets,omitempty"`
	Readme    *LayerRef `json:"readme,omitempty"`
}

// LayerRef identifies one packed layer by media type and content digest.
type LayerRef struct {
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

// ParseManifest unmarshals the config blob. It tolerates unknown additive
// fields (forward compatibility within a known format major).
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse oap manifest: %w", err)
	}
	return &m, nil
}

// Validate enforces the version gate and required identity fields.
func (m *Manifest) Validate() error {
	major, err := formatMajor(m.OapFormatVersion)
	if err != nil {
		return err
	}
	if major > SupportedFormatMajor {
		return fmt.Errorf("oapFormatVersion %q is newer than this oap supports (max major %d); upgrade oap", m.OapFormatVersion, SupportedFormatMajor)
	}
	if m.Agent.Name == "" {
		return fmt.Errorf("agent.name is required")
	}
	if m.Agent.Version == "" {
		return fmt.Errorf("agent.version is required")
	}
	for i, im := range m.Requires.Images {
		if im.Ref == "" {
			return fmt.Errorf("requires.images[%d]: ref is required", i)
		}
		if im.Build != nil {
			if im.Build.Dockerfile == "" || im.Build.Context == "" {
				return fmt.Errorf("requires.images[%d] (%s): build requires both dockerfile and context", i, im.Ref)
			}
		}
	}
	paths := make(map[string]int, len(m.Requires.Agents))
	for i, dep := range m.Requires.Agents {
		if err := dep.validate(i); err != nil {
			return err
		}
		if dep.Path == "" {
			continue
		}
		if prior, ok := paths[dep.Path]; ok {
			return fmt.Errorf("requires.agents[%d]: duplicate path %q (already declared at index %d)", i, dep.Path, prior)
		}
		paths[dep.Path] = i
	}
	return nil
}

// formatMajor extracts the integer major from a version like "1" or "1.3".
func formatMajor(v string) (int, error) {
	if v == "" {
		return 0, fmt.Errorf("oapFormatVersion is required")
	}
	major := v
	if i := strings.IndexByte(v, '.'); i >= 0 {
		major = v[:i]
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("oapFormatVersion %q: major must be an integer", v)
	}
	return n, nil
}
