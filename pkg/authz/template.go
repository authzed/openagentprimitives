package authz

import (
	"fmt"
	"strings"
)

// ResolveResourceID returns the resource id a PermissionCheck names for
// one tool call's args. Dispatches between the slice-1
// template-and-transforms form (ResourceIDTemplate + ResourceIDTransforms)
// and the slice-4 CEL form (ResourceIDExpr). Exactly one form is expected
// to be set; the caller's validation ensures that.
func ResolveResourceID(c PermissionCheck, args map[string]any) (string, error) {
	if c.ResourceIDExpr != "" {
		prg, err := CompileString(c.ResourceIDExpr)
		if err != nil {
			return "", fmt.Errorf("resourceIDExpr compile: %w", err)
		}
		s, err := EvalString(prg, args)
		if err != nil {
			return "", err
		}
		return ApplyTransforms(s, c.ResourceIDTransforms)
	}
	return ResolveTemplate(c.ResourceIDTemplate, args, c.ResourceIDTransforms)
}

// RawResourceID returns the resource id a PermissionCheck names for one tool
// call's args WITHOUT running the check's ResourceIDTransforms — the
// pre-transform provider identifier, which is what a recorded fact is keyed by.
//
// Facts are written under the raw id (see precondition.LookupSubject and
// factcontent.ForSubject, which state the same rule at the storage layer), while
// ResourceIDTransforms exist to turn that value into something SpiceDB will
// accept as an object id — `spicedb_escape` is there because `#` is illegal in
// one. So a fact lookup keyed by the transformed form cannot ever match, and it
// fails as an EMPTY result, which reads as "nothing has been observed": the gate
// holds closed forever with nothing anywhere reporting a fault.
//
// c is taken BY VALUE, so clearing the transform chain is local to this call and
// the caller's check is untouched. Both branches of ResolveResourceID are
// therefore covered by one line, rather than by a second copy of the
// template-vs-CEL dispatch that would have to be kept in step with it.
func RawResourceID(c PermissionCheck, args map[string]any) (string, error) {
	c.ResourceIDTransforms = nil
	return ResolveResourceID(c, args)
}

// ResolveTemplate substitutes {arg} placeholders in tmpl with values
// from args, runs the named transforms in order, and returns the
// final string. Returns an error for any {arg} not present in args.
//
// Escape: "{{" produces a literal "{". This is rarely needed; mostly
// here so a pathological resource ID containing a literal "{" can
// still be expressed.
func ResolveTemplate(tmpl string, args map[string]any, transforms []string) (string, error) {
	var b strings.Builder
	b.Grow(len(tmpl))
	i := 0
	for i < len(tmpl) {
		c := tmpl[i]
		if c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{' {
			// "{{" → literal "{"
			b.WriteByte('{')
			i += 2
			continue
		}
		if c != '{' {
			b.WriteByte(c)
			i++
			continue
		}
		// Scan to the matching '}'.
		end := strings.IndexByte(tmpl[i+1:], '}')
		if end < 0 {
			return "", fmt.Errorf("authz: unterminated template placeholder at offset %d in %q", i, tmpl)
		}
		name := tmpl[i+1 : i+1+end]
		v, ok := args[name]
		if !ok {
			return "", fmt.Errorf("authz: template references arg %q which is not present", name)
		}
		b.WriteString(stringify(v))
		i += end + 2
	}
	return ApplyTransforms(b.String(), transforms)
}

// ExtractTemplateRefs returns the unique {arg} names referenced in
// tmpl, in first-occurrence order. Used by AgentClass validation to
// verify every referenced arg exists on the tool's input schema.
// Escape sequences ("{{") are not collected.
func ExtractTemplateRefs(tmpl string) []string {
	seen := map[string]bool{}
	var refs []string
	i := 0
	for i < len(tmpl) {
		c := tmpl[i]
		if c == '{' && i+1 < len(tmpl) && tmpl[i+1] == '{' {
			i += 2
			continue
		}
		if c != '{' {
			i++
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '}')
		if end < 0 {
			return refs
		}
		name := tmpl[i+1 : i+1+end]
		if !seen[name] {
			seen[name] = true
			refs = append(refs, name)
		}
		i += end + 2
	}
	return refs
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}
