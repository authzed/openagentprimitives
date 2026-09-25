package spec

import (
	"fmt"
	"strings"
)

// ValidateRequiredFlagConstraints refuses a spec that admits subcommands
// reachable with a flag the toolkit declared RequiresConstraint, but says
// nothing about that flag.
//
// requiredKeys comes from toolkit.Toolkit.FlagsRequiringConstraint(). An empty
// list is an allow: a toolkit that declares nothing dangerous costs every spec
// written against it nothing.
//
// Why this is a refusal and not a lint: allowSubcommands narrows the
// SUBCOMMANDS a spec admits and leaves every declared global flag reachable,
// so for a flag like git's `-c` the narrowing buys nothing. A spec listing
// only read subcommands still yields arbitrary command execution, which is
// what makes the omission worth failing on rather than warning about.
//
// The coverage test is textual — does any constraint expression mention the
// flag's key — and that is deliberately crude. It catches the case this exists
// for, which is an author who did not think about the flag at all; it cannot
// stop an author who writes a permissive constraint on purpose, and nothing
// short of interpreting the CEL could. Being honest about that is better than
// implying a guarantee the check does not make.
func ValidateRequiredFlagConstraints(requiredKeys []string, sp *Spec) error {
	if len(requiredKeys) == 0 || sp == nil || len(sp.AllowSubcommands) == 0 {
		return nil
	}
	var uncovered []string
	for _, key := range requiredKeys {
		if key == "" || constraintsMention(sp.Constraints, key) {
			continue
		}
		uncovered = append(uncovered, key)
	}
	if len(uncovered) == 0 {
		return nil
	}
	return fmt.Errorf(
		"spec %q admits subcommands reachable with %s, which the toolkit declares must be constrained: "+
			"add a constraint bounding the value, e.g. "+
			"!call.hasFlag('%s') || call.flags['%s'].all(v, v.startsWith('...'))",
		sp.Name, quotedList(uncovered), uncovered[0], uncovered[0])
}

// constraintsMention reports whether any constraint expression addresses the
// flag by its key, in either of the two forms a spec can use to reach one:
// call.hasFlag('key') and call.flags['key'].
func constraintsMention(constraints []Constraint, key string) bool {
	for _, c := range constraints {
		if strings.Contains(c.CEL, "'"+key+"'") || strings.Contains(c.CEL, `"`+key+`"`) {
			return true
		}
	}
	return false
}

func quotedList(keys []string) string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, "-"+k)
	}
	return strings.Join(out, ", ")
}
