package builderbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// compiledViewPath is where `mage ui:compile` writes the workshop page
// compiled from src/ui/page.tsx. Bundle() splices it into the AgentUI
// manifest's spec.view, so the hand-written YAML keeps the actions table and
// the JSX keeps the page — one source of truth each, and the CR carries nodes
// only (spec §9).
const compiledViewPath = "src/ui/page.view.json"

// pageSourcePath is the page the compiled view is generated from, embedded
// alongside it so the Go side can tell whether the two still agree.
const pageSourcePath = "src/ui/page.tsx"

// compiledViewShaPath is the sidecar `mage ui:compile` writes beside the
// compiled view: the lowercase hex SHA-256 of the page source it compiled.
// The hash cannot ride inside the JSON — that is spliced whole into spec.view
// and strict-decoded at admission, so an extra key would be refused.
const compiledViewShaPath = "src/ui/page.view.sha256"

// checkCompiledViewFresh refuses a compiled view that no longer describes the
// page it was generated from. Nothing else on the Go side relates the two, so
// without it an edit to page.tsx with no `mage ui:compile` leaves every suite
// green and ships the previous page. Bundle() calls it, which puts the check
// in `mage test:unit` through every test that assembles the bundle.
func checkCompiledViewFresh(page, sha []byte) error {
	sum := sha256.Sum256(page)
	if strings.TrimSpace(string(sha)) != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%s is stale for %s: run mage ui:compile", compiledViewPath, pageSourcePath)
	}
	return nil
}

// spliceCompiledView sets spec.view on the one AgentUI manifest and returns
// every other document byte-identical. It refuses a manifest that already
// describes the page — spec.view or the legacy spec.slots — because two
// descriptions of one page cannot be reconciled by picking one (the same rule
// uicomponents.Normalize applies to the wire form).
func spliceCompiledView(manifest []byte, view json.RawMessage) ([]byte, error) {
	var obj map[string]any
	if err := yaml.Unmarshal(manifest, &obj); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if obj["kind"] != "AgentUI" {
		return manifest, nil
	}
	// sigs.k8s.io/yaml reads only the first document, and the re-marshal below
	// emits only what it parsed — so a second document in this file would
	// leave the bundle without a word. Every manifest under src/manifests is
	// single-doc; refusing here keeps it that way rather than dropping one.
	if bytes.Contains(manifest, []byte("\n---")) {
		return nil, errors.New("AgentUI manifest must be a single document; a second document would be dropped by the splice")
	}
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return nil, errors.New("AgentUI manifest has no spec to splice the compiled view into")
	}
	if _, has := spec["view"]; has {
		return nil, errors.New("AgentUI manifest already carries spec.view; the page is compiled from src/ui/page.tsx — remove one")
	}
	if _, has := spec["slots"]; has {
		return nil, errors.New("AgentUI manifest still carries spec.slots; the page is compiled from src/ui/page.tsx — remove the slots")
	}
	var v any
	if err := json.Unmarshal(view, &v); err != nil {
		return nil, fmt.Errorf("compiled view %s: %w", compiledViewPath, err)
	}
	spec["view"] = v
	out, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal spliced AgentUI manifest: %w", err)
	}
	return out, nil
}
