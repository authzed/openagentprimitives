package validator

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// resolveResourceID returns the canonical resource-instance id this call names,
// or "" when the call names no argument-derived external instance (a
// workspace/self sentinel template, an unresolvable id, or a subcommand with no
// Check). It is the value bound as call.resourceId for constraint CEL, resolved
// through the SAME machinery the authz permission path uses (checkForCall +
// the check's resourceIDExpr/Template + transforms) so the two agree on which
// repository a call reaches.
//
// It applies every canonicalizing transform, but with the SpiceDB-object-id
// ENCODINGS swapped for their plain-text equivalents (see celTransforms):
// spicedb_escape is dropped and github_repo_url_id becomes github_repo_id.
// call.resourceId is a CEL policy value, not a SpiceDB key, so it stays the
// canonical human form (e.g. https://github.com/owner/repo) a toolspec author
// can write a readable allowlist against — never a `=3A`/`=2E`-escaped or
// base64url-encoded object key. github_repo_id and normalize_url run as-is.
//
// Only argument-derived ids gate: a static-literal ResourceIDTemplate
// (no `{...}` placeholder, e.g. "workspace"/"self") names no external instance,
// so it is reported as "" and a config-driven allowlist leaves such local ops
// alone.
func resolveResourceID(tk *toolkit.Toolkit, call *parser.Call) string {
	check, ok := checkForCall(tk, call)
	if !ok {
		return ""
	}
	if check.ResourceIDExpr == "" && !hasTemplatePlaceholder(check.ResourceIDTemplate) {
		return ""
	}
	args := namedArgs(call)
	raw, err := authz.RawResourceID(check, args)
	if err != nil || raw == "" {
		// Unresolvable or empty (e.g. clone of a non-URL): not gated here. A
		// genuinely broken Check is an authoring bug surfaced by the permission
		// path, not this best-effort exposure. "" leaves the call ungated by
		// resourceId-based constraints (which themselves treat "" as "no
		// instance").
		return ""
	}
	id, err := authz.ApplyTransforms(raw, celTransforms(check.ResourceIDTransforms))
	if err != nil {
		return ""
	}
	return id
}

// celTransforms is the check's transform chain adjusted for a CEL policy value:
// the SpiceDB-object-id ENCODINGS are removed or replaced by their plain-text
// equivalents, so call.resourceId is the canonical HUMAN form a toolspec author
// writes an allowlist against — not a cryptic object key. See resolveResourceID.
//
//   - spicedb_escape is dropped: it exists only to make an id a legal SpiceDB
//     object id (=3A/=2E escaping), which a CEL value never needs.
//   - github_repo_url_id is replaced by github_repo_id: the former base64url-
//     encodes the canonical https URL to key the github_repo_url SpiceDB object
//     (transforms.go), so a call gated by it would expose call.resourceId as
//     "aHR0cHM6Ly9naXRodWIuY29t…" — which no readable "owner/*" allowlist can
//     match. github_repo_id is the SAME canonical fold WITHOUT the encoding, so
//     a gh call (`gh … --repo owner/repo`, resourceIDTransforms:[github_repo_url_id])
//     yields the same plain https://github.com/owner/repo the git toolkit's
//     normalize_url'd clone URL does, and one config.allowedRepos pattern gates
//     both. Without this, the repo allowlist silently denied EVERY gh repo call
//     (a reviewbot could clone via git but never `gh pr view --repo`).
func celTransforms(ts []string) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		switch t {
		case "spicedb_escape":
			continue
		case "github_repo_url_id":
			out = append(out, "github_repo_id")
		default:
			out = append(out, t)
		}
	}
	return out
}

// checkForCall picks the PermissionCheck governing this parsed call: a matching
// argument variant first, then the subcommand's own permission, then the
// toolkit default. Mirrors SandboxTool.PermissionForCall's selection order.
func checkForCall(tk *toolkit.Toolkit, call *parser.Call) (authz.PermissionCheck, bool) {
	sc := findSubcommand(tk, call.SubcommandPath)
	if sc != nil && len(sc.PermissionVariants) > 0 {
		if got, matched, err := authz.ResolveVariant(sc.PermissionVariants, namedArgs(call)); err == nil && matched && got.Check != nil {
			return *got.Check, true
		}
	}
	if sc != nil && sc.Permission != nil && sc.Permission.Check != nil {
		return *sc.Permission.Check, true
	}
	if tk.Permission != nil && tk.Permission.Check != nil {
		return *tk.Permission.Check, true
	}
	return authz.PermissionCheck{}, false
}

// namedArgs renders a parsed call as the named-argument map a Check reads
// (positional then flags; a flag wins a name clash), mirroring
// SandboxTool.NamedArgs. The subcommand is included so a Check may key on it.
func namedArgs(call *parser.Call) map[string]any {
	out := make(map[string]any, len(call.Flags)+len(call.Positional)+1)
	for k, v := range call.Positional {
		out[k] = v
	}
	for k, v := range call.Flags {
		out[k] = v
	}
	out["subcommand"] = call.Subcommand
	return out
}

// hasTemplatePlaceholder reports whether a ResourceIDTemplate references any
// argument (a `{...}` placeholder). A template with none is a static literal
// (a sentinel like "workspace"), which names no argument-derived instance.
func hasTemplatePlaceholder(tmpl string) bool {
	return strings.Contains(tmpl, "{")
}
