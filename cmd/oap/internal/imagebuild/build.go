// Package imagebuild builds a bundle's declared images and delivers them to
// the current cluster (local node/VM load, or a registry push).
package imagebuild

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/buildx"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

type Inputs struct {
	Secrets       map[string]string
	BuildContexts map[string]string
	Platform      string
}

// envVarFor derives a stable, shell-safe env var name for a recipe secret.
func envVarFor(secretName string) string {
	up := strings.ToUpper(secretName)
	safe := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, up)
	return "AP_BUILD_SECRET_" + safe
}

// ctxDir resolves a recipe's build context, defaulting to the bundle root.
// Callers must have passed the recipe through checkBundlePaths first.
func ctxDir(b oap.ImageBuild) string {
	if b.Context == "" {
		return "."
	}
	return b.Context
}

// checkBundlePaths refuses a recipe whose `context` or `dockerfile` reaches
// outside the bundle.
//
// Both values come from the bundle's own manifest and are handed to docker
// verbatim — the context as the final positional argument, the dockerfile as
// `-f`. The build runs with cmd.Dir set to the bundle root, but cmd.Dir bounds
// nothing: an absolute path ignores it, and a "../"-laden one climbs out of
// it. The only other validation these fields get anywhere is a non-empty check
// in pkg/platform/oap. So without this an untrusted bundle can name any directory on
// the installing machine as its build context and read it from its own
// Dockerfile RUN steps.
//
// This is worst for the *packed* .oap — the untrusted-distribution form —
// because the caller can only set bundleDir for a folder source, leaving
// cmd.Dir as the installing user's CWD.
//
// Named --build-context values are not checked here: those come from the
// installing user's own flags, not the bundle, and are legitimately absolute.
func checkBundlePaths(b oap.ImageBuild, tag string) error {
	for _, p := range []struct{ field, value string }{
		{"context", b.Context},
		{"dockerfile", b.Dockerfile},
	} {
		if p.value == "" {
			continue
		}
		if filepath.IsAbs(p.value) {
			return fmt.Errorf("build %s: %s %q must be a path inside the bundle, not an absolute path", tag, p.field, p.value)
		}
		if rel := filepath.Clean(p.value); rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("build %s: %s %q resolves outside the bundle", tag, p.field, p.value)
		}
	}
	return nil
}

// buildFlags returns the recipe-derived flags shared by the local-build and
// push argvs, plus the extra process env carrying secret values. BuildKit reads
// each secret from its env var via --secret id=<name>,env=<VAR>, so the value
// never lands on argv.
func buildFlags(b oap.ImageBuild, in Inputs, tag string) (flags []string, env []string, err error) {
	// The single choke point both BuildArgs and PushArgs pass through, so the
	// bundle-path confinement cannot be reached around by adding a third argv
	// builder.
	if err := checkBundlePaths(b, tag); err != nil {
		return nil, nil, err
	}
	if b.Dockerfile != "" {
		flags = append(flags, "-f", b.Dockerfile)
	}
	if in.Platform != "" {
		flags = append(flags, "--platform", in.Platform)
	}
	for _, name := range b.Secrets {
		val, ok := in.Secrets[name]
		if !ok {
			return nil, nil, fmt.Errorf("build %s: required build secret %q not supplied", tag, name)
		}
		ev := envVarFor(name)
		env = append(env, ev+"="+val)
		flags = append(flags, "--secret", "id="+name+",env="+ev)
	}
	for _, name := range b.BuildContexts {
		path, ok := in.BuildContexts[name]
		if !ok {
			return nil, nil, fmt.Errorf("build %s: required build-context %q not supplied", tag, name)
		}
		flags = append(flags, "--build-context", name+"="+path)
	}
	keys := make([]string, 0, len(b.Args))
	for k := range b.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		flags = append(flags, "--build-arg", k+"="+b.Args[k])
	}
	return flags, env, nil
}

// BuildArgs constructs the local `docker build` argv and the extra process env
// (secret values only).
func BuildArgs(b oap.ImageBuild, in Inputs, tag string) (argv []string, env []string, err error) {
	flags, env, err := buildFlags(b, in, tag)
	if err != nil {
		return nil, nil, err
	}
	argv = append([]string{"build"}, flags...)
	argv = append(argv, "-t", tag, ctxDir(b))
	return argv, env, nil
}

// PushArgs constructs the `docker buildx build --push` argv for a recipe,
// targeting ref. It shares buildFlags with the local path so recipe inputs have
// one definition, and keeps the build context as the final positional argument.
func PushArgs(b oap.ImageBuild, in Inputs, ref, metadataFile string) (argv []string, env []string, err error) {
	flags, env, err := buildFlags(b, in, ref)
	if err != nil {
		return nil, nil, err
	}
	argv = append([]string{"buildx", "build"}, flags...)
	if metadataFile != "" {
		argv = append(argv, "--metadata-file", metadataFile)
	}
	argv = append(argv, "--push", "-t", ref, ctxDir(b))
	return argv, env, nil
}

// Runner executes a prepared docker command.
//
// This is the seam that makes the build paths testable. Without it these
// functions shell out to a real `docker`, so nothing could observe what was
// actually asked of it — and an image built for the wrong architecture pushed,
// pulled, and only failed at container start, far from the command that caused
// it. Production uses ExecRunner; tests substitute a fake that records the argv.
type Runner interface {
	Run(cmd *exec.Cmd) error
}

// ExecRunner runs the command for real.
type ExecRunner struct{}

func (ExecRunner) Run(cmd *exec.Cmd) error { return cmd.Run() }

// BuildLocal runs the local docker build (BuildKit enabled) for a recipe,
// resolving relative dockerfile/context against bundleDir.
func BuildLocal(ctx context.Context, r Runner, out io.Writer, bundleDir string, b oap.ImageBuild, in Inputs, tag string) error {
	argv, extraEnv, err := BuildArgs(b, in, tag)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "==> docker build -t %s (%s)\n", tag, b.Context)
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Dir = bundleDir
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := r.Run(cmd); err != nil {
		return fmt.Errorf("docker build %s: %w", tag, err)
	}
	return nil
}

// BuildAndPush cross-builds a recipe and pushes it to ref, returning the pushed
// digest.
//
// The caller pins the ref it records to this digest. That is safe for
// server-side apply precisely because this function only runs when the image
// was ABSENT: an unchanged re-install never gets here, short-circuiting instead
// at the presence probe, which re-derives the same digest from the registry. So
// the applied ref is stable across re-installs and changes only when the image
// content does.
func BuildAndPush(ctx context.Context, r Runner, out io.Writer, bundleDir string, b oap.ImageBuild, in Inputs, ref string) (string, error) {
	mf, err := os.CreateTemp("", "ap-oap-buildx-meta-*.json")
	if err != nil {
		return "", fmt.Errorf("create buildx metadata file: %w", err)
	}
	mfPath := mf.Name()
	_ = mf.Close()
	defer os.Remove(mfPath)

	argv, extraEnv, err := PushArgs(b, in, ref, mfPath)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "==> docker buildx build --push -t %s (%s)\n", ref, ctxDir(b))

	// Each attempt gets a fresh stderr capture: classification must read the
	// failure that ended THIS attempt, not one a previous attempt survived.
	push := func(ctx context.Context) (string, error) {
		var translate bytes.Buffer
		cmd := exec.CommandContext(ctx, "docker", argv...)
		cmd.Dir = bundleDir
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
		cmd.Env = append(cmd.Env, extraEnv...)
		cmd.Stdout = out
		cmd.Stderr = io.MultiWriter(out, &translate) // tee, so auth/repo failures can be translated
		err := r.Run(cmd)                            // must complete before the capture is read
		return translate.String(), err
	}
	if err := buildx.RunPush(ctx, out, ref, registryOf(ref), buildx.Retry{}, push); err != nil {
		return "", err
	}

	meta, err := os.ReadFile(mfPath)
	if err != nil {
		return "", fmt.Errorf("read buildx metadata for %s: %w", ref, err)
	}
	digest, err := buildx.MetadataDigest(meta)
	if err != nil {
		return "", fmt.Errorf("resolve pushed digest for %s: %w", ref, err)
	}
	return digest, nil
}

// registryOf returns the registry portion of a pushed ref, for error messages.
func registryOf(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[:i]
	}
	return ref
}
