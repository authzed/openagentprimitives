package validator

import (
	"fmt"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// WarnSensitiveFieldPathMalformed is the Warning.Kind recorded when a
// sensitiveFields entry in the spec does not parse. The redactor still fails
// closed (see redact.RedactValueAtPath), so the verdict is unaffected — but the
// declared path never resolved, and without this the only symptom would be a
// whole argument object collapsing into one token for no visible reason.
const WarnSensitiveFieldPathMalformed = "SensitiveFieldPathMalformed"

// registerSensitiveArgs walks tool.Args.SensitiveFields and, for every
// matching value in args, (1) registers every scalar leaf with the
// redactor so any literal occurrence is scrubbed from free-form text
// (Reason / Trace / FailedOn), and (2) replaces the whole value at that
// path with a single redaction token so NO byte of a declared-sensitive
// value survives in the serialized Parsed.Args.
//
// args MUST be the redaction-safe Parsed copy, not the caller's raw
// Invocation.Args — this function rebinds the sensitive path to a token
// string and so mutates the map it is given.
//
// Why whole-value replacement: the Parsed-args scrub (core.RedactAny) runs
// RedactInString over *string* leaves only, so a number, bool, or non-string
// composite would serialize verbatim. Replacing the value outright closes that
// gap for every JSON shape. Leaf registration still matters, so the same secret
// appearing in a CEL message or trace detail is caught too.
//
// pkg/tools/redact.RedactValueAtPath does the walk and the replacement; its
// path syntax accepts array indices ("tags[2]") as well as dotted fields, and
// it reports a path that does not parse instead of quietly redacting an
// ancestor. That is recorded on the Decision as a Warning rather than returned
// through Check: Check's error return is reserved for faults that make a
// verdict impossible, and a malformed sensitiveFields entry is not one — the
// redactor has already failed closed. Denying every call to the tool over a
// typo would trade an over-broad audit record for an outage.
func registerSensitiveArgs(d *Decision, r *redact.Redactor, tool *mcpspec.Tool, args map[string]any) {
	if tool == nil {
		return
	}
	for _, path := range tool.Args.SensitiveFields {
		if err := redact.RedactValueAtPath(args, path, r, path); err != nil {
			d.Warnings = append(d.Warnings, Warning{
				Kind:    WarnSensitiveFieldPathMalformed,
				Message: fmt.Sprintf("tool %q: %s", tool.Name, err.Error()),
			})
		}
	}
}
