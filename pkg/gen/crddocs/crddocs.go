// Package crddocs generates the showcase docs' CRD reference: one MDX page per
// OAP CustomResourceDefinition, from the generated CRD YAML schemas (which carry
// field descriptions from the Go doc comments, plus types/enums/defaults). A
// change to a *_types.go field surfaces — after `mage gen:api` — as a docs diff,
// the same no-drift discipline as clidocs and blockcapture.
package crddocs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/gen/mdxutil"
)

// The CRD reference is the "CRD reference" group of the docs' Reference section,
// ordered after the CLI group (which ends below 3000).
const (
	section       = "Reference"
	group         = "CRD reference"
	overviewOrder = 3000
	crdStep       = 10
	crdBase       = 3010
	maxDepth      = 8
)

type crdInfo struct {
	kind, singular, apiGroup, scope, desc string
	shortNames                            []string
	spec, status                          *apiextensionsv1.JSONSchemaProps
}

// Generate reads every CRD YAML in crdDir and writes crd-reference.mdx (an
// overview) plus crd-<singular>.mdx per kind, into outDir. Deterministic: kinds
// and fields are sorted, so a re-run over unchanged CRDs is a no-op diff.
func Generate(crdDir, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(crdDir)
	if err != nil {
		return err
	}
	var crds []crdInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		info, ok, err := parseCRD(filepath.Join(crdDir, e.Name()))
		if err != nil {
			return fmt.Errorf("crddocs: %s: %w", e.Name(), err)
		}
		if ok {
			crds = append(crds, info)
		}
	}
	if len(crds) == 0 {
		return fmt.Errorf("crddocs: no CRDs found in %s", crdDir)
	}
	sort.Slice(crds, func(i, j int) bool { return crds[i].kind < crds[j].kind })

	if err := os.WriteFile(filepath.Join(outDir, "crd-reference.mdx"), []byte(overviewPage(crds)), 0o644); err != nil {
		return err
	}
	for i, c := range crds {
		name := "crd-" + c.singular + ".mdx"
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(crdPage(c, crdBase+i*crdStep)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func parseCRD(path string) (crdInfo, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return crdInfo{}, false, err
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return crdInfo{}, false, err
	}
	if crd.Kind != "CustomResourceDefinition" || crd.Spec.Names.Kind == "" {
		return crdInfo{}, false, nil
	}
	// Prefer the storage version's schema; fall back to the first served one.
	var schema *apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		if v.Storage {
			schema = v.Schema.OpenAPIV3Schema
			break
		}
		if schema == nil {
			schema = v.Schema.OpenAPIV3Schema
		}
	}
	info := crdInfo{
		kind:       crd.Spec.Names.Kind,
		singular:   crd.Spec.Names.Singular,
		apiGroup:   crd.Spec.Group,
		scope:      string(crd.Spec.Scope),
		shortNames: crd.Spec.Names.ShortNames,
	}
	if info.singular == "" {
		info.singular = strings.ToLower(info.kind)
	}
	if schema != nil {
		info.desc = schema.Description
		if s, ok := schema.Properties["spec"]; ok {
			info.spec = &s
		}
		if s, ok := schema.Properties["status"]; ok {
			info.status = &s
		}
	}
	return info, true, nil
}

func overviewPage(crds []crdInfo) string {
	var b bytes.Buffer
	b.WriteString(mdxutil.Frontmatter("CRD reference", section, group, overviewOrder,
		"Every OAP custom resource, generated from the installed CRD schemas."))
	b.WriteString("# CRD reference\n\n")
	b.WriteString("OAP's resources are Kubernetes CRDs. Each page below is generated from the CRD schema — so\n")
	b.WriteString("field names, types, and descriptions match what the cluster enforces. Pick a kind for its\n")
	b.WriteString("full spec.\n\n")
	b.WriteString("| Kind | Scope | Summary |\n| --- | --- | --- |\n")
	for _, c := range crds {
		fmt.Fprintf(&b, "| [%s](#/crd-%s) | %s | %s |\n", c.kind, c.singular, c.scope, mdxutil.TableText(c.desc))
	}
	return b.String()
}

func crdPage(c crdInfo, order int) string {
	var b bytes.Buffer
	b.WriteString(mdxutil.Frontmatter(c.kind, section, group, order, mdxutil.FirstLine(c.desc)))
	fmt.Fprintf(&b, "# %s\n\n", c.kind)
	fmt.Fprintf(&b, "**Group** `%s` · **Scope** %s", c.apiGroup, c.scope)
	if len(c.shortNames) > 0 {
		fmt.Fprintf(&b, " · **Short names** `%s`", strings.Join(c.shortNames, "`, `"))
	}
	b.WriteString("\n\n")
	if c.desc != "" {
		b.WriteString(mdxutil.EscapeMDX(c.desc) + "\n\n")
	}
	if c.spec != nil {
		b.WriteString("## Spec\n\n")
		writeFieldTable(&b, "spec", c.spec)
	}
	if c.status != nil {
		b.WriteString("## Status\n\n")
		b.WriteString("Status is controller-owned (observed state).\n\n")
		writeFieldTable(&b, "status", c.status)
	}
	return b.String()
}

type fieldRow struct {
	path, typ, desc, constraints string
	required                     bool
}

func writeFieldTable(b *bytes.Buffer, prefix string, root *apiextensionsv1.JSONSchemaProps) {
	var rows []fieldRow
	walk(prefix, root, &rows, 0)
	if len(rows) == 0 {
		b.WriteString("_No documented fields._\n\n")
		return
	}
	b.WriteString("| Field | Type | Description |\n| --- | --- | --- |\n")
	for _, r := range rows {
		field := "`" + r.path + "`"
		if r.required {
			field += " \\*"
		}
		desc := mdxutil.CellText(r.desc)
		if r.constraints != "" {
			if desc != "" {
				desc += " "
			}
			desc += "_(" + mdxutil.CellText(r.constraints) + ")_"
		}
		fmt.Fprintf(b, "| %s | `%s` | %s |\n", field, r.typ, desc)
	}
	b.WriteString("\n<small>\\* required</small>\n\n")
}

// walk flattens an object schema into dotted-path rows, recursing into nested
// objects and into the item schema of object arrays (rendered as `field[]`).
func walk(prefix string, s *apiextensionsv1.JSONSchemaProps, rows *[]fieldRow, depth int) {
	if s == nil || depth > maxDepth {
		return
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := s.Properties[k]
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		*rows = append(*rows, fieldRow{
			path: path, typ: typeName(&p), desc: p.Description,
			constraints: constraints(&p), required: req[k],
		})
		switch {
		case len(p.Properties) > 0:
			walk(path, &p, rows, depth+1)
		case p.Type == "array" && p.Items != nil && p.Items.Schema != nil && len(p.Items.Schema.Properties) > 0:
			walk(path+"[]", p.Items.Schema, rows, depth+1)
		}
	}
}

func typeName(s *apiextensionsv1.JSONSchemaProps) string {
	if s == nil {
		return ""
	}
	if s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields {
		return "object (free-form)"
	}
	switch s.Type {
	case "array":
		if s.Items != nil && s.Items.Schema != nil {
			return "[]" + typeName(s.Items.Schema)
		}
		return "array"
	case "object":
		if len(s.Properties) == 0 && s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
			return "map[string]" + typeName(s.AdditionalProperties.Schema)
		}
		return "object"
	default:
		if s.Format != "" {
			return s.Type + " (" + s.Format + ")"
		}
		return s.Type
	}
}

func constraints(s *apiextensionsv1.JSONSchemaProps) string {
	var parts []string
	if len(s.Enum) > 0 {
		vals := make([]string, 0, len(s.Enum))
		for _, e := range s.Enum {
			vals = append(vals, strings.Trim(string(e.Raw), `"`))
		}
		parts = append(parts, "enum: "+strings.Join(vals, " | "))
	}
	if s.Default != nil {
		parts = append(parts, "default: "+strings.Trim(string(s.Default.Raw), `"`))
	}
	if s.Minimum != nil {
		parts = append(parts, fmt.Sprintf("min %g", *s.Minimum))
	}
	if s.Maximum != nil {
		parts = append(parts, fmt.Sprintf("max %g", *s.Maximum))
	}
	return strings.Join(parts, "; ")
}
