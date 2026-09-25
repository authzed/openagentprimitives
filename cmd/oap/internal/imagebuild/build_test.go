package imagebuild

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

func TestBuildArgsSecretRidesInEnvNotArgv(t *testing.T) {
	b := oap.ImageBuild{Dockerfile: "images/sandbox/Dockerfile", Context: "images/sandbox", Secrets: []string{"github_token"}}
	argv, env, err := BuildArgs(b, Inputs{Secrets: map[string]string{"github_token": "SECRETVALUE"}}, "sre-sandbox:dev")
	require.NoError(t, err)
	joined := strings.Join(argv, " ")
	assert.Contains(t, joined, "-f images/sandbox/Dockerfile")
	assert.Contains(t, joined, "-t sre-sandbox:dev")
	assert.Contains(t, joined, "--secret id=github_token,env=")
	assert.NotContains(t, joined, "SECRETVALUE", "secret value must never appear on argv")
	var found bool
	for _, e := range env {
		if strings.HasSuffix(e, "=SECRETVALUE") {
			found = true
		}
	}
	assert.True(t, found, "secret value delivered via env")
}

func TestBuildArgsBuildContextAndPlatform(t *testing.T) {
	b := oap.ImageBuild{Dockerfile: "images/dedicated-mcp/Dockerfile", Context: "images/dedicated-mcp", BuildContexts: []string{"dmcp"}}
	argv, _, err := BuildArgs(b, Inputs{BuildContexts: map[string]string{"dmcp": "/tmp/dmcp"}, Platform: "linux/arm64"}, "sre-dedicated-mcp:dev")
	require.NoError(t, err)
	joined := strings.Join(argv, " ")
	assert.Contains(t, joined, "--build-context dmcp=/tmp/dmcp")
	assert.Contains(t, joined, "--platform linux/arm64")
}

func TestBuildArgsMissingInputIsError(t *testing.T) {
	b := oap.ImageBuild{Dockerfile: "d", Context: "c", Secrets: []string{"github_token"}}
	_, _, err := BuildArgs(b, Inputs{}, "x:dev")
	assert.ErrorContains(t, err, "github_token")
}

func TestPushArgs(t *testing.T) {
	b := oap.ImageBuild{
		Dockerfile:    "images/app/Dockerfile",
		Context:       "images/app",
		Secrets:       []string{"npm_token"},
		BuildContexts: []string{"shared"},
		Args:          map[string]string{"VERSION": "1.2.3"},
	}
	in := Inputs{
		Secrets:       map[string]string{"npm_token": "s3cr3t"},
		BuildContexts: map[string]string{"shared": "/tmp/shared"},
		Platform:      "linux/arm64",
	}
	argv, env, err := PushArgs(b, in, "myreg.io/ap/demo-image:dev", "/tmp/meta.json")
	require.NoError(t, err)

	assert.Equal(t, []string{"buildx", "build"}, argv[:2], "push path must use buildx, not plain build")
	assert.Subset(t, argv, []string{"--platform", "linux/arm64"})
	assert.Subset(t, argv, []string{"--secret", "id=npm_token,env=AP_BUILD_SECRET_NPM_TOKEN"})
	assert.Subset(t, argv, []string{"--build-context", "shared=/tmp/shared"})
	assert.Subset(t, argv, []string{"--build-arg", "VERSION=1.2.3"})
	assert.Subset(t, argv, []string{"--metadata-file", "/tmp/meta.json"})
	assert.Contains(t, argv, "--push")
	assert.Equal(t, []string{"-t", "myreg.io/ap/demo-image:dev", "images/app"}, argv[len(argv)-3:],
		"the build context must be the final positional arg")
	assert.Equal(t, []string{"AP_BUILD_SECRET_NPM_TOKEN=s3cr3t"}, env,
		"secret values travel in the process env, never on argv")

	joined := strings.Join(argv, " ")
	assert.NotContains(t, joined, "s3cr3t", "a secret value must never reach argv")
}

func TestPushArgsMissingSecretIsError(t *testing.T) {
	b := oap.ImageBuild{Context: "c", Secrets: []string{"npm_token"}}
	_, _, err := PushArgs(b, Inputs{}, "myreg.io/ap/demo-image:dev", "")
	assert.ErrorContains(t, err, "npm_token")
}

// fakeRunner records what would have been handed to docker and, on the push
// path, writes the metadata file docker would have written so BuildAndPush can
// complete. This is the seam that lets a test observe the build inputs at all.
type fakeRunner struct {
	argv   []string
	dir    string
	env    []string
	digest string // written to --metadata-file when non-empty
	stderr string // emitted to cmd.Stderr before failing, when err != nil
	err    error
}

func (f *fakeRunner) Run(cmd *exec.Cmd) error {
	f.argv, f.dir, f.env = cmd.Args, cmd.Dir, cmd.Env
	if f.err != nil {
		if cmd.Stderr != nil {
			_, _ = io.WriteString(cmd.Stderr, f.stderr)
		}
		return f.err
	}
	if f.digest != "" {
		for i, a := range cmd.Args {
			if a == "--metadata-file" && i+1 < len(cmd.Args) {
				_ = os.WriteFile(cmd.Args[i+1], []byte(`{"containerimage.digest":"`+f.digest+`"}`), 0o600)
			}
		}
	}
	return nil
}

func pushRecipe() oap.ImageBuild {
	return oap.ImageBuild{Dockerfile: "images/app/Dockerfile", Context: "images/app"}
}

// The regression test for a real shipped bug: an arm64 image was pushed for
// amd64 nodes. It pushed and pulled cleanly and would have failed with
// "exec format error" at container start. Nothing caught it because the push
// shelled out to docker with no seam, so no test could see the platform.
func TestBuildAndPushSendsTheRequestedPlatformToDocker(t *testing.T) {
	r := &fakeRunner{digest: "sha256:abc"}
	_, err := BuildAndPush(context.Background(), r, io.Discard, t.TempDir(), pushRecipe(),
		Inputs{Platform: "linux/amd64"}, "myreg.io/ap/demo-image:dev")
	require.NoError(t, err)

	assert.Subset(t, r.argv, []string{"--platform", "linux/amd64"},
		"the push must carry the caller's platform, not the docker host's default")
	assert.Subset(t, r.argv, []string{"-t", "myreg.io/ap/demo-image:dev"})
	assert.Contains(t, r.argv, "--push")
	assert.Equal(t, "docker", r.argv[0])
}

func TestBuildAndPushReturnsTheDigestFromBuildxMetadata(t *testing.T) {
	r := &fakeRunner{digest: "sha256:deadbeef"}
	got, err := BuildAndPush(context.Background(), r, io.Discard, t.TempDir(), pushRecipe(),
		Inputs{Platform: "linux/amd64"}, "myreg.io/ap/demo-image:dev")
	require.NoError(t, err)
	assert.Equal(t, "sha256:deadbeef", got)
}

func TestBuildAndPushTranslatesAnAuthFailureIntoTheDockerLoginRemedy(t *testing.T) {
	r := &fakeRunner{err: errors.New("exit status 1"), stderr: "denied: permission_denied"}
	_, err := BuildAndPush(context.Background(), r, io.Discard, t.TempDir(), pushRecipe(),
		Inputs{Platform: "linux/amd64"}, "us-east1-docker.pkg.dev/my-proj/ap/demo-image:dev")
	require.Error(t, err)
	assert.ErrorContains(t, err, "gcloud auth configure-docker us-east1-docker.pkg.dev")
}

func TestBuildAndPushRunsInTheBundleDirWithBuildKitEnabled(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{digest: "sha256:abc"}
	_, err := BuildAndPush(context.Background(), r, io.Discard, dir, pushRecipe(),
		Inputs{Platform: "linux/amd64"}, "myreg.io/ap/demo-image:dev")
	require.NoError(t, err)
	assert.Equal(t, dir, r.dir, "relative dockerfile/context resolve against the bundle dir")
	assert.Contains(t, r.env, "DOCKER_BUILDKIT=1")
}

func TestBuildLocalRunsDockerBuildWithoutPushing(t *testing.T) {
	r := &fakeRunner{}
	require.NoError(t, BuildLocal(context.Background(), r, io.Discard, t.TempDir(), pushRecipe(),
		Inputs{}, "demo-image:dev"))
	assert.Equal(t, []string{"docker", "build"}, r.argv[:2])
	assert.NotContains(t, r.argv, "--push", "the local path must never push")
}

// TestBuildFlags_RejectsBundleEscapingPaths pins the confinement of the two
// manifest-supplied paths. `context` is appended as docker's final positional
// argument and `dockerfile` as `-f`, both verbatim, while cmd.Dir is only the
// bundle root — which bounds nothing. The sole validation anywhere else is a
// non-empty check in pkg/platform/oap. So an absolute or "../"-laden value in an
// untrusted bundle hands docker a build context outside the bundle, which the
// bundle's own Dockerfile RUN steps then read. Exposure is not limited to
// folder sources: for a packed .oap the caller passes bundleDir == "", so
// cmd.Dir is the installing user's CWD.
//
// Named --build-context values are deliberately NOT covered: those come from
// the installing user's own --build-context flags, not the bundle, and are
// legitimately absolute.
func TestBuildFlags_RejectsBundleEscapingPaths(t *testing.T) {
	cases := []struct {
		name  string
		build oap.ImageBuild
		want  string
	}{
		{name: "absolute context: rejected", build: oap.ImageBuild{Context: "/etc"}, want: "context"},
		{name: "parent-traversing context: rejected", build: oap.ImageBuild{Context: "../../../etc"}, want: "context"},
		{name: "context escaping after cleaning: rejected", build: oap.ImageBuild{Context: "images/../.."}, want: "context"},
		{name: "absolute dockerfile: rejected", build: oap.ImageBuild{Context: ".", Dockerfile: "/etc/passwd"}, want: "dockerfile"},
		{name: "parent-traversing dockerfile: rejected", build: oap.ImageBuild{Context: ".", Dockerfile: "../Dockerfile"}, want: "dockerfile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, buildErr := BuildArgs(tc.build, Inputs{}, "x:dev")
			assert.ErrorContains(t, buildErr, tc.want, "BuildArgs must fail closed")

			_, _, pushErr := PushArgs(tc.build, Inputs{}, "reg/x:dev", "")
			assert.ErrorContains(t, pushErr, tc.want, "PushArgs shares the same choke point")
		})
	}

	t.Run("in-bundle relative paths: accepted", func(t *testing.T) {
		b := oap.ImageBuild{Context: "images/app", Dockerfile: "images/app/Dockerfile"}
		argv, _, err := BuildArgs(b, Inputs{}, "x:dev")
		require.NoError(t, err)
		assert.Equal(t, "images/app", argv[len(argv)-1], "the context stays the final positional arg")
	})

	t.Run("empty context: accepted as the bundle root", func(t *testing.T) {
		argv, _, err := BuildArgs(oap.ImageBuild{}, Inputs{}, "x:dev")
		require.NoError(t, err)
		assert.Equal(t, ".", argv[len(argv)-1])
	})
}
