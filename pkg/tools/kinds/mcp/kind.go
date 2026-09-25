// Package mcp implements the contract.Kind plug-in for the MCPServer CR.
// It backs the kind-agnostic `oap tools` verbs: list/get/edit/apply (cluster)
// and validate/lint/probe (local-file).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// Kind is the contract.Kind impl for MCPServer.
type Kind struct{}

// New returns a fresh Kind.
func New() *Kind { return &Kind{} }

// Name is the user-facing kind name (matches CR `kind:` field).
func (Kind) Name() string { return "MCPServer" }

// GVR is what kubectl-edit and the dynamic client need.
func (Kind) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    spiceboxv1alpha1.GroupName,
		Version:  spiceboxv1alpha1.SchemeGroupVersion.Version,
		Resource: "mcpservers",
	}
}

// NewObject returns a fresh empty CR pointer for client.Get.
func (Kind) NewObject() client.Object { return &spiceboxv1alpha1.MCPServer{} }

// NewList returns a fresh empty CR list for client.List.
func (Kind) NewList() client.ObjectList { return &spiceboxv1alpha1.MCPServerList{} }

// DecodeYAML decodes one YAML document into a typed MCPServer. Returning
// (nil, nil) lets the registry try the next kind for that doc.
func (Kind) DecodeYAML(doc []byte) (client.Object, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(doc, &probe); err != nil {
		return nil, fmt.Errorf("decode kind: %w", err)
	}
	if probe.Kind != "MCPServer" {
		return nil, nil
	}
	var out spiceboxv1alpha1.MCPServer
	if err := yaml.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("decode MCPServer: %w", err)
	}
	return &out, nil
}

// Row returns the columns rendered by `oap tools list`.
func (Kind) Row(obj client.Object) contract.Row {
	cr := obj.(*spiceboxv1alpha1.MCPServer)
	valid := condStatus(cr.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	reach := condStatus(cr.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable)
	summary := fmt.Sprintf("%s · %d tool(s)", cr.Spec.Server.URL, len(cr.Spec.Tools))
	status := fmt.Sprintf("Valid=%s Reachable=%s", valid, reach)
	return contract.Row{
		Kind:      "MCPServer",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Status:    status,
		Summary:   summary,
	}
}

// Detail returns the long-form view rendered by `oap tools get`.
func (Kind) Detail(obj client.Object) contract.Detail {
	cr := obj.(*spiceboxv1alpha1.MCPServer)
	sections := []contract.Section{
		{Title: "Server", Body: fmt.Sprintf("%s (%s)", cr.Spec.Server.URL, cr.Spec.Server.Transport)},
	}
	if cr.Spec.Auth.Provider != "" || cr.Spec.Auth.Header != "" {
		var b strings.Builder
		if cr.Spec.Auth.Provider != "" {
			fmt.Fprintf(&b, "provider:    %s\n", cr.Spec.Auth.Provider)
		}
		if cr.Spec.Auth.Header != "" {
			fmt.Fprintf(&b, "header:      %s\n", cr.Spec.Auth.Header)
		}
		if cr.Spec.Auth.ValuePrefix != "" {
			fmt.Fprintf(&b, "valuePrefix: %q\n", cr.Spec.Auth.ValuePrefix)
		}
		sections = append(sections, contract.Section{Title: "Auth", Body: strings.TrimRight(b.String(), "\n")})
	}
	sections = append(sections,
		contract.Section{Title: fmt.Sprintf("Allowlisted tools (%d)", len(cr.Spec.Tools)), Body: renderToolList(cr.Spec.Tools)},
		contract.Section{Title: "Observed tools", Body: renderObserved(cr.Spec.Tools, cr.Status.ObservedTools)},
		contract.Section{Title: "Conditions", Body: renderConditions(cr.Status.Conditions)},
	)
	return contract.Detail{
		Kind:      "MCPServer",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Sections:  sections,
	}
}

// MatchFlat reports whether data looks like a flat MCP spec (top-level
// server.url present, no toolkit.name).
func (Kind) MatchFlat(data []byte) bool {
	var probe struct {
		Server  *struct{ URL string }  `yaml:"server"`
		Toolkit *struct{ Name string } `yaml:"toolkit"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Server != nil && probe.Server.URL != "" && (probe.Toolkit == nil || probe.Toolkit.Name == "")
}

// ValidateFile loads a flat MCP spec file and runs structural + CEL compile
// checks. Returns one diagnostic per error.
func (Kind) ValidateFile(path string) ([]contract.Diagnostic, error) {
	sp, err := mcpspec.Load(path)
	if err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: path, Message: err.Error()}}, nil
	}
	res, err := mcpspec.Compile(sp)
	if err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: path, Message: err.Error()}}, nil
	}
	var diags []contract.Diagnostic
	for _, w := range res.Warnings {
		diags = append(diags, contract.Diagnostic{Severity: "warning", Path: path, Message: fmt.Sprintf("%s: %s", w.Path, w.Message)})
	}
	return diags, nil
}

// LintFile is identical to ValidateFile for MCP — the local checks are the
// same. The two verbs split apart for kinds where lint is a strict subset.
func (k Kind) LintFile(path string) ([]contract.Diagnostic, error) {
	return k.ValidateFile(path)
}

// ProbeFile loads a flat MCP spec, calls tools/list against the server URL,
// and returns the live tool list as a ProbeResult.
func (Kind) ProbeFile(path string) (contract.ProbeResult, error) {
	sp, err := mcpspec.Load(path)
	if err != nil {
		return contract.ProbeResult{}, err
	}
	c := &probe.Client{URL: sp.Server.URL}
	// Probe is anonymous from a file — no auth context. The CLI's
	// `oap tools probe` is informational; flow through credentials happens at
	// runtime via the controller.
	tools, err := c.ListTools(context.Background(), "", "")
	if err != nil {
		return contract.ProbeResult{}, err
	}
	var sections []contract.Section
	sections = append(sections, contract.Section{
		Title: fmt.Sprintf("%s — %d tool(s)", sp.Server.URL, len(tools)),
		Body:  renderProbeTools(tools),
	})
	return contract.ProbeResult{Title: "MCP probe", Sections: sections}, nil
}

// condStatus returns the string Status of the named condition, or "Unknown".
func condStatus(conds []metav1.Condition, t string) string {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return "Unknown"
	}
	return string(c.Status)
}

// renderConditions formats a slice of metav1.Condition for the Conditions
// section of Detail.
func renderConditions(conds []metav1.Condition) string {
	if len(conds) == 0 {
		return "(none — controller has not reconciled this resource yet)"
	}
	var b strings.Builder
	for _, c := range conds {
		fmt.Fprintf(&b, "- %s=%s reason=%s", c.Type, c.Status, c.Reason)
		if c.Message != "" {
			b.WriteString("\n  ")
			b.WriteString(strings.ReplaceAll(strings.TrimSpace(c.Message), "\n", "\n  "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderToolList renders the allowlisted MCPServerTool slice for a Detail
// section.
func renderToolList(tools []spiceboxv1alpha1.MCPServerTool) string {
	if len(tools) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s", t.Name)
		if t.DescriptionOverride != "" {
			fmt.Fprintf(&b, " (override)")
		}
		if len(t.Args.AllowedFields) > 0 {
			fmt.Fprintf(&b, " allowedFields=%s", strings.Join(t.Args.AllowedFields, ","))
		}
		if len(t.Args.Constraints) > 0 {
			fmt.Fprintf(&b, " constraints=%d", len(t.Args.Constraints))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderObserved formats Status.ObservedTools, marking each tool that's not in
// the allowlist.
func renderObserved(allowed []spiceboxv1alpha1.MCPServerTool, observed []string) string {
	if len(observed) == 0 {
		return "(none — server has not been probed yet)"
	}
	allowSet := map[string]bool{}
	for _, t := range allowed {
		allowSet[t.Name] = true
	}
	var b strings.Builder
	for _, n := range observed {
		fmt.Fprintf(&b, "- %s", n)
		if !allowSet[n] {
			b.WriteString(" (not allowlisted)")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderProbeTools formats a probe.Tool slice for the Probe ProbeResult body.
func renderProbeTools(tools []probe.Tool) string {
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s", t.Name)
		if t.Description != "" {
			fmt.Fprintf(&b, " — %s", t.Description)
		}
		b.WriteString("\n")
		if len(t.InputSchema) > 0 {
			var pretty []byte
			pretty, _ = json.MarshalIndent(json.RawMessage(t.InputSchema), "    ", "  ")
			if len(pretty) > 0 {
				fmt.Fprintf(&b, "    inputSchema: %s\n", string(pretty))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
