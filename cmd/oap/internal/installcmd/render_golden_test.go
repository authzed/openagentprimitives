package installcmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// goldenOverlayFiles returns the SMALL, config-driven overlay files (the ones
// render actually composes) keyed by relative path. It deliberately excludes
// base/*.yaml and components/<dep>/<dep>.yaml — those are the vendored bundles
// (1.5 MB), drift-tested by TestInstallYAMLMatchesKustomize, and would bloat the
// repo if goldened here. The byte-identical re-render property of the base is
// covered by TestWriteOverlay_DeterministicAndBuildable.
func goldenOverlayFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		keep := rel == "kustomization.yaml" ||
			rel == "routing.yaml" ||
			rel == "bootstrap-job.yaml" ||
			rel == "webhook-cert.yaml" ||
			strings.HasPrefix(rel, "patches/") ||
			(strings.HasPrefix(rel, "components/") && filepath.Base(rel) == "kustomization.yaml")
		if !keep {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	}))
	return out
}

// TestRenderGoldens freezes the composed overlay files for representative
// configs. Regenerate with RENDER_UPDATE_GOLDEN=1 and READ THE DIFF — an
// unexplained change is the regression this guards.
func TestRenderGoldens(t *testing.T) {
	cases := []struct {
		name string
		cfg  RenderConfig
	}{
		{"local", RenderConfig{ClusterKind: cloud.KeyLocal, Strat: cloud.MustFor(cloud.KeyLocal)}},
		{"gke-postgres-routing", func() RenderConfig {
			c := RenderConfig{ClusterKind: cloud.KeyGKE, Strat: cloud.MustFor(cloud.KeyGKE), WithPostgres: true}
			c.Routing.trustedHostname = "webd.example.com"
			c.Routing.sandboxHostname = "sandbox.example.com"
			c.Routing.gatewayClass = "eg"
			return c
		}()},
	}
	update := os.Getenv("RENDER_UPDATE_GOLDEN") != ""
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.cfg.OutputDir = dir
			require.NoError(t, WriteOverlay(tc.cfg))
			got := goldenOverlayFiles(t, dir)

			goldenDir := filepath.Join("testdata", "render", tc.name)
			if update {
				require.NoError(t, os.RemoveAll(goldenDir))
				for rel, content := range got {
					p := filepath.Join(goldenDir, filepath.FromSlash(rel))
					require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
					require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
				}
				return
			}

			want := goldenOverlayFiles(t, goldenDir)
			gotKeys, wantKeys := keysOf(got), keysOf(want)
			require.Equal(t, wantKeys, gotKeys, "overlay file set changed (run RENDER_UPDATE_GOLDEN=1 and read the diff)")
			for rel, content := range got {
				assert.Equal(t, want[rel], content, "%s changed (run RENDER_UPDATE_GOLDEN=1 and read the diff)", rel)
			}
		})
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
