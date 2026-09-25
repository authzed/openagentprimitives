package toolkits_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

var templateRef = regexp.MustCompile(`\{(\w+)\}`)

// A check that CAN fail to resolve must say what the caller should have written.
//
// This was fixed once, for git's remote-naming subcommands, and the same defect
// was sitting in gh eighteen times: `{repo}` names an OPTIONAL --repo flag, so
// `gh pr view 123` without it produced `internal: authz: template references arg
// "repo" which is not present` — a message that reads as a system fault and
// tells the agent not to retry, when the fix is one flag away.
//
// Stated over every builtin toolkit rather than per-toolkit, because that is how
// it recurred: the git fix taught the lesson and gh never learned it.
//
// A template referencing only REQUIRED positionals always resolves, so it needs
// no hint — the parser rejects the call before authorization sees it.
func TestBuiltinToolkits_everyCheckThatCanFailToResolveCarriesAHint(t *testing.T) {
	for _, tk := range toolkits.All() {
		for i := range tk.Subcommands {
			sc := &tk.Subcommands[i]
			if sc.Permission == nil || sc.Permission.Check == nil {
				continue
			}
			c := sc.Permission.Check
			label := tk.Name + " " + strings.Join(sc.Path, " ")

			assert.True(t, c.ResourceIDTemplate != "" || c.ResourceIDExpr != "",
				"`%s`: a check with neither an id template nor an expr cannot resolve", label)

			if !canFailToResolve(sc, c.ResourceIDTemplate, c.ResourceIDExpr) {
				continue
			}
			assert.NotEmpty(t, c.ResourceIDHint,
				"`%s` can fail to resolve its %s id, and would then deny with an "+
					"`internal:` message the agent cannot act on", label, c.ResourceType)
		}
	}
}

// canFailToResolve reports whether a check's id source can come back empty for a
// call the parser would otherwise accept.
//
//   - an EXPR can always refuse (that is what the guards are for)
//   - a TEMPLATE referencing anything that is not a required positional can be
//     absent at call time — an optional positional, or any flag
func canFailToResolve(sc *toolkit.Subcommand, tmpl, expr string) bool {
	if expr != "" {
		return true
	}
	required := map[string]bool{}
	for _, p := range sc.Positional {
		if p.Required {
			required[p.Name] = true
		}
	}
	for _, m := range templateRef.FindAllStringSubmatch(tmpl, -1) {
		if !required[m[1]] {
			return true
		}
	}
	return false
}
