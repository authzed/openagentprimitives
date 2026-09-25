// pkg/platform/settings/status_cel_guard_test.go
//
// The structural guard over the defect class resolve_toolguard_status_test.go
// covers one instance of. status.effectiveSettings mirrors AUTHORED types, so
// any validation rule added for the authored surface is also attached to a
// status path, where it governs writes derived from objects the apiserver never
// re-validates. A rule a stored object violates does not fail that object; it
// fails the operator's status write, wedging every reconcile in the namespace
// with an error naming a field on a different object.
//
// This test enumerates the CEL rules reaching a status path in the shipped CRDs
// and fails when a new one appears, so the next person to add one states why the
// producer guarantees it rather than discovering it in a cluster. It lives here
// because the guarantee is this package's: it produces status.effectiveSettings,
// so it owes the promise that what it stamps is admissible.
package settings

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// guaranteedStatusCELRules maps each schema path under `status` that carries an
// x-kubernetes-validations rule to the reason the value stamped there always
// satisfies it. Paths are dotted, with list items elided (see celRulePaths).
//
// An entry is a claim that this package CANNOT emit a violating value, and each
// must be backed by a test. Adding a rule to a status-reachable type without
// adding the guarantee is the bug this map exists to prevent.
var guaranteedStatusCELRules = map[string]string{
	"effectiveSettings.toolGuard.ceiling":                   "foldToolGuardCeiling sets MaxCalls and Window only together, from one CallRateBound (foldRateBounds); tierRateBounds drops and reports every half-authored tier bound. Covered by TestFoldToolGuardCeiling_KeepsEveryTiersBound.",
	"effectiveSettings.toolGuard.cluster.rules.rateLimit":   "admissibleToolGuardPolicy clears both halves of any rate pair that cannot enforce before mirroring, and reports it. Covered by TestEffectiveToolGuardDropsHalfAuthoredRateLimit.",
	"effectiveSettings.toolGuard.namespace.rules.rateLimit": "admissibleToolGuardPolicy clears both halves of any rate pair that cannot enforce before mirroring, and reports it. Covered by TestEffectiveToolGuardDropsHalfAuthoredRateLimit.",
}

// crdSchema is the sliver of a CRD manifest this test reads.
type crdSchema struct {
	Spec struct {
		Names struct {
			Kind string `json:"kind"`
		} `json:"names"`
		Versions []struct {
			Schema struct {
				OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// celRulePaths walks an OpenAPI schema and returns the dotted path of every
// subschema carrying x-kubernetes-validations. List items are elided rather
// than indexed ("rules.rateLimit", not "rules.items.rateLimit") so the path
// reads as the field path an admin would see in an apiserver error.
func celRulePaths(node map[string]any, prefix string) []string {
	var out []string
	if _, ok := node["x-kubernetes-validations"]; ok && prefix != "" {
		out = append(out, prefix)
	}
	join := func(seg string) string {
		if prefix == "" {
			return seg
		}
		return prefix + "." + seg
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, sub := range props {
			if s, ok := sub.(map[string]any); ok {
				out = append(out, celRulePaths(s, join(name))...)
			}
		}
	}
	// items / additionalProperties do not advance the path: a rule inside a
	// list or map is a rule on that field's elements.
	if items, ok := node["items"].(map[string]any); ok {
		out = append(out, celRulePaths(items, prefix)...)
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		out = append(out, celRulePaths(ap, prefix)...)
	}
	return out
}

// TestNoUnguardedStatusCELRules fails when a CEL validation rule reaches a
// status path without a recorded guarantee that the producer satisfies it.
func TestNoUnguardedStatusCELRules(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "config", "crds")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "reading the generated CRDs")

	found := map[string]string{} // path -> the kinds it appears on
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err, "reading %s", e.Name())

		var crd crdSchema
		require.NoError(t, yaml.Unmarshal(raw, &crd), "parsing %s", e.Name())
		for _, ver := range crd.Spec.Versions {
			props, ok := ver.Schema.OpenAPIV3Schema["properties"].(map[string]any)
			if !ok {
				continue
			}
			status, ok := props["status"].(map[string]any)
			if !ok {
				continue
			}
			for _, p := range celRulePaths(status, "") {
				found[p] = strings.TrimSpace(found[p] + " " + crd.Spec.Names.Kind)
			}
		}
	}
	require.NotEmpty(t, found,
		"no status CEL rules found at all — the walk is broken, or config/crds needs regenerating; "+
			"this test must never pass vacuously")

	var unguarded []string
	for path, kinds := range found {
		if _, ok := guaranteedStatusCELRules[path]; !ok {
			unguarded = append(unguarded, path+" (on "+kinds+")")
		}
	}
	sort.Strings(unguarded)
	assert.Empty(t, unguarded,
		"a CEL rule reaches a status path with no recorded guarantee that pkg/platform/settings satisfies it.\n"+
			"Such a rule also governs status writes derived from objects stored BEFORE it existed, which the "+
			"apiserver never re-validates — a stored object that violates it wedges every reconcile instead of "+
			"failing that object.\n"+
			"Either keep the rule off the type status mirrors, or make pkg/platform/settings guarantee the value it "+
			"stamps satisfies it and record that guarantee (with its test) in guaranteedStatusCELRules.")

	// The map is a claim about the shipped schema; a stale entry means a
	// guarantee is being maintained for a rule that no longer exists.
	var stale []string
	for path := range guaranteedStatusCELRules {
		if _, ok := found[path]; !ok {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "guaranteedStatusCELRules names paths the generated CRDs no longer carry; drop them")
}
