// Package sandbox implements the contract.Kind plug-in for the
// SpiceboxToolspec CR. It backs the kind-agnostic `oap tools` verbs:
// list/get/edit/apply (cluster) and validate (local-file).
package sandbox

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// Kind is the contract.Kind impl for SpiceboxToolspec.
type Kind struct{}

// New returns a fresh Kind.
func New() *Kind { return &Kind{} }

// Name is the user-facing kind name.
func (Kind) Name() string { return "SpiceboxToolspec" }

// ClusterScoped marks SpiceboxToolspec as cluster-scoped (matches the
// kubebuilder marker on the type). The CLI uses this to skip
// namespace-defaulting on apply.
func (Kind) ClusterScoped() bool { return true }

// GVR is what kubectl-edit and the dynamic client need.
func (Kind) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    spiceboxv1alpha1.GroupName,
		Version:  spiceboxv1alpha1.SchemeGroupVersion.Version,
		Resource: "spiceboxtoolspecs",
	}
}

// NewObject returns a fresh empty CR pointer for client.Get.
func (Kind) NewObject() client.Object { return &spiceboxv1alpha1.SpiceboxToolspec{} }

// NewList returns a fresh empty CR list for client.List.
func (Kind) NewList() client.ObjectList { return &spiceboxv1alpha1.SpiceboxToolspecList{} }

// DecodeYAML decodes one YAML doc into a typed SpiceboxToolspec; returns
// (nil, nil) if the doc is a different kind.
func (Kind) DecodeYAML(doc []byte) (client.Object, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(doc, &probe); err != nil {
		return nil, fmt.Errorf("decode kind: %w", err)
	}
	if probe.Kind != "SpiceboxToolspec" {
		return nil, nil
	}
	var out spiceboxv1alpha1.SpiceboxToolspec
	if err := yaml.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("decode SpiceboxToolspec: %w", err)
	}
	return &out, nil
}

// Row returns the columns rendered by `oap tools list`.
func (Kind) Row(obj client.Object) contract.Row {
	cr := obj.(*spiceboxv1alpha1.SpiceboxToolspec)
	valid := condStatus(cr.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	toolkit := cr.Spec.Toolkit.Name
	if cr.Spec.Toolkit.Revision != "" {
		toolkit = fmt.Sprintf("%s@%s", toolkit, cr.Spec.Toolkit.Revision)
	}
	summary := fmt.Sprintf("toolkit=%s · %d subcommand(s)", toolkit, len(cr.Spec.AllowSubcommands))
	return contract.Row{
		Kind:      "SpiceboxToolspec",
		Name:      cr.Name,
		Namespace: cr.Namespace, // cluster-scoped → ""
		Status:    fmt.Sprintf("Valid=%s", valid),
		Summary:   summary,
	}
}

// Detail returns the long-form view rendered by `oap tools get`.
func (Kind) Detail(obj client.Object) contract.Detail {
	cr := obj.(*spiceboxv1alpha1.SpiceboxToolspec)
	toolkit := cr.Spec.Toolkit.Name
	if cr.Spec.Toolkit.Revision != "" {
		toolkit = fmt.Sprintf("%s@%s", toolkit, cr.Spec.Toolkit.Revision)
	}

	var sections []contract.Section
	sections = append(sections, contract.Section{Title: "Toolkit", Body: toolkit})

	if len(cr.Spec.AllowSubcommands) > 0 {
		var b strings.Builder
		for _, s := range cr.Spec.AllowSubcommands {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		sections = append(sections, contract.Section{
			Title: fmt.Sprintf("AllowSubcommands (%d)", len(cr.Spec.AllowSubcommands)),
			Body:  strings.TrimRight(b.String(), "\n"),
		})
	} else {
		sections = append(sections, contract.Section{
			Title: "AllowSubcommands",
			Body:  "(none — denies everything)",
		})
	}

	if hasDeny(cr.Spec.Deny.Effects) {
		sections = append(sections, contract.Section{
			Title: "Deny",
			Body:  renderDeny(cr.Spec.Deny.Effects),
		})
	}

	if len(cr.Spec.Constraints) > 0 {
		var b strings.Builder
		for i, c := range cr.Spec.Constraints {
			fmt.Fprintf(&b, "- [%d] %s", i, c.CEL)
			if c.Message != "" {
				fmt.Fprintf(&b, " — %s", c.Message)
			}
			b.WriteString("\n")
		}
		sections = append(sections, contract.Section{
			Title: fmt.Sprintf("Constraints (%d)", len(cr.Spec.Constraints)),
			Body:  strings.TrimRight(b.String(), "\n"),
		})
	}

	if cr.Status.ResolvedToolkit != "" {
		sections = append(sections, contract.Section{Title: "ResolvedToolkit", Body: cr.Status.ResolvedToolkit})
	}
	sections = append(sections, contract.Section{Title: "Conditions", Body: renderConditions(cr.Status.Conditions)})

	return contract.Detail{
		Kind:      "SpiceboxToolspec",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Sections:  sections,
	}
}

// MatchFlat reports whether data looks like a flat SpiceboxToolspec
// (top-level toolkit.name + allowSubcommands present).
func (Kind) MatchFlat(data []byte) bool {
	var probe struct {
		Toolkit          *struct{ Name string } `yaml:"toolkit"`
		AllowSubcommands *[]string              `yaml:"allowSubcommands"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Toolkit != nil && probe.Toolkit.Name != "" && probe.AllowSubcommands != nil
}

// ValidateFile loads a SpiceboxToolspec spec file and runs structural + CEL
// compile checks. The CEL env mirrors what the runtime validator uses.
func (Kind) ValidateFile(path string) ([]contract.Diagnostic, error) {
	sp, err := spec.Load(path)
	if err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: path, Message: err.Error()}}, nil
	}
	var diags []contract.Diagnostic
	for i, c := range sp.Constraints {
		if _, err := toolspeccel.Compile(c.CEL); err != nil {
			diags = append(diags, contract.Diagnostic{
				Severity: "error",
				Path:     fmt.Sprintf("constraints[%d]", i),
				Message:  err.Error(),
			})
		}
	}
	for i, ex := range sp.Exceptions {
		if _, err := toolspeccel.Compile(ex.When); err != nil {
			diags = append(diags, contract.Diagnostic{
				Severity: "error",
				Path:     fmt.Sprintf("exceptions[%d].when", i),
				Message:  err.Error(),
			})
		}
	}
	return diags, nil
}

// LintFile is identical to ValidateFile for SpiceboxToolspec — the local
// checks are the same.
func (k Kind) LintFile(path string) ([]contract.Diagnostic, error) {
	return k.ValidateFile(path)
}

// condStatus returns the string Status of the named condition, or "Unknown".
func condStatus(conds []metav1.Condition, t string) string {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return "Unknown"
	}
	return string(c.Status)
}

func hasDeny(d spiceboxv1alpha1.ToolspecDenyEffects) bool {
	return d.Destructive || len(d.Reads) > 0 || len(d.Writes) > 0 || d.Creds.Writes
}

func renderDeny(d spiceboxv1alpha1.ToolspecDenyEffects) string {
	var parts []string
	if d.Destructive {
		parts = append(parts, "destructive: true")
	}
	if len(d.Reads) > 0 {
		parts = append(parts, "reads: ["+strings.Join(d.Reads, ", ")+"]")
	}
	if len(d.Writes) > 0 {
		parts = append(parts, "writes: ["+strings.Join(d.Writes, ", ")+"]")
	}
	if d.Creds.Writes {
		parts = append(parts, "creds.writes: true")
	}
	return strings.Join(parts, "\n")
}

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
