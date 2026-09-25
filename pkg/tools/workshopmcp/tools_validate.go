// tools_validate.go implements `validate_spec`, three checks in one call so
// the builder agent catches a structural, CEL, schema or whole-workshop error
// before ever calling workshop_apply — design spec's "Assess before Apply"
// step:
//
//   - The local half: round-trip the candidate's spec content through the
//     SAME local-file validator and CEL compiler `oap tools validate` uses
//     for its kind (cmd/oap/internal/toolscmd/validate.go), for the three
//     kinds that have one. It writes the candidate to a throwaway temp file
//     and calls that kind's own contract.Validator, exactly the shape a local
//     file takes on disk before `oap tools apply` turns it into a CR.
//   - The server half: a DRY RUN of the exact server-side apply
//     workshop_apply would make, for every kind the workshop can author
//     (tools_crud.go's workshopKindTable). The apiserver answers with the
//     CRD's own schema and rules, and nothing is created. This half exists
//     because the local validators decode leniently — a field the CRD never
//     declared passed them and failed only at the real apply — and because
//     the kinds with no local validator at all (AgentIdentity, Skill,
//     AgentUI, AgentClass) were otherwise discoverable only by
//     approval-gated applies, one human click per guessed field (observed
//     live on oap-desktop, 2026-09-13).
//   - The cross-check half (crossChecksByKind): rules about how the candidate
//     combines with the resources ALREADY in the workshop namespace, which
//     neither of the other two halves can state because each sees exactly one
//     document. Today that is one rule, authored from either end: an MCPServer
//     nobody can ever connect an account to, under a class that runs as the
//     person.
//
// validate_spec is read-only and auto — that policy is declared on this
// sidecar's SidecarToolbox entry (Task 8), not enforced by this file. The dry
// run keeps it true: DryRunAll is what makes the server half a read.
package workshopmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	mcpkind "github.com/authzed/openagentprimitives/pkg/tools/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/sandbox"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/sidecartoolbox"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// toolValidateSpec is the name `validate_spec` announces on the MCP surface.
const toolValidateSpec = "validate_spec"

// validateSpecKindTable maps the CR kind names that have a local-file
// contract.Validator — the exact same three impls `oap tools validate`
// dispatches to — to that validator. The other kinds the workshop can author
// (AgentClass, AgentIdentity, Skill, AgentUI — tools_crud.go's
// workshopKindTable) have no local-file authoring format and implement no
// contract.Validator, so they are deliberately absent here; for them
// validate_spec's server half (dryRunDiagnostics) is the whole check. A data
// table, not an `if kind == "..."` chain (CLAUDE.md's pluggability rule of
// thumb) — a fourth Validator kind is one new entry, read uniformly by
// validateCandidate.
var validateSpecKindTable = map[string]contract.Validator{
	"SpiceboxToolspec": sandbox.Kind{},
	"MCPServer":        mcpkind.Kind{},
	"SidecarToolbox":   sidecartoolbox.Kind{},
}

// extraChecksByKind holds the per-kind checks validate_spec runs IN ADDITION
// to that kind's own local validator — rules the validator cannot state
// because they are about how the platform will treat the candidate at CALL
// time, not about whether the document is well-formed.
//
// A table, not an `if kind == "..."` chain, for the same reason
// validateSpecKindTable is one: a second kind's check is one new entry, read
// uniformly by validateCandidate.
var extraChecksByKind = map[string]func(raw []byte) []contract.Diagnostic{
	"MCPServer": mcpServerArgumentGateDiagnostics,
}

// mcpServerArgumentGateDiagnostics warns about every tool entry whose argument
// gate would refuse every argument sent to it.
//
// The gate is fail-closed (mcpspec.RefusesEveryArgument): a tool entry with no
// allowedFields and no unconstrainedArgs is fully specified and refuses every
// call that carries an argument. Authoring one is easy and its consequence is
// invisible until the agent is running — a tool authored with CEL constraints
// and no allowedFields passed every check, earned a person's approval, was
// installed, and then denied every single call it received.
//
// A WARNING, not an error: a tool that genuinely takes no arguments is
// correctly authored this way, and failing the candidate would refuse a
// correct spec. The document's shape cannot say which case this is — only the
// connector's own schema can, which is what workshop_probe_mcp reports.
func mcpServerArgumentGateDiagnostics(raw []byte) []contract.Diagnostic {
	// Both accepted manifest shapes: a full resource carries spec.tools, and
	// spec-content-only carries tools at the top level. A body that decodes as
	// neither yields no diagnostics here — the local validator and the dry run
	// both already report a decode failure, and a second copy of that error
	// would only be noise.
	var doc struct {
		Spec  *spiceboxv1alpha1.MCPServerSpec  `json:"spec"`
		Tools []spiceboxv1alpha1.MCPServerTool `json:"tools"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	tools := doc.Tools
	if doc.Spec != nil {
		tools = doc.Spec.Tools
	}
	var diags []contract.Diagnostic
	for i, t := range tools {
		if !mcpspec.RefusesEveryArgument(t.Args.AllowedFields, t.Args.UnconstrainedArgs) {
			continue
		}
		diags = append(diags, contract.Diagnostic{
			Severity: "warning",
			Path:     fmt.Sprintf("tools[%d].args", i),
			Message: fmt.Sprintf(
				"tool %q will refuse every argument: set allowedFields to the exact argument names it needs, "+
					"or unconstrainedArgs: true if it is a read-only tool whose arguments are free text. "+
					"Leave both unset only for a tool that takes no arguments at all.", t.Name),
		})
	}
	return diags
}

// crossChecksByKind holds the per-kind checks validate_spec runs against the
// REST of the workshop namespace. Unlike extraChecksByKind's pure functions
// these read the cluster, because the rule they state is about a PAIR of
// resources and no single document carries both halves.
//
// A table, not an `if kind == "..."` chain, for the same reason the two tables
// above are ones: a third kind's cross-check is one new entry, read uniformly
// by handleValidate.
//
// Both entries today state ONE rule from the two ends it can be authored
// from — an MCPServer nobody can ever connect an account to, under a class
// that runs as the person — so the gate is order-independent: whichever of
// the two resources is validated second sees the other.
var crossChecksByKind = map[string]func(ctx context.Context, s *Server, raw []byte) []contract.Diagnostic{
	"MCPServer":  mcpServerPassthroughDiagnostics,
	"AgentClass": agentClassPassthroughDiagnostics,
}

// crossCheckUnavailable is what validate_spec says when a cross-check could
// not read the workshop namespace. A WARNING, never an error: a read that did
// not happen is not evidence against the candidate, and failing a correct spec
// on a transient read would be worse than the gap the rule closes. Never a
// silent pass either — the caller is told the comparison did not happen. The
// raw cause is logged rather than relayed: the caller is a builder model that
// may repeat this sentence to a person, and an apiserver error carries
// infrastructure words that must not reach one.
const crossCheckUnavailable = "the per-person credential check could not run just now: this candidate was not compared " +
	"with the agents that reference it, so run validate_spec again before relying on it"

// mcpServerPassthroughDiagnostics refuses an MCPServer that names no
// credential a person could ever link, under a class that runs as the person.
//
// The predicate is passthrough.Unauthenticated — the SAME one the credential
// resolver skips on — which is what makes the refusal correct rather than a
// guess: a server of that shape contributes no credential requirement, so
// under identityMode=userPassthrough nobody is ever prompted to link an
// account, no header is injected, and every upstream call goes out
// unauthenticated. The agent looks finished and is not. Observed live on
// oap-desktop (2026-09-13), twice in one class, with every other check passing.
//
// The rule needs the CLASS's identity mode, which the MCPServer reconciler
// cannot see and validate_spec can: the same server under an agent-mode class
// is legitimate, its credential coming from the AgentIdentity.
func mcpServerPassthroughDiagnostics(ctx context.Context, s *Server, raw []byte) []contract.Diagnostic {
	name, spec := decodeMCPServerCandidate(raw)
	if !unconnectableAuth(spec) {
		return nil
	}
	// Nothing an AgentClass ref could name, so nothing to compare: a nameless
	// MCPServer is already an error from the local validator ("name is
	// required"), and a second copy of it here would only be noise.
	if name == "" {
		return nil
	}
	if s.K8s == nil {
		return []contract.Diagnostic{crossCheckSkipped("spec.auth", "MCPServer", name, s.Identity.Namespace, "no cluster client")}
	}
	var classes spiceboxv1alpha1.AgentClassList
	if err := s.K8s.List(ctx, &classes, client.InNamespace(s.Identity.Namespace)); err != nil {
		return []contract.Diagnostic{crossCheckSkipped("spec.auth", "MCPServer", name, s.Identity.Namespace, err.Error())}
	}
	var diags []contract.Diagnostic
	for i := range classes.Items {
		ac := &classes.Items[i]
		if !referencesMCPServer(ac, name) {
			continue
		}
		severity := passthroughSeverity(ac.Spec.IdentityMode)
		if severity == "" {
			continue
		}
		diags = append(diags, contract.Diagnostic{
			Severity: severity,
			Path:     "spec.auth",
			Message:  unconnectableServerMessage(name, spec.Auth.Type, ac.Spec.IdentityMode, ac.Name),
		})
	}
	return diags
}

// agentClassPassthroughDiagnostics is the same rule from the class's end: a
// class that runs as the person, referencing a server nobody can connect an
// account to. Reading it from both ends is what makes the gate
// order-independent — a builder that authors the server first and the class
// second is told either way.
//
// A referenced server that does not exist yet is skipped SILENTLY: validating
// a class before its servers exist is a normal authoring order, and a truly
// dangling ref is already the dry run's finding, reported in the apiserver's
// own words.
func agentClassPassthroughDiagnostics(ctx context.Context, s *Server, raw []byte) []contract.Diagnostic {
	spec := decodeAgentClassCandidate(raw)
	if spec == nil {
		return nil
	}
	severity := passthroughSeverity(spec.IdentityMode)
	if severity == "" {
		return nil
	}
	if s.K8s == nil {
		return []contract.Diagnostic{crossCheckSkipped("spec.mcpServers", "AgentClass", "", s.Identity.Namespace, "no cluster client")}
	}
	var diags []contract.Diagnostic
	for i, ref := range spec.MCPServers {
		if ref.Ref == "" {
			continue
		}
		path := fmt.Sprintf("spec.mcpServers[%d]", i)
		var srv spiceboxv1alpha1.MCPServer
		err := s.K8s.Get(ctx, client.ObjectKey{Namespace: s.Identity.Namespace, Name: ref.Ref}, &srv)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			diags = append(diags, crossCheckSkipped(path, "MCPServer", ref.Ref, s.Identity.Namespace, err.Error()))
			continue
		}
		if !unconnectableAuth(&srv.Spec) {
			continue
		}
		diags = append(diags, contract.Diagnostic{
			Severity: severity,
			Path:     path,
			Message:  unconnectableServerMessage(ref.Ref, srv.Spec.Auth.Type, spec.IdentityMode, ""),
		})
	}
	return diags
}

// crossCheckSkipped logs why a cross-check could not read what it needed —
// with the namespace, kind and name an operator greps for — and returns the
// fixed warning the caller sees in its place.
func crossCheckSkipped(path, kind, name, namespace, cause string) contract.Diagnostic {
	slog.Default().Info("workshopmcp: the per-person credential check could not read the workshop namespace",
		"namespace", namespace, "kind", kind, "name", name, "err", cause)
	return contract.Diagnostic{Severity: "warning", Path: path, Message: crossCheckUnavailable}
}

// unconnectableAuth reports whether spec is the shape nobody can ever connect
// an account to: passthrough.Unauthenticated (the credential resolver's own
// skip predicate) narrowed to a server that DECLARES it wants a token —
// auth.type oauth or static. A server declaring no type at all is left alone:
// it may simply be an endpoint that needs no credential, which is a
// legitimate thing to author and is not what this rule is about.
//
// Spelled with passthroughcatalog's constants rather than string literals.
// MCPServer.Spec.Auth.Type is a DIFFERENT enum from the credential-type one,
// which happens to share two value names — and pkg/platform/identity/credkind's
// guard test fails any comparison against those names outside the credkind
// tree. It carries a file exemption list for exactly this collision; using the
// constants keeps this file off it, which is the better end of the same fix.
func unconnectableAuth(spec *spiceboxv1alpha1.MCPServerSpec) bool {
	if !passthrough.Unauthenticated(spec) {
		return false
	}
	return spec.Auth.Type == passthroughcatalog.AuthTypeOAuth ||
		spec.Auth.Type == passthroughcatalog.AuthTypeStatic
}

// passthroughSeverity maps an AgentClass's identity mode onto how hard
// validate_spec refuses a server nobody can connect: certain under
// userPassthrough (the session WILL run as the person), a warning under
// ask/dynamic (the person may still choose their own account at session time,
// and then nobody can connect one), and nothing under agent — including the
// empty value the CRD defaults to agent — where the credential comes from
// spec.agentIdentity and naming none on the server is correct.
func passthroughSeverity(mode string) string {
	switch mode {
	case spiceboxv1alpha1.IdentityModeUserPassthrough:
		return "error"
	case spiceboxv1alpha1.IdentityModeAsk, spiceboxv1alpha1.IdentityModeDynamic:
		return "warning"
	default:
		return ""
	}
}

// unconnectableServerMessage is the one wording both cross-checks emit. Its
// reader is the builder model, which relays it: it names the fields to set and
// the consequence of leaving them unset, and no infrastructure word beyond
// those field names.
//
// referencedBy names the AgentClass the finding is about, and is set only by
// the SERVER-side check — the end that walks a SET of classes and so can emit
// two findings for one candidate. Without it those two are the same sentence
// twice, which reads as one finding reported in duplicate: a person told
// "this connector has a problem" fixes the agent they were thinking of and
// leaves the other exactly as broken. The class side passes "" — there the
// referencing class IS the candidate under validation, so naming it would
// repeat back what the caller just handed in.
func unconnectableServerMessage(server, authType, mode, referencedBy string) string {
	runs := "may run as the person"
	if mode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		runs = "runs as the person"
	}
	subject := fmt.Sprintf("server %q", server)
	if referencedBy != "" {
		subject = fmt.Sprintf("server %q, referenced by %q,", server, referencedBy)
	}
	return fmt.Sprintf("%s uses %s auth but names neither auth.provider nor auth.credential, and the agent %s "+
		"(identityMode %s): nobody can ever be asked to connect an account, so every call would go out unauthenticated. "+
		"Name the credential the person will link (auth.credential) and the provider that issues it (auth.provider), "+
		"or run the agent with its own identity.", subject, authType, runs, mode)
}

// referencesMCPServer reports whether ac names this MCPServer in its own
// spec.mcpServers.
func referencesMCPServer(ac *spiceboxv1alpha1.AgentClass, name string) bool {
	for _, ref := range ac.Spec.MCPServers {
		if ref.Ref == name {
			return true
		}
	}
	return false
}

// decodeMCPServerCandidate decodes a candidate into the spec the cross-check
// reads and the NAME an AgentClass ref would name it by. Both accepted
// manifest shapes, derived the way candidateObject derives them: a document
// carrying `spec` is a full resource, named by metadata.name (its spec.name is
// the tool-name prefix, not the CR's name); anything else is spec content in
// the local-file shape, whose own `name` is the closest thing to a CR name it
// carries. A body that decodes as neither yields ("", nil) and no diagnostics
// — the local validator and the dry run both already report a decode failure.
func decodeMCPServerCandidate(raw []byte) (string, *spiceboxv1alpha1.MCPServerSpec) {
	var full struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec *spiceboxv1alpha1.MCPServerSpec `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &full); err != nil {
		return "", nil
	}
	if full.Spec != nil {
		name := full.Metadata.Name
		if name == "" {
			name = full.Spec.Name
		}
		return name, full.Spec
	}
	var bare spiceboxv1alpha1.MCPServerSpec
	if err := yaml.Unmarshal(raw, &bare); err != nil {
		return "", nil
	}
	return bare.Name, &bare
}

// decodeAgentClassCandidate decodes a candidate into its spec, over the same
// two manifest shapes decodeMCPServerCandidate reads. A body that decodes as
// neither yields nil and no diagnostics, for the same reason.
func decodeAgentClassCandidate(raw []byte) *spiceboxv1alpha1.AgentClassSpec {
	var full struct {
		Spec *spiceboxv1alpha1.AgentClassSpec `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &full); err != nil {
		return nil
	}
	if full.Spec != nil {
		return full.Spec
	}
	var bare spiceboxv1alpha1.AgentClassSpec
	if err := yaml.Unmarshal(raw, &bare); err != nil {
		return nil
	}
	return &bare
}

// validateSpecKindNames lists every kind validateSpecKindTable knows,
// sorted — used to build a helpful error message for an unknown kind.
func validateSpecKindNames() []string {
	names := make([]string, 0, len(validateSpecKindTable))
	for k := range validateSpecKindTable {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// registerValidate wires `validate_spec` onto mcpSrv.
func (s *Server) registerValidate(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolValidateSpec,
		Description: "Check a candidate resource before apply, with nothing created and no approval " +
			"needed. For SpiceboxToolspec, MCPServer and SidecarToolbox the spec content is " +
			"round-tripped through the same local-file validator and CEL compiler `oap tools " +
			"validate` uses; then, for EVERY kind the workshop can author, the exact apply " +
			"workshop_apply would make is dry-run against the cluster, so the schema and rules " +
			"the real apply enforces answer here first. Returns every diagnostic found " +
			"(severity/path/message); an empty result means the candidate is structurally " +
			"sound, every CEL expression compiles, and the cluster would accept it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind": map[string]any{
					"type":        "string",
					"description": "one of " + strings.Join(workshopKindNames(), ", "),
				},
				"manifest": map[string]any{
					"description": "the candidate's spec content — a JSON object, or a raw YAML/JSON " +
						"string — the same shape `oap tools validate` reads from a local file; a " +
						"full resource (kind, metadata, spec) is accepted too, exactly as apply " +
						"takes it",
				},
			},
			"required": []any{"kind", "manifest"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleValidate)
}

// validateArgs is validate_spec's own argument shape. Manifest is decoded
// leniently by manifestBytes: a JSON object round-trips as YAML-compatible
// bytes verbatim (JSON is a YAML subset), while a JSON string is unwrapped
// so its contents — a raw YAML/JSON document — are what gets validated,
// rather than the string's own JSON quoting.
type validateArgs struct {
	Kind     string          `json:"kind"`
	Manifest json.RawMessage `json:"manifest"`
}

// handleValidate answers the `validate_spec` tool call: the local half, then
// the cross-check half for a kind that has one, then — for a kind the workshop
// can author, and only when the LOCAL half found no error, since a candidate
// the local validator refuses has no shape worth sending anywhere — the server
// half.
//
// The dry run's gate reads the local half alone, deliberately: a cross-check
// error says the candidate combines badly with the workshop, not that it is
// malformed, and letting it suppress the dry run would hide a schema refusal
// behind it for as long as the pairing stays broken.
func (s *Server) handleValidate(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a validateArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("validate_spec: decode arguments: %v", err), nil
	}
	if a.Kind == "" {
		return s.toolErr("validate_spec: kind is required"), nil
	}
	body, err := manifestBytes(a.Manifest)
	if err != nil {
		return s.toolErr("validate_spec: %v", err), nil
	}

	diags := validateCandidate(a.Kind, body)
	localClean := !hasErrorDiagnostic(diags)
	if cross, ok := crossChecksByKind[a.Kind]; ok {
		diags = append(diags, cross(ctx, s, body)...)
	}
	if _, authorable := workshopKindTable[a.Kind]; authorable && localClean {
		diags = append(diags, s.dryRunDiagnostics(ctx, a.Kind, body)...)
	}
	return s.jsonResult(map[string]any{
		"kind":        a.Kind,
		"valid":       !hasErrorDiagnostic(diags),
		"diagnostics": diags,
	})
}

// manifestBytes decodes validate_spec's raw "manifest" argument into the
// bytes a Validator's ValidateFile should see. The MCP wire only ever
// carries JSON, so a manifest supplied as an object arrives as JSON bytes
// (already valid YAML, since JSON is a YAML subset) and is used as-is; one
// supplied as a JSON string (the caller pasted a raw YAML document) is
// unwrapped first so the STRING'S CONTENTS are validated, not its quoting.
func manifestBytes(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	// A missing "manifest" key decodes to a nil/empty RawMessage; an explicit
	// "manifest": null (json.RawMessage's own zero-value marshaling, e.g. from
	// a struct literal with no Manifest set) arrives as the 4-byte literal
	// "null" instead — both mean the same thing here, so both fail closed with
	// the same clear message rather than one of them silently reaching a
	// validator with an empty candidate.
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, fmt.Errorf("manifest is required")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("decode manifest string: %w", err)
		}
		if len(bytes.TrimSpace([]byte(s))) == 0 {
			return nil, fmt.Errorf("manifest is required")
		}
		return []byte(s), nil
	}
	return trimmed, nil
}

// validateCandidate dispatches kind to its real contract.Validator and
// returns every contract.Diagnostic from validating raw — the candidate's
// spec content, round-tripped through a temp file exactly the way
// `oap tools validate -f <file>` reads one from disk. A kind the workshop can
// author but that has no local validator returns no diagnostics here: the
// server half (dryRunDiagnostics) is its check. A kind known to neither
// table fails closed with a single error Diagnostic naming it, never a
// silent pass. A ValidateFile call that itself returns a Go error (as
// opposed to returning it as a Diagnostic — sidecartoolbox.Kind.ValidateFile
// is the one impl with a path that can, via os.ReadFile) is likewise
// surfaced as an error Diagnostic rather than dropped, per CLAUDE.md's
// never-silently-drop-an-error rule.
func validateCandidate(kind string, raw []byte) []contract.Diagnostic {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []contract.Diagnostic{{Severity: "error", Path: "manifest", Message: "manifest is required"}}
	}
	var extra []contract.Diagnostic
	if check, ok := extraChecksByKind[kind]; ok {
		extra = check(raw)
	}

	v, ok := validateSpecKindTable[kind]
	if !ok {
		if _, authorable := workshopKindTable[kind]; authorable {
			return append([]contract.Diagnostic{}, extra...)
		}
		return []contract.Diagnostic{{
			Severity: "error",
			Path:     "kind",
			Message:  fmt.Sprintf("unknown or unsupported kind %q (known: %v)", kind, workshopKindNames()),
		}}
	}

	path, cleanup, err := writeValidateTemp(raw)
	if err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: "manifest", Message: err.Error()}}
	}
	defer cleanup()

	diags, err := v.ValidateFile(path)
	if err != nil {
		diags = append(diags, contract.Diagnostic{Severity: "error", Path: "manifest", Message: err.Error()})
	}
	if diags == nil {
		diags = []contract.Diagnostic{}
	}
	return append(diags, extra...)
}

// dryRunDiagnostics is validate_spec's server half: the exact server-side
// apply workshop_apply would make (same field owner, same forced ownership,
// same group stamped from the kind), sent with DryRunAll so the apiserver
// runs the CRD's schema, its validation rules and admission, and persists
// nothing. Its refusal comes back verbatim as one error Diagnostic under
// Path "server" — the same words the real apply would have failed with, and
// the same words the builder used to buy one approval card at a time. A
// Server with no cluster client (the local-only validator tests build one)
// cannot run this half and says so with a warning rather than pretending the
// candidate passed a check that never happened.
func (s *Server) dryRunDiagnostics(ctx context.Context, kind string, raw []byte) []contract.Diagnostic {
	if s.K8s == nil {
		return []contract.Diagnostic{{Severity: "warning", Path: "server",
			Message: "server-side dry run skipped: no cluster client; the real apply may still refuse this candidate"}}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: "manifest", Message: fmt.Sprintf("decode manifest for the dry run: %v", err)}}
	}
	obj := candidateObject(kind, doc, s.Identity.Namespace)
	if err := s.K8s.Patch(ctx, obj, client.Apply, client.FieldOwner(s.FieldOwner), client.ForceOwnership, client.DryRunAll); err != nil {
		return []contract.Diagnostic{{Severity: "error", Path: "server", Message: err.Error()}}
	}
	return nil
}

// candidateObject shapes doc as the object apply would send. A document that
// carries a spec is a full resource and is taken as apply takes it — its
// apiVersion replaced by the kind's, its namespace forced to the workshop's.
// Anything else is spec content in the local-file shape (the same bytes the
// local half validated) and is wrapped under spec; a stray kind/apiVersion/
// metadata at that level is dropped rather than nested into the spec. SSA
// needs a name, so spec-only content gets a placeholder: the dry run creates
// nothing, and the name only has to be a legal one.
func candidateObject(kind string, doc map[string]any, ns string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	if _, full := doc["spec"]; full {
		obj.Object = doc
	} else {
		delete(doc, "apiVersion")
		delete(doc, "kind")
		delete(doc, "metadata")
		obj.Object = map[string]any{"spec": doc}
	}
	obj.SetGroupVersionKind(spiceboxv1alpha1.SchemeGroupVersion.WithKind(kind))
	if obj.GetName() == "" {
		obj.SetName("validate-candidate")
	}
	obj.SetNamespace(ns)
	return obj
}

// writeValidateTemp writes body to a fresh temp file for a ValidateFile call
// to read, returning its path and a cleanup func the caller must defer. The
// cleanup func is always safe to call, even when writeValidateTemp itself
// returned an error, so a defer right after the call is always correct.
func writeValidateTemp(body []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "workshop-validate-*.yaml")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp file for validation: %w", err)
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("writing candidate to temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("closing candidate temp file: %w", err)
	}
	return f.Name(), cleanup, nil
}

// hasErrorDiagnostic reports whether diags carries at least one
// error-severity Diagnostic — validate_spec's "valid" convenience field.
func hasErrorDiagnostic(diags []contract.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}
