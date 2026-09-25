package apimage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolchainsIncludeClaude(t *testing.T) {
	names := map[string]bool{}
	for _, im := range Toolchains {
		names[im.Name] = true
	}
	assert.Truef(t, names["ap-toolchain-claude"], "Toolchains must include the claude overlay")

	// Catalog() (used by digest-pinning + registry rewrite + `oap build`) must
	// carry it too, or it ships as an unpinned, unqualified ref.
	inCat := map[string]bool{}
	for _, im := range Catalog() {
		inCat[im.Name] = true
	}
	assert.Truef(t, inCat["ap-toolchain-claude"], "Catalog must include the claude overlay")
	assert.Equal(t, "toolchain-claude", ToolchainClaude.Target)
	// Unlike its toolchain siblings, ToolchainClaude's context is the repo
	// root: its Dockerfile builds internal/cmd/claudeshim, which imports
	// pkg/tools/toolchain/claude — unreachable from a context scoped to
	// images/toolchain-claude/ alone.
	assert.Equal(t, ".", ToolchainClaude.Context)
	assert.Equal(t, "images/toolchain-claude/Dockerfile", ToolchainClaude.Dockerfile)
}

func TestDesktopBakeImages(t *testing.T) {
	// Regular app: control plane + slim base, NO toolchains (registry-hosted long-term).
	prod := DesktopBakeImages(false)
	assert.ElementsMatch(t, AlwaysOnImages(), prod, "non-dev bake is exactly AlwaysOnImages")
	for _, im := range prod {
		assert.NotEqual(t, "ap-toolchain-claude", im.Name, "regular desktop bake must not carry toolchains")
	}
	// Dev app: AlwaysOnImages + every toolchain, so coder-bot works fully offline.
	dev := DesktopBakeImages(true)
	assert.Len(t, dev, len(AlwaysOnImages())+len(Toolchains))
	names := map[string]bool{}
	for _, im := range dev {
		names[im.Name] = true
	}
	for _, tc := range Toolchains {
		assert.Truef(t, names[tc.Name], "dev desktop bake must include %q", tc.Name)
	}
}

func TestRefs(t *testing.T) {
	assert.Equal(t, "agentprimitives-channelsd:dev", Channelsd.LocalRef())
	assert.Equal(t, "myreg.io/ap/agentprimitives-channelsd:dev", Channelsd.RegistryRef("myreg.io/ap"))
	assert.Equal(t, "myreg.io/ap/pi-detector:dev", Detector.RegistryRef("myreg.io/ap/"))
}

func TestAllEleven(t *testing.T) {
	assert.Len(t, All, 11)
	names := map[string]bool{}
	for _, im := range All {
		assert.NotEmpty(t, im.Name)
		assert.NotEmpty(t, im.Target)
		assert.NotEmpty(t, im.Context)
		names[im.Name] = true
	}
	for _, want := range []string{
		"spicebox-operator", "agentprimitives-runner", "agentprimitives-webd",
		"agentprimitives-authzd", "agentprimitives-channelsd", "spicebox-sandbox",
		"agentprimitives-extractord", "pi-detector", "ap-workshop", "ap-api-adapter",
		"ap-websearchd",
	} {
		assert.Truef(t, names[want], "All must include %q", want)
	}
}

// TestAlwaysOnImages guards the invariant that broke the desktop bundle: an
// offline image bake must include every first-party image a running install
// needs at session time — above all the Sandbox default. Its omission left the
// per-session sandbox pod stuck in ErrImagePull on spicebox-sandbox:dev.
func TestAlwaysOnImages(t *testing.T) {
	on := AlwaysOnImages()
	names := map[string]bool{}
	for _, im := range on {
		names[im.Name] = true
	}
	for _, want := range []Image{Operator, Sandbox, Runner, Channelsd, Webd, Authzd, Extractord} {
		assert.Truef(t, names[want.Name], "AlwaysOnImages must include %q (needed by every session)", want.Name)
	}
	// The prompt-injection detector, the agent-builder workshop sidecar, the
	// declarative HTTP→MCP api-adapter sidecar, and the websearch search+fetch
	// sidecar are all opt-in — pulled only when enabled/sanctioned/named by a
	// SidecarToolbox.
	optIn := map[string]bool{Detector.Name: true, Workshop.Name: true, APIAdapter.Name: true, Websearchd.Name: true}
	for name := range optIn {
		assert.Falsef(t, names[name], "AlwaysOnImages must exclude the opt-in %q", name)
	}
	// AlwaysOnImages is exactly All minus the opt-in set: every non-opt-in All
	// member is present, and every opt-in one is excluded — derived from the
	// opt-in set itself rather than a literal count, so a fifth opt-in image
	// doesn't silently stop being covered here.
	assert.Len(t, on, len(All)-len(optIn))
	for _, im := range All {
		if optIn[im.Name] {
			continue
		}
		assert.Truef(t, names[im.Name], "AlwaysOnImages must include non-opt-in %q", im.Name)
	}
}

func TestMirrorRef(t *testing.T) {
	assert.Equal(t, "myreg.io/ap/authzed-spicedb:latest", MirrorRef("authzed/spicedb:latest", "myreg.io/ap"))
	assert.Equal(t, "myreg.io/ap/pgvector-pgvector:pg17", MirrorRef("pgvector/pgvector:pg17", "myreg.io/ap/"))
	assert.Equal(t, "myreg.io/ap/envoyproxy-ratelimit:49af5cca", MirrorRef("docker.io/envoyproxy/ratelimit:49af5cca", "myreg.io/ap"))
	assert.Equal(t, "myreg.io/ap/busybox", MirrorRef("busybox", "myreg.io/ap"))
	// registry host with a port in the SOURCE is not mistaken for a tag boundary
	assert.Equal(t, "myreg.io/ap/foo:1.0", MirrorRef("foo:1.0", "myreg.io/ap"))
}

func TestMirrorMap(t *testing.T) {
	m := MirrorMap("myreg.io/ap")
	assert.Len(t, m, len(DependencyImages))
	assert.Equal(t, "myreg.io/ap/ghcr.io-authzed-spicedb:v1.56.2", m["ghcr.io/authzed/spicedb:v1.56.2"])
}

func TestResolveDigests_RegistryAndOverride(t *testing.T) {
	m := ResolveDigests("", nil, nil)
	assert.Equal(t, "spicebox-operator:dev", m["spicebox-operator:dev"])

	m = ResolveDigests("myreg.io/ap", nil, nil)
	assert.Equal(t, "myreg.io/ap/spicebox-operator:dev", m["spicebox-operator:dev"])
	assert.Equal(t, "myreg.io/ap/pi-detector:dev", m["pi-detector:dev"])

	m = ResolveDigests("myreg.io/ap", nil, map[string]string{"spicebox-operator": "ghcr.io/x/op:1.0"})
	assert.Equal(t, "ghcr.io/x/op:1.0", m["spicebox-operator:dev"])
	assert.Equal(t, "myreg.io/ap/agentprimitives-runner:dev", m["agentprimitives-runner:dev"])
}

func TestDigestRef(t *testing.T) {
	got := Runner.DigestRef("myreg.io/ap", "sha256:abc123")
	assert.Equal(t, "myreg.io/ap/agentprimitives-runner:dev@sha256:abc123", got)
}

func TestResolveDigests_Precedence(t *testing.T) {
	const reg = "myreg.io/ap"
	digests := map[string]string{Runner.Name: "sha256:dddd"}
	overrides := map[string]string{Operator.Name: "other.io/op@sha256:eeee"}

	m := ResolveDigests(reg, digests, overrides)

	// override wins over everything
	assert.Equal(t, "other.io/op@sha256:eeee", m[Operator.LocalRef()])
	// digest wins over the registry tag
	assert.Equal(t, "myreg.io/ap/agentprimitives-runner:dev@sha256:dddd", m[Runner.LocalRef()])
	// no digest, no override → registry tag
	assert.Equal(t, "myreg.io/ap/agentprimitives-webd:dev", m[Webd.LocalRef()])
}

func TestResolveDigests_LocalAndNoDigest(t *testing.T) {
	// registry == "" → local refs regardless of any digests passed
	m := ResolveDigests("", map[string]string{Runner.Name: "sha256:x"}, nil)
	assert.Equal(t, "agentprimitives-runner:dev", m[Runner.LocalRef()])
}

func TestVersion_DrivesRefs(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v9.9.9"
	assert.Equal(t, "spicebox-operator:v9.9.9", Operator.LocalRef())
	assert.Equal(t, "myreg.io/ap/spicebox-operator:v9.9.9@sha256:abc",
		Operator.DigestRef("myreg.io/ap", "sha256:abc"))
}

func TestCatalogIncludesToolchains(t *testing.T) {
	assert.Len(t, All, 11, "All stays the platform images only")
	assert.Len(t, Catalog(), len(All)+len(Toolchains))

	names := map[string]bool{}
	for _, im := range Catalog() {
		names[im.Name] = true
	}
	assert.True(t, names["ap-toolchain-go"], "Catalog must include the go toolchain overlay")
	assert.True(t, names["ap-toolchain-node"], "Catalog must include the node toolchain overlay")
}

// Ranges over Toolchains rather than naming one image: a toolchain that slipped
// out of the digest-pinned set would ship as a mutable tag, letting a warm node
// serve a different payload than the one that was reviewed.
func TestResolveDigests_PinsEveryToolchainImage(t *testing.T) {
	const reg = "myreg.io/ap"
	require.NotEmpty(t, Toolchains, "there must be toolchain images to pin")

	digests := map[string]string{}
	for _, im := range Toolchains {
		digests[im.Name] = "sha256:cccc"
	}
	m := ResolveDigests(reg, digests, nil)

	for _, im := range Toolchains {
		assert.Equalf(t, reg+"/"+im.Name+":dev@sha256:cccc", m[im.LocalRef()],
			"%s must be digest-pinned on a registry install", im.Name)
	}
}

// TestGoServices_AllInAll pins the Go service set: every entry is a real member
// of All.
//
// Stage distinctness is deliberately NOT asserted here: it lives in
// cmd/oap's TestGoServiceImagesCrossCompile, alongside the Dockerfile-shape
// guards (TestCatalogStagesExist, TestDockerfileServicesStructure) that can
// also confirm each named stage actually exists in Dockerfile.services —
// duplicating that check here would just be a second place to keep in sync.
func TestGoServices_AllInAll(t *testing.T) {
	require.NotEmpty(t, GoServices, "the Go service set must not be empty")

	inAll := make(map[string]bool, len(All))
	for _, im := range All {
		inAll[im.Target] = true
	}
	for _, im := range GoServices {
		assert.Truef(t, inAll[im.Target], "GoServices entry %q must also be in All", im.Target)
	}
}

// TestWorkshopSidecarImages_AllInAll pins the workshop-namable sidecar image
// set: every entry is a real member of All. This is the set the workshop
// admission webhook's allowlist and the workshop sidecar's `inventory` tool
// both derive from — a member missing from All would be an image nothing
// else in the catalog knows how to build, pin, or resolve a digest for.
func TestWorkshopSidecarImages_AllInAll(t *testing.T) {
	require.NotEmpty(t, WorkshopSidecarImages, "the workshop sidecar image set must not be empty")

	inAll := make(map[string]bool, len(All))
	for _, im := range All {
		inAll[im.Target] = true
	}
	for _, im := range WorkshopSidecarImages {
		assert.Truef(t, inAll[im.Target], "WorkshopSidecarImages entry %q must also be in All", im.Target)
	}
}

// TestWorkshopSidecarImages_HavePurpose confirms every workshop-namable
// sidecar image carries a non-empty WorkshopPurpose — the one-liner
// internal/cmd/workshop/tools_inventory.go's `inventory` tool reports
// verbatim. A third image added to WorkshopSidecarImages with no
// WorkshopPurpose set would report an empty Purpose to the builder there
// (tools_inventory_test.go's TestInventory_SidecarImages_ReportsAdmissibleRefs
// catches that from the tool-output side); this pins the invariant at its
// source, on the catalog itself.
func TestWorkshopSidecarImages_HavePurpose(t *testing.T) {
	require.NotEmpty(t, WorkshopSidecarImages, "the workshop sidecar image set must not be empty")

	for _, im := range WorkshopSidecarImages {
		assert.NotEmptyf(t, im.WorkshopPurpose, "%q must set WorkshopPurpose", im.Name)
	}
}

// TestWorkshopSidecarImages_OptIn confirms every workshop-namable sidecar
// image is opt-in — absent from AlwaysOnImages(). A workshop sidecar that got
// pre-pulled on every install would be a real regression: these images are
// pulled only for a session whose SidecarToolbox names them, per
// AlwaysOnImages' own doc.
func TestWorkshopSidecarImages_OptIn(t *testing.T) {
	require.NotEmpty(t, WorkshopSidecarImages, "the workshop sidecar image set must not be empty")

	alwaysOn := make(map[string]bool, len(All))
	for _, im := range AlwaysOnImages() {
		alwaysOn[im.Name] = true
	}
	for _, im := range WorkshopSidecarImages {
		assert.Falsef(t, alwaysOn[im.Name], "WorkshopSidecarImages entry %q must be opt-in, absent from AlwaysOnImages", im.Name)
	}
}

// TestUsesGoBuilder distinguishes the images that are thin final stages over
// the shared compile from every other image in the catalog. The prebuild
// decision rides on this, so a false positive would add a pointless docker exec
// and a false negative would drop the shared compile entirely.
func TestUsesGoBuilder(t *testing.T) {
	cases := []struct {
		name       string
		dockerfile string
		stage      string
		want       bool
	}{
		{name: "services Dockerfile with a stage: uses the shared builder", dockerfile: GoBuilder.Dockerfile, stage: "runner", want: true},
		{name: "services Dockerfile with no stage: not a service image", dockerfile: GoBuilder.Dockerfile, stage: "", want: false},
		{name: "sandbox Dockerfile: does not use the shared builder", dockerfile: Sandbox.Dockerfile, stage: "", want: false},
		{name: "inferred Dockerfile (detector): does not use the shared builder", dockerfile: "", stage: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, UsesGoBuilder(tc.dockerfile, tc.stage))
		})
	}
}
