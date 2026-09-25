// Package apimage is the single source of truth for oap's first-party container
// images: their CLI build-target names, image names, local :dev tags,
// Dockerfiles, and build contexts. Adding a new first-party image is a one-line
// addition to the vars + All below; oap build, the manifest image rewrite
// (oap install --image-registry), the security-wizard detector default, and the
// operator runner default all derive from this catalog.
package apimage

import "strings"

// Image describes one first-party image oap builds.
type Image struct {
	Target     string // `oap build <target>` name, e.g. "channelsd"
	Name       string // image name, e.g. "agentprimitives-channelsd"
	Dockerfile string // "" → docker infers <Context>/Dockerfile
	Context    string // build context dir
	Stage      string // "" → the Dockerfile's last stage; else `docker build --target <Stage>`
	// WorkshopPurpose is the fixed one-liner describing what this image is for,
	// when it is one of WorkshopSidecarImages — the workshop sidecar's
	// `inventory` tool reports it verbatim (internal/cmd/workshop/
	// tools_inventory.go's sidecarImages) so a builder reading the allowlist
	// sees what each admissible image is for without a second lookup. Empty
	// for every image outside that set; a WorkshopSidecarImages member must
	// set it non-empty (apimage_test.go's TestWorkshopSidecarImages_HavePurpose
	// asserts this).
	WorkshopPurpose string
}

// Version is the tag basis for every first-party image. Today there are no
// releases, so it is "dev" and local/remote builds carry the :dev tag. When we
// cut releases this becomes the release version (e.g. set via
// -ldflags "-X github.com/authzed/openagentprimitives/pkg/platform/apimage.Version=v1.2.3");
// LocalRef / RegistryRef / DigestRef all derive from it, so nothing else changes.
var Version = "dev"

// LocalRef is the local development image reference, <name>:<Version>.
func (i Image) LocalRef() string { return i.Name + ":" + Version }

// RegistryRef is the reference under registry reg: <reg>/<name>:<Version>.
func (i Image) RegistryRef(reg string) string {
	return strings.TrimSuffix(reg, "/") + "/" + i.LocalRef()
}

// DigestRef returns the canonical tag+digest reference under reg:
//
//	<reg>/<name>:<Version>@<digest>
//
// The tag rides along with the digest (a valid OCI "repo:tag@digest"): the
// digest is what Kubernetes resolves and what makes the reference immutable; the
// tag stays human-readable in `kubectl get`. digest must be a "sha256:…" string.
func (i Image) DigestRef(reg, digest string) string {
	return i.RegistryRef(reg) + "@" + digest
}

// The eleven first-party platform images. The nine Go services share one
// builder stage in Dockerfile.services and select their own final stage with
// --target; sandbox and the detector have Dockerfiles of their own.
var (
	Operator   = Image{Target: "operator", Name: "spicebox-operator", Dockerfile: "Dockerfile.services", Context: ".", Stage: "operator"}
	Sandbox    = Image{Target: "sandbox", Name: "spicebox-sandbox", Dockerfile: "Dockerfile.sandbox", Context: "."}
	Runner     = Image{Target: "runner", Name: "agentprimitives-runner", Dockerfile: "Dockerfile.services", Context: ".", Stage: "runner"}
	Channelsd  = Image{Target: "channelsd", Name: "agentprimitives-channelsd", Dockerfile: "Dockerfile.services", Context: ".", Stage: "channelsd"}
	Webd       = Image{Target: "webd", Name: "agentprimitives-webd", Dockerfile: "Dockerfile.services", Context: ".", Stage: "webd"}
	Authzd     = Image{Target: "authzd", Name: "agentprimitives-authzd", Dockerfile: "Dockerfile.services", Context: ".", Stage: "authzd"}
	Extractord = Image{Target: "extractord", Name: "agentprimitives-extractord", Dockerfile: "Dockerfile.services", Context: ".", Stage: "extractord"}
	Detector   = Image{Target: "promptinjection-detector", Name: "pi-detector", Dockerfile: "", Context: "images/promptinjection-detector"}
	// Workshop is the agent-builder sidecar (internal/cmd/workshop): a
	// per-session MCP server that runs ONLY inside a Ready Workshop's
	// provisioned namespace, sanctioned per (namespace, class) by
	// ClusterAgentSettings — never a standing Deployment, never started by a
	// plain `oap init`. Opt-in like Detector and APIAdapter (see AlwaysOnImages
	// below), not always-on like the control-plane services.
	Workshop = Image{Target: "workshop", Name: "ap-workshop", Dockerfile: "Dockerfile.services", Context: ".", Stage: "workshop",
		WorkshopPurpose: "the build space's own helper; not for an agent you are building"}
	// APIAdapter is the declarative HTTP→MCP adapter (internal/cmd/apiadapter):
	// a per-session sidecar that reads an API config from AP_SIDECAR_CONFIG and
	// serves one MCP tool per configured operation. Opt-in like Workshop and
	// Detector — pulled only for a session whose SidecarToolbox names it, never
	// a standing Deployment.
	//
	// Install-time digest pinning (Catalog, below) resolves and verifies
	// APIAdapter's digest same as every other first-party image, but plants it
	// nowhere — no manifest under config/ names this image — so a running
	// adapter's spec.source.image stays whatever tag the builder wrote, and
	// its immutability is instead the SidecarToolbox pin baseline's job
	// (pkg/controllers/sidecartoolbox's status.pin TOFU/drift check).
	APIAdapter = Image{Target: "apiadapter", Name: "ap-api-adapter", Dockerfile: "Dockerfile.services", Context: ".", Stage: "apiadapter",
		WorkshopPurpose: "turn a service's REST API into tools by writing a configuration — the adapter tier"}
	// Websearchd is the client-dispatched search+fetch sidecar
	// (internal/cmd/websearchd): a per-session sidecar exposing two MCP
	// tools — search and fetch — over a registry-selected websearch.Provider,
	// so a page's bytes pass through this platform's own controls
	// (untrusted-tagging, content guards, the byte budget) instead of a
	// model provider's own inline search, which bypasses all of them by
	// construction. Opt-in like Workshop, APIAdapter and Detector — pulled
	// only for a session whose SidecarToolbox names it, never a standing
	// Deployment.
	Websearchd = Image{Target: "websearchd", Name: "ap-websearchd", Dockerfile: "Dockerfile.services", Context: ".", Stage: "websearchd"}
)

// GoServices are the first-party images whose binaries are Go commands in this
// repo. Everything that needs to reason about "the Go services" as a set
// derives from this list, so adding a tenth service is a one-line change
// here rather than an edit to every consumer that enumerates them.
//
// They are built from the shared Go builder stage in Dockerfile.services — one
// `go build` for all nine, then a thin final stage each.
var GoServices = []Image{Operator, Runner, Channelsd, Webd, Authzd, Extractord, Workshop, APIAdapter, Websearchd}

// All is the canonical ordered list of every first-party image.
var All = []Image{Operator, Sandbox, Runner, Channelsd, Webd, Authzd, Extractord, Detector, Workshop, APIAdapter, Websearchd}

// WorkshopSidecarImages are the first-party images a WORKSHOP-authored
// SidecarToolbox may name in spec.source.image. It is the single source of
// truth for that set: the workshop admission webhook's allowlist enforces it
// and the workshop sidecar's `inventory` tool reports it, so what a builder is
// told it may use and what admission will accept cannot drift apart. Adding a
// third is a one-line change here.
var WorkshopSidecarImages = []Image{Workshop, APIAdapter}

// AlwaysOnImages returns the first-party images every running install needs
// regardless of which optional features are enabled — All minus the images
// that are pulled only when a user opts into a feature.
//
// Four opt-in first-party images exist: the prompt-injection Detector (off by
// default; started only when enabled via the settings wizard), the
// agent-builder Workshop sidecar (started only for a session whose class is
// sanctioned via ClusterAgentSettings — see pkg/controllers/agentsession/workshop_hook.go),
// the declarative HTTP→MCP APIAdapter sidecar, and the search+fetch
// Websearchd sidecar (both started only for a session whose SidecarToolbox
// names them). Every other image in All backs an always-present part of a
// session: the control plane (Operator/Runner/Channelsd/Webd/Authzd/Extractord)
// and Sandbox, the operator's --sandbox-image default used by any
// SpiceboxClass that does not pin spec.image.
//
// Any install profile that pre-provisions first-party images with no registry
// to fall back on — the desktop minimal-profile image bake, an air-gap mirror —
// MUST include ALL of these. Omitting one breaks the core session path with
// ErrImagePull the instant a session starts: drop Sandbox and every agent chat
// hangs on a sandbox pod pulling a tag that exists on no registry.
func AlwaysOnImages() []Image {
	optIn := map[string]bool{Detector.Name: true, Workshop.Name: true, APIAdapter.Name: true, Websearchd.Name: true} // see doc above
	out := make([]Image, 0, len(All))
	for _, im := range All {
		if optIn[im.Name] {
			continue
		}
		out = append(out, im)
	}
	return out
}

// ToolchainGo is the Go language-toolchain overlay (compiler + gopls). Toolchain
// images are first-party but are NOT in All: `oap build all` should not compile
// every language toolchain on a normal dev loop.
var ToolchainGo = Image{Target: "toolchain-go", Name: "ap-toolchain-go", Dockerfile: "", Context: "images/toolchain-go"}

// ToolchainNode is the Node.js language-toolchain overlay: node + npm/pnpm/yarn
// and the TypeScript language server.
var ToolchainNode = Image{Target: "toolchain-node", Name: "ap-toolchain-node", Dockerfile: "", Context: "images/toolchain-node"}

// ToolchainClaude is the Claude Code CLI overlay (the inner coding agent's own
// binary). Moved out of the base sandbox image so the shared base stays slim for
// the agents that never invoke claude; only codebot's codelike-bundle composes it.
//
// Unlike ToolchainGo/ToolchainNode, its context is the repo root, not its own
// images/ subdirectory: images/toolchain-claude/Dockerfile builds
// internal/cmd/claudeshim, the launcher that bridges AP-staged skills into
// Claude Code's discovery path before handing off to the real binary, and that
// shim imports pkg/tools/toolchain/claude — unreachable from a build context
// scoped to images/toolchain-claude/ alone. Root context + explicit Dockerfile
// mirrors Dockerfile.services/Dockerfile.sandbox (Operator, Sandbox, …
// above), and .dockerignore already trims it for exactly this shape of build.
var ToolchainClaude = Image{Target: "toolchain-claude", Name: "ap-toolchain-claude", Dockerfile: "images/toolchain-claude/Dockerfile", Context: "."}

// Toolchains are the first-party language/agent-toolchain overlay images.
var Toolchains = []Image{ToolchainGo, ToolchainNode, ToolchainClaude}

// BuilderStage describes a Dockerfile stage that is not itself a shipped image
// but is the shared parent several images COPY from. Materializing it once
// before those images build turns their builds into cache hits.
//
// It is deliberately NOT an Image: it has no Target, no Name, and no ref, so it
// can never be mistaken for something installable, land in Catalog(), or be
// planted into a manifest by the registry rewrite.
type BuilderStage struct {
	Dockerfile string
	Context    string
	Stage      string
}

// GoBuilder is the shared compile stage of Dockerfile.services: one `go build`
// producing every Go service binary.
//
// Each GoServices image is a thin final stage that copies out its own binary.
var GoBuilder = BuilderStage{Dockerfile: "Dockerfile.services", Context: ".", Stage: "builder"}

// UsesGoBuilder reports whether an image is a thin final stage over GoBuilder.
func UsesGoBuilder(dockerfile, stage string) bool {
	return dockerfile == GoBuilder.Dockerfile && stage != ""
}

// GoBuilderPrebuildMin is how many GoBuilder-derived images must be in a build
// set before materializing the shared stage as its own step is worth it. At two
// or more, skipping the prebuild means that many concurrent builds each start
// cold and race to produce the same builder vertex. At one, that single build
// produces the vertex itself and a prebuild is pure overhead.
const GoBuilderPrebuildMin = 2

// DesktopBakeImages is the first-party image set the desktop app bakes into its
// offline k3s image store. The regular app (dev=false) bakes only AlwaysOnImages
// — the control plane + slim base sandbox — because the long-term path pulls
// toolchain images from a registry on demand. A local-development build
// (dev=true, `mage desktop:devapp`) additionally bakes every Toolchains overlay:
// their `:dev` tags exist on no registry, so a codebot session that mounts one
// would otherwise ErrImagePull on an offline desktop.
func DesktopBakeImages(dev bool) []Image {
	out := AlwaysOnImages()
	if dev {
		out = append(out, Toolchains...)
	}
	return out
}

// Catalog is every first-party image: platform images plus toolchain overlays.
// Digest pinning, the registry rewrite, and `oap build` all range over this — a
// list that omitted Toolchains would ship them as unpinned, unqualified refs.
func Catalog() []Image {
	out := make([]Image, 0, len(All)+len(Toolchains))
	out = append(out, All...)
	return append(out, Toolchains...)
}

// SnapshotImage is the operator's workspace snapshot/restore helper image
// (the operator --snapshot-image default). It's a binary default not present
// in any manifest, so --mirror-dependencies mirrors it explicitly and the
// install injects --snapshot-image=<mirror> onto the operator.
const SnapshotImage = "busybox:1.36"

// DependencyImages are the public (non-first-party) images oap deploys for the
// core platform. --mirror-dependencies copies these under the target registry
// (flattened, see MirrorRef) for air-gapped clusters. The cloud-aware
// cert-manager / envoy-gateway images are intentionally excluded (separate
// optional install path). Keep this in sync with the bundle — the drift guard
// in pkg/platform/manifests fails if a new public image appears.
var DependencyImages = []string{
	"ghcr.io/authzed/spicedb:v1.56.2",
	"ghcr.io/authzed/spicedb-operator:v1.27.0",
	"nats:2.10-alpine",
	"pgvector/pgvector:pg17",
	"neo4j:5.26-community",
	"zepai/graphiti:latest",
	"rancher/local-path-provisioner:v0.0.32",
	"busybox",
	SnapshotImage,
}

// MirrorRef returns src mirrored under registry, with its repository path
// flattened (slashes → dashes, default docker.io host dropped) so it works on
// registries that reject nested paths (e.g. ECR). The tag is preserved.
//
//	authzed/spicedb:latest             → <reg>/authzed-spicedb:latest
//	docker.io/envoyproxy/ratelimit:abc → <reg>/envoyproxy-ratelimit:abc
//	busybox                            → <reg>/busybox
func MirrorRef(src, registry string) string {
	reg := strings.TrimSuffix(registry, "/")
	repo, tag := src, ""
	// A colon AFTER the last slash is a tag; a colon before is a registry port.
	if c := strings.LastIndex(src, ":"); c > strings.LastIndex(src, "/") {
		repo, tag = src[:c], src[c+1:]
	}
	repo = strings.TrimPrefix(repo, "docker.io/")
	out := reg + "/" + strings.ReplaceAll(repo, "/", "-")
	if tag != "" {
		out += ":" + tag
	}
	return out
}

// MirrorMap maps each DependencyImage to its MirrorRef under registry.
func MirrorMap(registry string) map[string]string {
	m := make(map[string]string, len(DependencyImages))
	for _, src := range DependencyImages {
		m[src] = MirrorRef(src, registry)
	}
	return m
}

// ResolveDigests maps each image's LocalRef to its final reference. Precedence,
// highest first: an explicit override (keyed by image Name) → a known digest
// (keyed by image Name, only when registry != "") → the registry ref (registry
// != "") → the unchanged LocalRef. This is the single place that decides
// override-vs-digest-vs-registry-vs-local for the manifest rewrite and the
// runner/sandbox/detector image args.
func ResolveDigests(registry string, digests, overrides map[string]string) map[string]string {
	cat := Catalog()
	m := make(map[string]string, len(cat))
	for _, im := range cat {
		final := im.LocalRef()
		if registry != "" {
			final = im.RegistryRef(registry)
			if d := digests[im.Name]; d != "" {
				final = im.DigestRef(registry, d)
			}
		}
		if ov := overrides[im.Name]; ov != "" {
			final = ov
		}
		m[im.LocalRef()] = final
	}
	return m
}
