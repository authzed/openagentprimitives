package installcmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// dependencyComponent describes one optional, opt-in dependency bundle the render
// can include as a kustomize Component.
type dependencyComponent struct {
	name    string
	enabled func(RenderConfig) bool
	load    func() ([][]byte, error)
}

// dependencyComponents is the fixed, ordered set of optional dependencies. Order
// is fixed so the emitted `components:` list (and the on-disk layout) is
// deterministic across renders.
//
// Postgres only, by design: its token is Job-minted, so --with-postgres renders
// a bundle that works end to end. Graphiti/Neo4j are omitted until a follow-up
// adds the external-LLM-key story — graphiti needs a user-supplied
// spicebox-graphiti-config (OpenAI key) the overlay cannot mint, which render's
// "nothing external by default" contract has no place for yet. (The install path
// still supports them; see install.go / manifests.Graphiti/Neo4j.)
var dependencyComponents = []dependencyComponent{
	{"postgres", func(c RenderConfig) bool { return c.WithPostgres }, manifests.Postgres},
}

// enabledComponentNames returns the enabled dependency names in fixed order. Used
// by both the writer and the kustomization emitter so they cannot disagree.
func enabledComponentNames(cfg RenderConfig) []string {
	var names []string
	for _, d := range dependencyComponents {
		if d.enabled(cfg) {
			names = append(names, d.name)
		}
	}
	return names
}

// writeDependencyComponents writes a kustomize Component directory per enabled
// dependency, with the storage class injected into each stateful doc (a no-op
// for non-PVC/StatefulSet kinds). Creates components/ only when at least one
// dependency is enabled.
func writeDependencyComponents(cfg RenderConfig) error {
	for _, d := range dependencyComponents {
		if !d.enabled(cfg) {
			continue
		}
		docs, err := d.load()
		if err != nil {
			return fmt.Errorf("load %s component: %w", d.name, err)
		}
		if cfg.StorageClass != "" {
			for i := range docs {
				m, err := manifests.InjectStorageClass(docs[i], cfg.StorageClass)
				if err != nil {
					return fmt.Errorf("inject storageClassName=%q into %s: %w", cfg.StorageClass, d.name, err)
				}
				docs[i] = m
			}
		}
		compDir := filepath.Join(cfg.OutputDir, "components", d.name)
		if err := os.MkdirAll(compDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(compDir, d.name+".yaml"), bytes.Join(docs, []byte("---\n")), 0o644); err != nil {
			return err
		}
		comp := "apiVersion: kustomize.config.k8s.io/v1alpha1\n" +
			"kind: Component\n" +
			"resources:\n" +
			"  - " + d.name + ".yaml\n"
		if err := os.WriteFile(filepath.Join(compDir, "kustomization.yaml"), []byte(comp), 0o644); err != nil {
			return err
		}
	}
	return nil
}
