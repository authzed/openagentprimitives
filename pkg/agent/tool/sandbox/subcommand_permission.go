package sandbox

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// PermissionVariants exposes each subcommand's declared permission.
//
// A toolspec that allows several subcommands synthesizes ONE tool for the whole
// CLI, and that tool's base permission is the toolkit-level default. Every
// per-subcommand check the toolkit declares was therefore discarded before it
// reached the runtime: `gh pr create` (external, github_repo#write) and
// `gh pr view` (readonly, github_repo#read) authorized identically, as
// passthrough. Observed live — an agent whose gh tools carry real checks
// enumerated a permission surface of ZERO.
//
// Variants are what the permission SURFACE reads (tool.Candidates collects
// Permission + PermissionVariants), so emitting them is what makes those
// handles declarable in a plan at all. The `When` expression is recorded as
// provenance and is a correct fallback, but dispatch prefers PermissionForCall
// below — see its comment for why the CEL is not the authority.
func (s *SandboxTool) PermissionVariants() []authz.PermissionVariant {
	tk := s.opts.Toolkit
	if tk == nil {
		return nil
	}
	var out []authz.PermissionVariant
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if sc.Permission == nil {
			continue // inherits the toolkit default; the base permission covers it
		}
		if !s.allowsSubcommand(sc) {
			continue // narrowed out by the toolspec; unreachable, so not on the surface
		}
		out = append(out, authz.PermissionVariant{
			When:  subcommandWhen(sc.Path),
			Check: *sc.Permission,
		})
	}

	// Argument-level variants reach the surface too, or the cheap reading is not
	// declarable: a phase could not say "I will only GET" and every plan would
	// have to ask for the fallback's authority to make a read.
	//
	// Second loop rather than folding into the one above, because a subcommand
	// may declare variants and NO permission of its own — it inherits the
	// toolkit default as its fallback, and the loop above skips it.
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if !s.allowsSubcommand(sc) {
			continue
		}
		for _, v := range sc.PermissionVariants {
			out = append(out, authz.PermissionVariant{
				// The recorded When is the SUBCOMMAND predicate, not the
				// variant's own: this is provenance for the surface, and
				// dispatch resolves through PermissionForCall regardless.
				When:  subcommandWhen(sc.Path),
				Check: v.Check,
			})
		}
	}
	return out
}

// PermissionForCall resolves the permission for ONE call's argv.
//
// Dispatch uses this rather than the variants' CEL because the toolkit's own
// parser is the authority on which subcommand an argv selects — it already
// skips recognized leading global flags (`git -C /repo clone …`). Re-deriving
// that in CEL would be a second implementation of argv parsing living in an
// authorization path, free to disagree with the first. When two things must
// agree about what a call IS, they should not be two things.
//
// ok=false only when this tool has no toolkit, which is a test shape rather
// than a live one. An argv matching no subcommand, or one whose subcommand
// declares no permission of its own, resolves to the base permission — the
// same fallback resolvePermission applies at construction.
func (s *SandboxTool) PermissionForCall(args map[string]any) (authz.Permission, bool) {
	if s.opts.Toolkit == nil {
		return authz.Permission{}, false
	}
	sc := parser.IdentifySubcommand(s.opts.Toolkit, argvFrom(args))
	if sc == nil {
		return s.permission, true
	}

	// Argument-level variants, resolved against the PARSED args.
	//
	// Authority is not always a property of which subcommand ran: `gh api -X
	// GET repos/o/n` reads and `gh api -X POST repos/o/n/pulls` opens a pull
	// request, and `claude --dangerously-skip-permissions` is the same shape on
	// a CLI with no subcommands at all.
	//
	// NamedArgs, not the raw argv, and never re-deriving the subcommand in CEL:
	// the toolkit's parser is the authority on what a call IS (it already skips
	// recognized global flags), and a second argv parser living in an
	// authorization predicate is free to disagree with the first.
	//
	// A variant that fails to resolve falls through to the subcommand's own
	// permission rather than being skipped silently: an unparseable predicate is
	// an authoring error, and the fallback is the strict reading by convention
	// (variants name what is SAFE), so failing into it cannot widen authority.
	if len(sc.PermissionVariants) > 0 {
		if named, ok := s.NamedArgs(args); ok {
			if got, matched, err := authz.ResolveVariant(sc.PermissionVariants, named); err != nil {
				slog.Default().Info("permission variant did not resolve; using the subcommand's own permission",
					"tool", s.Name(), "subcommand", strings.Join(sc.Path, " "), "err", err.Error())
			} else if matched {
				return got, true
			}
		}
	}

	if sc.Permission == nil {
		return s.permission, true
	}
	return *sc.Permission, true
}

// allowsSubcommand reports whether the toolspec's narrowing admits sc.
//
// A subcommand the spec excluded cannot be called, so its permission is not on
// this session's surface — listing it would invite a plan to declare authority
// the agent could never exercise, and a human to approve it.
func (s *SandboxTool) allowsSubcommand(sc *toolkit.Subcommand) bool {
	if s.opts.Spec == nil || len(s.opts.Spec.AllowSubcommands) == 0 {
		return true // no narrowing declared: the whole toolkit is reachable
	}
	want := strings.Join(sc.Path, " ")
	for _, a := range s.opts.Spec.AllowSubcommands {
		if strings.TrimSpace(a) == want {
			return true
		}
	}
	return false
}

// subcommandWhen renders the CEL that selects a subcommand from an argv.
//
// Sandbox args are an argv ARRAY, not named fields, so the envelope reaches the
// evaluator whole and the array is at `args.args` (unwrapEnvelope only unwraps
// when the inner value is a map). Leading flags are filtered out to approximate
// the parser's global-flag skip; the approximation is acceptable here precisely
// because it is NOT the authority — PermissionForCall is.
func subcommandWhen(path []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("size(args.args.filter(a, !a.startsWith(\"-\"))) >= %d", len(path)))
	for i, p := range path {
		fmt.Fprintf(&b, " && args.args.filter(a, !a.startsWith(\"-\"))[%d] == %q", i, p)
	}
	return b.String()
}

// argvFrom pulls the argv out of a decoded tool-call envelope.
func argvFrom(args map[string]any) []string {
	raw, _ := args["args"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// NamedArgs renders this call's argv as the NAMED arguments a permission check
// reads.
//
// A check declaring `resourceIDTemplate: "{repo}"` resolves that key out of a
// map. MCP tools already arrive with named arguments; a sandbox tool arrives
// with an argv ARRAY, so the template found nothing and every per-resource
// check on a sandbox tool failed with `template references arg "repo" which is
// not present`. The permission resolved correctly and the RESOURCE did not,
// which is the half that decides WHICH repo — and therefore what a slot could
// bind.
//
// The parser already produces this view: Call.Flags is keyed by long name and
// Call.Positional by name. Nothing needed inventing; it simply was not handed
// to the check. Using the authoritative validating Parse (not
// IdentifySubcommand) is deliberate — a check must resolve against arguments
// that actually parse, not against a best-effort reading of them.
//
// ok=false when the argv does not parse. A partial named view is worse than
// none here: the check would resolve against arguments the call never made,
// and authorize a resource nobody named.
func (s *SandboxTool) NamedArgs(args map[string]any) (map[string]any, bool) {
	if s.opts.Toolkit == nil {
		return nil, false
	}
	call, err := (&parser.Declarative{}).Parse(s.opts.Toolkit, argvFrom(args))
	if err != nil || call == nil {
		return nil, false
	}
	out := make(map[string]any, len(call.Flags)+len(call.Positional)+1)
	for k, v := range call.Positional {
		out[k] = v
	}
	// Flags last: a flag and a positional sharing a name is a toolkit-authoring
	// mistake, and the explicit flag is the better guess at intent.
	for k, v := range call.Flags {
		out[k] = v
	}
	// The subcommand, so a check can key on it without re-splitting argv.
	out["subcommand"] = call.Subcommand
	return out, true
}
