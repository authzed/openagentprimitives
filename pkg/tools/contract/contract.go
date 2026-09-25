// Package contract defines the kind plug-in surface that the unified
// `oap tools` CLI consumes. One impl per CR kind (MCPServer,
// SpiceboxToolspec, future kinds). Implementations register themselves
// with pkg/tools/kinds/registry at init time.
package contract

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kind is the per-tool-kind plug-in surface.
type Kind interface {
	// Name is the user-facing kind name (matches CR `kind:` field, e.g.
	// "MCPServer", "SpiceboxToolspec").
	Name() string

	// GVR is what kubectl-edit and the dynamic client need.
	GVR() schema.GroupVersionResource

	// NewObject returns a fresh empty CR pointer for client.Get.
	NewObject() client.Object
	// NewList returns a fresh empty CR list for client.List.
	NewList() client.ObjectList

	// DecodeYAML decodes one YAML document into a typed CR. Returning
	// (nil, nil) lets the registry try the next kind for that doc.
	DecodeYAML(doc []byte) (client.Object, error)

	// Row returns the columns rendered by `oap tools list`.
	Row(obj client.Object) Row
	// Detail returns the long-form view rendered by `oap tools get`.
	Detail(obj client.Object) Detail
}

// Row is one rendered row in the `oap tools list` output.
type Row struct {
	Kind      string
	Name      string
	Namespace string
	Status    string
	Summary   string
}

// Detail is the long-form view rendered by `oap tools get`.
type Detail struct {
	Kind      string
	Name      string
	Namespace string
	Sections  []Section
}

// Section is a labeled block in a Detail render.
type Section struct {
	Title string
	Body  string
}

// Validator is an optional interface for kinds that support local-file
// validation (`oap tools validate <file>`).
type Validator interface {
	ValidateFile(path string) ([]Diagnostic, error)
}

// ClusterScoped is an optional interface a Kind implements when its CR is
// cluster-scoped (resource scope=Cluster in kubebuilder). The default —
// kinds that don't implement this — is namespaced.
type ClusterScoped interface {
	ClusterScoped() bool
}

// FlatMatcher is an optional interface a Kind implements when the local-file
// authoring format ("flat") differs from the CR YAML format. The CLI uses
// MatchFlat to dispatch validate/lint/probe on flat-format files (those
// without apiVersion+kind). MatchFlat is a quick structural sniff — it
// should NOT load or compile the spec, just answer "could this be mine?".
type FlatMatcher interface {
	MatchFlat(data []byte) bool
}

// Linter is an optional interface for kinds that support local-file
// linting (`oap tools lint <file>`).
type Linter interface {
	LintFile(path string) ([]Diagnostic, error)
}

// Prober is an optional interface for kinds that support local-file
// probing (`oap tools probe <file>`).
type Prober interface {
	ProbeFile(path string) (ProbeResult, error)
}

// Diagnostic is one finding from a Validator or Linter.
type Diagnostic struct {
	Severity string // "error" | "warning"
	Path     string // optional file path / yaml jsonpath
	Message  string
}

// ProbeResult is the rendered output of a Prober.
type ProbeResult struct {
	Title    string
	Sections []Section
}
