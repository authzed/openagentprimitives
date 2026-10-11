// Package browsertoolbox implements the contract.Kind plug-in for the
// BrowserToolbox CR: CLI presentation only (Row, Detail, ValidateFile),
// blank-imported by cmd/oap. Host validation defers to
// pkg/tools/browserpolicy.CanonicalHost so the authoring-time check never
// drifts from the egress gate's own notion of a well-formed host.
package browsertoolbox

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

// Valid Interaction values. There is no shared const for these in
// spiceboxv1alpha1 (the CRD states them only as a kubebuilder Enum marker on
// BrowserToolboxSpec.Interaction), so they are mirrored here as string
// literals that must stay in sync with that marker.
const (
	interactionAct      = "act"
	interactionReadonly = "readonly"
)

type Kind struct{}

func New() *Kind { return &Kind{} }

func (Kind) Name() string { return "BrowserToolbox" }

func (Kind) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    spiceboxv1alpha1.GroupName,
		Version:  spiceboxv1alpha1.SchemeGroupVersion.Version,
		Resource: "browsertoolboxes",
	}
}

func (Kind) NewObject() client.Object   { return &spiceboxv1alpha1.BrowserToolbox{} }
func (Kind) NewList() client.ObjectList { return &spiceboxv1alpha1.BrowserToolboxList{} }

func (Kind) DecodeYAML(doc []byte) (client.Object, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(doc, &probe); err != nil {
		return nil, fmt.Errorf("decode kind: %w", err)
	}
	if probe.Kind != "BrowserToolbox" {
		return nil, nil
	}
	var out spiceboxv1alpha1.BrowserToolbox
	if err := yaml.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("decode BrowserToolbox: %w", err)
	}
	return &out, nil
}

func (Kind) Row(obj client.Object) contract.Row {
	cr := obj.(*spiceboxv1alpha1.BrowserToolbox)
	return contract.Row{
		Kind:      "BrowserToolbox",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Status:    fmt.Sprintf("Mode=%s Interaction=%s", cr.Spec.Access.Mode, cr.Spec.Interaction),
		Summary:   fmt.Sprintf("%d site(s)", len(cr.Spec.Access.Sites)),
	}
}

func (Kind) Detail(obj client.Object) contract.Detail {
	cr := obj.(*spiceboxv1alpha1.BrowserToolbox)
	return contract.Detail{
		Kind:      "BrowserToolbox",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Sections: []contract.Section{
			{Title: "Access", Body: renderAccess(cr.Spec.Access)},
			{Title: "Interaction", Body: cr.Spec.Interaction},
			{Title: "State", Body: cr.Spec.State},
		},
	}
}

// MatchFlat: top-level access.mode present, and no MCPServer-shaped
// server.url or SidecarToolbox-shaped source.
func (Kind) MatchFlat(data []byte) bool {
	var probe struct {
		Access *struct {
			Mode string `yaml:"mode"`
		} `yaml:"access"`
		Server *struct {
			URL string `yaml:"url"`
		} `yaml:"server"`
		Source map[string]any `yaml:"source"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	if probe.Server != nil && probe.Server.URL != "" {
		return false
	}
	if probe.Source != nil {
		return false
	}
	return probe.Access != nil && probe.Access.Mode != ""
}

func (k Kind) ValidateFile(path string) ([]contract.Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sp spiceboxv1alpha1.BrowserToolboxSpec
	if err := yaml.Unmarshal(data, &sp); err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: path, Message: err.Error()}}, nil
	}

	var diags []contract.Diagnostic

	if sp.Access.Mode != browserpolicy.ModeAllowlist && sp.Access.Mode != browserpolicy.ModeBlocklist {
		diags = append(diags, contract.Diagnostic{
			Severity: "error",
			Path:     "access.mode",
			Message:  fmt.Sprintf("access.mode %q is not valid (want %q or %q)", sp.Access.Mode, browserpolicy.ModeAllowlist, browserpolicy.ModeBlocklist),
		})
	}

	// Canonicalize every site's host via the SAME browserpolicy.CanonicalHost
	// the runtime egress gate uses, so an authoring-time-valid host can never
	// turn out to be rejected (or silently mismatched) at enforcement time.
	// The canonical form also dedupes entries that differ only in case or a
	// trailing dot.
	seen := make(map[string]int, len(sp.Access.Sites))
	for i, site := range sp.Access.Sites {
		sitePath := fmt.Sprintf("access.sites[%d].host", i)
		canon, err := browserpolicy.CanonicalHost(site.Host)
		if err != nil {
			diags = append(diags, contract.Diagnostic{Severity: "error", Path: sitePath, Message: err.Error()})
			continue
		}
		if firstIdx, dup := seen[canon]; dup {
			diags = append(diags, contract.Diagnostic{
				Severity: "error",
				Path:     sitePath,
				Message:  fmt.Sprintf("duplicate host %q (already listed at access.sites[%d].host)", canon, firstIdx),
			})
			continue
		}
		seen[canon] = i
	}

	// Mirror the two field-level XValidation rules on BrowserToolbox.Spec
	// (pkg/apis/v1alpha1/browsertoolbox_types.go) at authoring time, so an
	// author sees the same rejection `oap tools validate` would from the
	// apiserver, without a round trip through admission.
	if sp.Access.Mode == browserpolicy.ModeBlocklist && sp.Access.Identity != "" {
		diags = append(diags, contract.Diagnostic{
			Severity: "error",
			Path:     "access.identity",
			Message:  "a blocklist (general-access) browser cannot carry an identity or identitySites",
		})
	}
	if sp.Access.Mode == browserpolicy.ModeBlocklist && sp.Access.IdentitySites {
		diags = append(diags, contract.Diagnostic{
			Severity: "error",
			Path:     "access.identitySites",
			Message:  "a blocklist (general-access) browser cannot carry an identity or identitySites",
		})
	}
	if sp.Access.IdentitySites && sp.Access.Identity == "" {
		diags = append(diags, contract.Diagnostic{
			Severity: "error",
			Path:     "access.identitySites",
			Message:  "identitySites requires identity",
		})
	}

	// An allowlist with no sites denies every host — not a structural error,
	// but almost certainly not what the author intended, so it's a warning
	// rather than an error.
	if sp.Access.Mode == browserpolicy.ModeAllowlist && len(sp.Access.Sites) == 0 {
		diags = append(diags, contract.Diagnostic{
			Severity: "warning",
			Path:     "access.sites",
			Message:  "allowlist with no sites denies all browsing",
		})
	}

	if sp.Interaction != "" && sp.Interaction != interactionAct && sp.Interaction != interactionReadonly {
		diags = append(diags, contract.Diagnostic{
			Severity: "error",
			Path:     "interaction",
			Message:  fmt.Sprintf("interaction %q is not valid (want %q or %q)", sp.Interaction, interactionAct, interactionReadonly),
		})
	}

	return diags, nil
}

func (k Kind) LintFile(path string) ([]contract.Diagnostic, error) { return k.ValidateFile(path) }

// renderAccess renders Mode and the site list, marking Exact entries so
// `oap tools get` surfaces the subdomain-matching behavior Sites implies
// (see BrowserAccess's doc comment).
func renderAccess(a spiceboxv1alpha1.BrowserAccess) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode: %s\n", a.Mode)
	if a.Identity != "" {
		fmt.Fprintf(&b, "identity: %s\n", a.Identity)
	}
	if a.IdentitySites {
		b.WriteString("identitySites: true\n")
	}
	if len(a.Sites) == 0 {
		b.WriteString("sites: (none)")
		return b.String()
	}
	b.WriteString("sites:")
	for _, s := range a.Sites {
		b.WriteString("\n  - " + s.Host)
		if s.Exact {
			b.WriteString(" (exact)")
		}
	}
	return b.String()
}
