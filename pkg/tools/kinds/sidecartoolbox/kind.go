// Package sidecartoolbox implements the contract.Kind plug-in for the
// SidecarToolbox CR: CLI PRESENTATION only (Row, Detail, ValidateFile),
// blank-imported by cmd/oap, touching no credential.
//
// Not to be confused with pkg/agent/tool/sidecartoolbox, the EXECUTION
// synthesizer (Synthesize) that internal/cmd/runner imports directly to build sidecar
// tools from session status. A new CLI kind gets a blank import here; the
// synthesizer does not.
package sidecartoolbox

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

type Kind struct{}

func New() *Kind { return &Kind{} }

func (Kind) Name() string { return "SidecarToolbox" }

func (Kind) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    spiceboxv1alpha1.GroupName,
		Version:  spiceboxv1alpha1.SchemeGroupVersion.Version,
		Resource: "sidecartoolboxes",
	}
}

func (Kind) NewObject() client.Object   { return &spiceboxv1alpha1.SidecarToolbox{} }
func (Kind) NewList() client.ObjectList { return &spiceboxv1alpha1.SidecarToolboxList{} }

func (Kind) DecodeYAML(doc []byte) (client.Object, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(doc, &probe); err != nil {
		return nil, fmt.Errorf("decode kind: %w", err)
	}
	if probe.Kind != "SidecarToolbox" {
		return nil, nil
	}
	var out spiceboxv1alpha1.SidecarToolbox
	if err := yaml.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("decode SidecarToolbox: %w", err)
	}
	return &out, nil
}

func (Kind) Row(obj client.Object) contract.Row {
	cr := obj.(*spiceboxv1alpha1.SidecarToolbox)
	valid := condStatus(cr.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionValid)
	reach := condStatus(cr.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionReachable)
	src := "image=" + cr.Spec.Source.Image
	if cr.Spec.Source.Inline != nil {
		src = "inline=" + cr.Spec.Source.Inline.BaseImage
	}
	return contract.Row{
		Kind:      "SidecarToolbox",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Status:    fmt.Sprintf("Valid=%s Reachable=%s", valid, reach),
		Summary:   fmt.Sprintf("%s · %d tool(s)", src, len(cr.Spec.Tools)),
	}
}

func (Kind) Detail(obj client.Object) contract.Detail {
	cr := obj.(*spiceboxv1alpha1.SidecarToolbox)
	var sections []contract.Section
	sections = append(sections, contract.Section{Title: "Source", Body: renderSource(cr.Spec.Source)})
	sections = append(sections, contract.Section{Title: "Sandbox", Body: renderSandbox(cr.Spec.Sandbox)})
	sections = append(sections, contract.Section{
		Title: "Transport",
		Body: fmt.Sprintf("port=%d healthcheck=%s timeout=%ds",
			cr.Spec.Transport.Port,
			cr.Spec.Transport.Healthcheck.Path,
			cr.Spec.Transport.Healthcheck.TimeoutSeconds),
	})
	sections = append(sections, contract.Section{Title: "UpstreamAuth", Body: "provider=" + cr.Spec.UpstreamAuth.Provider})
	sections = append(sections, contract.Section{
		Title: fmt.Sprintf("Allowlisted tools (%d)", len(cr.Spec.Tools)),
		Body:  renderToolList(cr.Spec.Tools),
	})
	if len(cr.Status.ObservedTools) > 0 {
		sections = append(sections, contract.Section{Title: "ObservedTools", Body: strings.Join(cr.Status.ObservedTools, "\n")})
	}
	sections = append(sections, contract.Section{Title: "Conditions", Body: renderConditions(cr.Status.Conditions)})
	return contract.Detail{
		Kind:      "SidecarToolbox",
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Sections:  sections,
	}
}

// MatchFlat: top-level source + sandbox + upstreamAuth present, and no
// MCPServer-shaped server.url.
func (Kind) MatchFlat(data []byte) bool {
	var probe struct {
		Server *struct {
			URL string `yaml:"url"`
		} `yaml:"server"`
		Source *struct {
			Image  string         `yaml:"image"`
			Inline map[string]any `yaml:"inline"`
		} `yaml:"source"`
		Sandbox *struct {
			Class string `yaml:"class"`
		} `yaml:"sandbox"`
		UpstreamAuth *struct {
			Provider string `yaml:"provider"`
		} `yaml:"upstreamAuth"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	if probe.Server != nil && probe.Server.URL != "" {
		return false
	}
	return probe.Source != nil && probe.Sandbox != nil && probe.UpstreamAuth != nil
}

func (k Kind) ValidateFile(path string) ([]contract.Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	if err := yaml.Unmarshal(data, &sp); err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: path, Message: err.Error()}}, nil
	}
	var diags []contract.Diagnostic
	if err := validateSourceDiscriminator(sp.Source); err != nil {
		diags = append(diags, contract.Diagnostic{Severity: "error", Path: "source", Message: err.Error()})
	}
	if strings.TrimSpace(sp.Sandbox.Class) == "" {
		diags = append(diags, contract.Diagnostic{Severity: "error", Path: "sandbox.class", Message: "sandbox.class is required"})
	}
	// spiceboxv1alpha1.UpstreamAuthProviderNone (its doc comment carries the
	// full rationale) is a valid, non-empty provider value and passes this
	// check like any other — only a truly empty provider is rejected, because
	// the field must always state an explicit intent (a real provider name,
	// or the sentinel), never an accidental omission. Checked by symbol, not
	// by a duplicated literal, so this and providerCheck
	// (pkg/controllers/sidecartoolbox/controller.go) can never drift on what
	// counts as the sentinel.
	if strings.TrimSpace(sp.UpstreamAuth.Provider) == "" {
		diags = append(diags, contract.Diagnostic{Severity: "error", Path: "upstreamAuth.provider", Message: fmt.Sprintf("upstreamAuth.provider is required (use %q for a controller-issued-token sidecar)", spiceboxv1alpha1.UpstreamAuthProviderNone)})
	}
	// The sentinel and an envVar are mutually exclusive: nothing ever resolves a
	// library credential for a controller-issued-token sidecar, so an envVar
	// would name a variable no credential is ever injected into — and at session
	// boot it dead-ends on advice ("run `oap agent setup-identity`") that cannot
	// fix it. Caught here so validate_spec refuses it before an approval card,
	// mirroring providerCheck (pkg/controllers/sidecartoolbox/controller.go).
	if strings.TrimSpace(sp.UpstreamAuth.Provider) == spiceboxv1alpha1.UpstreamAuthProviderNone && strings.TrimSpace(sp.UpstreamAuth.EnvVar) != "" {
		diags = append(diags, contract.Diagnostic{Severity: "error", Path: "upstreamAuth.envVar", Message: fmt.Sprintf("upstreamAuth.envVar must be empty when upstreamAuth.provider is %q", spiceboxv1alpha1.UpstreamAuthProviderNone)})
	}
	for ti, t := range sp.Tools {
		for ci, c := range t.Args.Constraints {
			if _, err := toolspeccel.Compile(c.CEL); err != nil {
				diags = append(diags, contract.Diagnostic{
					Severity: "error",
					Path:     fmt.Sprintf("tools[%d].args.constraints[%d]", ti, ci),
					Message:  err.Error(),
				})
			}
		}
	}
	if isAPIAdapterImage(sp.Source.Image) {
		diags = append(diags, validateAdapterConfig(sp)...)
	}
	return diags, nil
}

// isAPIAdapterImage reports whether ref's repository basename matches
// apimage.APIAdapter.Name — the allowlisted name the admission webhook's
// checkSidecarImage also keys its config-carrier rule on (plan 7b task 3).
// This is a client-side hint, not a security boundary: unlike the webhook,
// ValidateFile has no cluster trust context to verify which registry ref
// actually resolves from, so a "config" diagnostic here is advisory — the
// webhook is what actually enforces the contract at admission time.
func isAPIAdapterImage(ref string) bool {
	if strings.TrimSpace(ref) == "" {
		return false
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return false
	}
	repoStr := parsed.Context().RepositoryStr()
	if i := strings.LastIndex(repoStr, "/"); i >= 0 {
		repoStr = repoStr[i+1:]
	}
	return repoStr == apimage.APIAdapter.Name
}

// validateAdapterConfig runs the SAME checks the admission webhook's
// checkSidecarConfig applies to an ap-api-adapter SidecarToolbox (plan 7b
// task 3, extended by MINOR-2): spec.config must parse as an
// apiadapter.Config, its auth.envVar (when auth is not "none") must agree
// with spec.upstreamAuth.envVar, and spec.tools must name exactly its
// operations — via apiadapter.Config.UpstreamEnvVarMismatch and
// .ToolNamesMatch, the comparisons both checks share, so the builder's
// "validate before apply" step gives the SAME refusal the cluster would give
// at admission, instead of returning clean and letting the first refusal
// arrive there.
func validateAdapterConfig(sp spiceboxv1alpha1.SidecarToolboxSpec) []contract.Diagnostic {
	if strings.TrimSpace(sp.Config) == "" {
		return []contract.Diagnostic{{
			Severity: "error",
			Path:     "config",
			Message:  fmt.Sprintf("spec.config is required for image %s: the adapter has no tools without it", apimage.APIAdapter.Name),
		}}
	}
	cfg, err := apiadapter.Parse([]byte(sp.Config))
	if err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: "config", Message: err.Error()}}
	}
	// UpstreamEnvVarMismatch is the SAME check the admission webhook's
	// checkSidecarConfig runs, so an author sees the identical refusal locally
	// that a mismatched upstreamAuth.envVar would otherwise only surface at
	// sidecar boot.
	if msg := cfg.UpstreamEnvVarMismatch(sp.UpstreamAuth.EnvVar); msg != "" {
		return []contract.Diagnostic{{Severity: "error", Path: "upstreamAuth.envVar", Message: msg}}
	}
	declared := make([]string, 0, len(sp.Tools))
	for _, t := range sp.Tools {
		declared = append(declared, t.Name)
	}
	if msg := cfg.ToolNamesMatch(declared); msg != "" {
		return []contract.Diagnostic{{Severity: "error", Path: "tools", Message: msg}}
	}
	return nil
}

func (k Kind) LintFile(path string) ([]contract.Diagnostic, error) { return k.ValidateFile(path) }

// validateSourceDiscriminator ensures exactly one of image / inline is set
// and that, when inline, all required sub-fields are populated.
func validateSourceDiscriminator(s spiceboxv1alpha1.SidecarToolboxSource) error {
	hasImage := strings.TrimSpace(s.Image) != ""
	hasInline := s.Inline != nil
	if hasImage && hasInline {
		return errors.New("source: exactly one of image or inline is allowed (both supplied)")
	}
	if !hasImage && !hasInline {
		return errors.New("source: exactly one of image or inline must be set (none supplied)")
	}
	if hasInline {
		if strings.TrimSpace(s.Inline.BaseImage) == "" {
			return errors.New("source.inline.baseImage is required")
		}
		if strings.TrimSpace(s.Inline.Script.ConfigMapRef.Name) == "" || strings.TrimSpace(s.Inline.Script.ConfigMapRef.Key) == "" {
			return errors.New("source.inline.script.configMapRef.{name,key} are required")
		}
		if len(s.Inline.Entrypoint) == 0 {
			return errors.New("source.inline.entrypoint must be non-empty")
		}
	}
	return nil
}

func condStatus(conds []metav1.Condition, t string) string {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return "Unknown"
	}
	return string(c.Status)
}

func renderSource(s spiceboxv1alpha1.SidecarToolboxSource) string {
	if s.Image != "" {
		return "image: " + s.Image
	}
	if s.Inline != nil {
		return fmt.Sprintf("inline:\n  baseImage:  %s\n  script:     %s/%s\n  entrypoint: %v",
			s.Inline.BaseImage,
			s.Inline.Script.ConfigMapRef.Name,
			s.Inline.Script.ConfigMapRef.Key,
			s.Inline.Entrypoint)
	}
	return "(none)"
}

func renderSandbox(s spiceboxv1alpha1.SidecarToolboxSandbox) string {
	hosts := "(none)"
	if len(s.Network.AllowedHosts) > 0 {
		hosts = strings.Join(s.Network.AllowedHosts, ", ")
	}
	return fmt.Sprintf("class: %s\nallowedHosts: %s", s.Class, hosts)
}

func renderToolList(tools []spiceboxv1alpha1.MCPServerTool) string {
	if len(tools) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s", t.Name)
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

func renderConditions(conds []metav1.Condition) string {
	if len(conds) == 0 {
		return "(none)"
	}
	sort.Slice(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	var b strings.Builder
	for _, c := range conds {
		fmt.Fprintf(&b, "- %s=%s reason=%s", c.Type, c.Status, c.Reason)
		if c.Message != "" {
			b.WriteString("\n  ")
			b.WriteString(strings.TrimSpace(c.Message))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
