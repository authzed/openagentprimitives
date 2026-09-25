package installcmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// dockerfile_user_test.go guards that the long-lived service images run as a
// NUMERIC uid.
//
// Their Deployments set a pod-level runAsNonRoot:true with no runAsUser, so the
// kubelet must verify non-root from the image's USER alone — and it CANNOT
// verify a non-numeric user *name* ("nonroot"), refusing the container with
// CreateContainerConfigError. channelsd's Dockerfile once shipped `USER
// nonroot` while its siblings had already been migrated to `USER
// 65532:65532`, wedging channelsd on every fresh install. This class is
// invisible to the envtest suites (apiserver+etcd only — no kubelet ever
// schedules a real pod).
//
// runner and sandbox are intentionally exempt: the operator synthesizes their
// Pods with an explicit numeric runAsUser (1000) that overrides the image USER,
// and their workspace UID story is deliberately 1000. The detector image is
// exempt too (no fixed runAsNonRoot Deployment in the install bundle).
//
// This is an EXEMPTION set, not an inclusion set: TestServiceImagesUseNumericUser
// iterates apimage.All (the long-lived platform Deployments) and requires a
// numeric USER from everything NOT listed here, so a new Go service added to
// apimage.GoServices (a subset of apimage.All) defaults to covered rather than
// silently skipped by a lookup table nobody remembered to update. It is scoped
// to apimage.All rather than the full apimage.Catalog(): the toolchain overlay
// images (apimage.Toolchains) are composed into sandbox images, not deployed as
// their own runAsNonRoot Deployment, so the invariant this test guards doesn't
// apply to them at all.
var imagesExemptFromNumericUser = map[string]bool{
	apimage.Runner.Target:   true,
	apimage.Sandbox.Target:  true,
	apimage.Detector.Target: true,
}

var numericUserRe = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)

func TestServiceImagesUseNumericUser(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..") // cmd/oap/internal/installcmd -> repo root

	for _, tgt := range allBuildTargets {
		if imagesExemptFromNumericUser[tgt.name] {
			continue
		}
		require.NotEmptyf(t, tgt.dockerfile, "%s: expected an explicit Dockerfile", tgt.name)
		require.NotEmptyf(t, tgt.stage, "%s: expected an explicit --target stage", tgt.name)

		b, err := os.ReadFile(filepath.Join(repoRoot, tgt.dockerfile))
		require.NoErrorf(t, err, "read %s", tgt.dockerfile)

		user, ok := stageUserDirective(string(b), tgt.stage)
		require.Truef(t, ok,
			"%s (%s stage %q) declares no USER; a runAsNonRoot service image must set a numeric USER or it runs as root",
			tgt.name, tgt.dockerfile, tgt.stage)
		assert.Truef(t, numericUserRe.MatchString(user),
			"%s (%s stage %q) uses non-numeric USER %q; the kubelet cannot verify a named user against runAsNonRoot:true and rejects the pod with CreateContainerConfigError. Use a numeric uid, e.g. `USER 65532:65532`.",
			tgt.name, tgt.dockerfile, tgt.stage, user)
	}

	// Guard the guard: every exemption must still be a real build target, so a
	// rename can't silently leave a stale exemption that no longer excludes
	// anything (the renamed target would fall back into the required set
	// unnoticed rather than the exemption visibly failing here).
	names := make(map[string]bool, len(allBuildTargets))
	for _, tgt := range allBuildTargets {
		names[tgt.name] = true
	}
	for name := range imagesExemptFromNumericUser {
		assert.Truef(t, names[name],
			"imagesExemptFromNumericUser lists %q but no build target by that name exists in apimage.All", name)
	}
}

// TestGoServiceImagesCrossCompile guards that the shared Go builder stage
// cross-compiles to the requested arch (buildx --platform). Without GOARCH
// wired to $TARGETARCH, `docker buildx build --platform linux/amd64` on an
// arm64 host silently produces an arm64 binary the remote node can't run.
//
// It derives from apimage.GoServices rather than a hand-listed map: the map it
// replaced duplicated the catalog, so a new service could be added and go
// unguarded.
func TestGoServiceImagesCrossCompile(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	require.NotEmpty(t, apimage.GoServices, "the Go service set must not be empty")

	df := apimage.GoBuilder.Dockerfile
	stages := map[string]string{}
	for _, im := range apimage.GoServices {
		assert.Equalf(t, df, im.Dockerfile,
			"every Go service must build from the one shared Dockerfile so they share a builder stage; %q uses %q",
			im.Target, im.Dockerfile)
		assert.NotEmptyf(t, im.Stage, "%s must select a stage of %s", im.Target, df)
		// Distinctness lives here, alongside the Dockerfile-shape guards
		// (TestCatalogStagesExist, TestDockerfileServicesStructure) rather than
		// in pkg/platform/apimage's own test, so all three checks against the same
		// Dockerfile.services reality stay in one place. Two images selecting
		// the same --target would silently ship the same binary under two
		// image names.
		if prev, dup := stages[im.Stage]; dup {
			assert.Failf(t, "duplicate stage",
				"%q and %q both select stage %q", prev, im.Target, im.Stage)
		}
		stages[im.Stage] = im.Target
	}

	b, err := os.ReadFile(filepath.Join(repoRoot, df))
	require.NoErrorf(t, err, "read %s", df)
	s := string(b)
	assert.Contains(t, s, "--platform=$BUILDPLATFORM", "the shared builder must pin $BUILDPLATFORM")
	assert.Contains(t, s, "TARGETARCH", "the shared builder must declare/use TARGETARCH")
	assert.Contains(t, s, "GOARCH=$TARGETARCH", "the shared builder must build with GOARCH=$TARGETARCH")

	// The Go services are cluster components, so their main packages live under
	// internal/cmd/<name>. `go build -o /out/` still names each output after its
	// package DIRECTORY, so the emitted binary is /out/<name> either way — which
	// is why a wrong prefix here surfaces only as the opaque COPY failure below.
	for _, im := range apimage.GoServices {
		assert.Containsf(t, s, "./internal/cmd/"+im.Target,
			"the shared builder's `go build` must name ./internal/cmd/%s, or that image's COPY fails as an opaque `/out/%s: not found`",
			im.Target, im.Target)
	}
}

// TestCatalogStagesExist: every catalog image that selects a --target must name
// a stage its Dockerfile actually declares. A renamed stage would otherwise
// surface only as a docker build failure on whoever runs `oap build` next.
func TestCatalogStagesExist(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	for _, im := range apimage.Catalog() {
		if im.Stage == "" {
			continue
		}
		require.NotEmptyf(t, im.Dockerfile, "%s declares stage %q but no Dockerfile", im.Target, im.Stage)
		b, err := os.ReadFile(filepath.Join(repoRoot, im.Dockerfile))
		require.NoErrorf(t, err, "read %s", im.Dockerfile)
		assert.Regexpf(t, `(?mi)^FROM\s+.*\sAS\s+`+regexp.QuoteMeta(im.Stage)+`\s*$`, string(b),
			"%s selects --target %q but %s declares no such stage", im.Target, im.Stage, im.Dockerfile)
	}
}

// stageUserDirective returns the argument of the last USER instruction inside
// the named build stage — the one that applies when that stage is the build
// target. A file-wide "last USER" scan is wrong here: Dockerfile.services holds
// multiple final stages, so it would return whichever stage happens to be last
// and pass every image on one image's directive.
func stageUserDirective(dockerfile, stage string) (user string, found bool) {
	inStage := false
	for _, line := range strings.Split(dockerfile, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		if strings.EqualFold(f[0], "FROM") {
			// Any FROM ends the previous stage; re-enter only on `FROM … AS <stage>`.
			inStage = false
			for i := 1; i+1 < len(f); i++ {
				if strings.EqualFold(f[i], "AS") && strings.EqualFold(f[i+1], stage) {
					inStage = true
				}
			}
			continue
		}
		if inStage && strings.EqualFold(f[0], "USER") && len(f) >= 2 {
			user, found = f[1], true
		}
	}
	return user, found
}

// TestDockerfileServicesStructure pins what neither TestGoServiceImagesCrossCompile
// nor TestCatalogStagesExist already covers: that Dockerfile.services declares
// GoBuilder's own stage (GoBuilder is a BuilderStage, not a catalog image, so
// TestCatalogStagesExist never looks for it) and mounts the Go build cache (so
// the warm loop doesn't recompile from scratch). Per-service final-stage
// existence is TestCatalogStagesExist's job; the cross-compile flags and the
// go-build line naming every service are TestGoServiceImagesCrossCompile's job
// — both derive from the apimage catalog rather than a literal list, so
// duplicating their checks here would just be two places to keep in sync.
func TestDockerfileServicesStructure(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "Dockerfile.services"))
	require.NoError(t, err, "read Dockerfile.services")
	s := string(b)

	assert.Regexpf(t, `(?mi)^FROM\s+.*\sAS\s+`+regexp.QuoteMeta(apimage.GoBuilder.Stage)+`\s*$`, s,
		"Dockerfile.services must declare a %q stage", apimage.GoBuilder.Stage)
	assert.Contains(t, s, "target=/root/.cache/go-build",
		"the shared builder must mount the Go build cache, or the warm loop recompiles from scratch")
}
