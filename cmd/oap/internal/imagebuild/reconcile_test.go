package imagebuild

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

type fakeEnv struct {
	present  map[string]bool
	resolved map[string]string // ref -> resolvedRef reported by Resolve
	built    []string
	resolve  map[string]string // ref -> resolvedRef from Deliver
	confirm  bool
}

func (f *fakeEnv) Resolve(_ context.Context, ref string) (string, bool, error) {
	out := ref
	if r, ok := f.resolved[ref]; ok {
		out = r
	}
	return out, f.present[ref], nil
}
func (f *fakeEnv) Deliver(_ context.Context, _ oap.ImageBuild, _ Inputs, ref string) (string, error) {
	f.built = append(f.built, ref)
	if r, ok := f.resolve[ref]; ok {
		return r, nil
	}
	return ref, nil
}
func (f *fakeEnv) Confirm(string) bool           { return f.confirm }
func (f *fakeEnv) Secret(string) (string, error) { return "v", nil }
func (f *fakeEnv) Path(string) (string, error)   { return "/p", nil }

func img(ref string, build bool) oap.RequiredImage {
	ri := oap.RequiredImage{Ref: ref}
	if build {
		ri.Build = &oap.ImageBuild{Dockerfile: "d", Context: "c"}
	}
	return ri
}

func TestReconcilePresentSkipsBuild(t *testing.T) {
	var out bytes.Buffer
	env := &fakeEnv{present: map[string]bool{"a:dev": true}}
	got, err := Reconcile(context.Background(), &out, []oap.RequiredImage{img("a:dev", true)}, env, Options{BuildMissing: true})
	require.NoError(t, err)
	assert.Empty(t, env.built, "present image is never rebuilt")
	assert.Equal(t, map[string]string{"a:dev": "a:dev"}, got)
	assert.NotContains(t, out.String(), "--rebuild-images",
		"a local path re-loads cheaply every install, so it has no staleness to warn about")
}

func TestReconcileMissingBuildsAndRewrites(t *testing.T) {
	env := &fakeEnv{present: map[string]bool{}, resolve: map[string]string{"a:dev": "reg/a@sha256:1"}}
	got, err := Reconcile(context.Background(), io.Discard, []oap.RequiredImage{img("a:dev", true)}, env, Options{BuildMissing: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"a:dev"}, env.built)
	assert.Equal(t, "reg/a@sha256:1", got["a:dev"])
}

func TestReconcileMissingNoRecipeIsError(t *testing.T) {
	env := &fakeEnv{present: map[string]bool{}}
	_, err := Reconcile(context.Background(), io.Discard, []oap.RequiredImage{img("a:dev", false)}, env, Options{BuildMissing: true})
	assert.ErrorContains(t, err, "a:dev")
}

func TestReconcileNoBuildRefusesToBuild(t *testing.T) {
	env := &fakeEnv{present: map[string]bool{}}
	_, err := Reconcile(context.Background(), io.Discard, []oap.RequiredImage{img("a:dev", true)}, env, Options{NoBuild: true})
	assert.ErrorContains(t, err, "a:dev")
	assert.Empty(t, env.built)
}

func TestReconcileRebuildForcesEvenWhenPresent(t *testing.T) {
	env := &fakeEnv{present: map[string]bool{"a:dev": true}}
	_, err := Reconcile(context.Background(), io.Discard, []oap.RequiredImage{img("a:dev", true)}, env, Options{Rebuild: true, BuildMissing: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"a:dev"}, env.built)
}

// A remote install pushes to a mirrored ref, so a present image must be
// recorded under the MIRRORED ref — not the original. Recording the original
// would leave the AgentClass pointing at an image the cluster cannot pull, and
// only on the SECOND install (the first one builds and rewrites correctly).
func TestReconcilePresentRecordsResolvedRefNotOriginal(t *testing.T) {
	const orig = "demo-image:dev"
	const mirrored = "us-east1-docker.pkg.dev/my-proj/ap/demo-image:dev"
	env := &fakeEnv{
		present:  map[string]bool{orig: true},
		resolved: map[string]string{orig: mirrored},
	}
	var out bytes.Buffer
	got, err := Reconcile(context.Background(), &out, []oap.RequiredImage{img(orig, true)}, env, Options{BuildMissing: true})
	require.NoError(t, err)
	assert.Empty(t, env.built, "present image is never rebuilt")
	assert.Equal(t, map[string]string{orig: mirrored}, got)
	// Registry presence is a lookup of a mutable tag, so "present" does NOT
	// mean "current". Skipping the build is deliberate, but it must not be
	// silent: the way to override it has to appear where the skip is reported.
	assert.Contains(t, out.String(), "--rebuild-images",
		"reusing a mirrored tag must name the flag that forces a rebuild")
}
