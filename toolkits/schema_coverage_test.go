package toolkits_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// A toolkit's checks name a resourceType and a permission. Nothing defined
// those types: `toolkits/gh.yaml` gates `pr view` on github_repo#read, and no
// shipped manifest declared github_repo, so a cluster running a gh-tooled agent
// under toolCalls.mode=enforcing got `object definition "github_repo" not found`
// on every gated call — the check resolved an id and then had nowhere to ask.
//
// The toolkit carries the definition now, the same way an MCPServer carries the
// schema fragment for the types ITS tools name. This test is what keeps the two
// halves from drifting: add a subcommand gated on a new permission and the
// toolkit must grow that permission, or this fails naming both sides.
func TestBuiltinToolkits_defineEveryResourceTypeTheirChecksName(t *testing.T) {
	for _, tk := range toolkits.All() {
		t.Run(tk.Name, func(t *testing.T) {
			defined := definedPermissions(tk)

			for _, want := range checkedPermissions(tk) {
				perms, ok := defined[want.resourceType]
				require.True(t, ok,
					"%s gates %s on %s#%s, but the toolkit defines no %q resource — "+
						"a cluster running it gets `object definition %q not found`",
					tk.Name, want.where, want.resourceType, want.permission,
					want.resourceType, want.resourceType)

				assert.Contains(t, perms, want.permission,
					"%s gates %s on %s#%s, but %s defines only %v",
					tk.Name, want.where, want.resourceType, want.permission,
					want.resourceType, perms)
			}
		})
	}
}

// The converse, and the reason it is worth asserting separately: a permission
// defined but never checked is dead schema. It is not a fault the way a missing
// one is — it is how you notice a subcommand's check was removed or retyped and
// the definition kept a permission nothing reaches any more.
//
// A memory AUDIENCE permission (view_memory-shaped) is the deliberate
// exception: schema.IsMemoryAudiencePermission's own doc explains why — it is
// never named by a subcommand's PermissionCheck at all, because it is expanded
// live by LookupSubjects wherever the runner needs a pool's readership, not
// gated as an ordinary tool permission. Without this exemption every toolkit
// that ships one (github_pull_request#view_memory today) would read as dead
// schema.
func TestBuiltinToolkits_defineNoPermissionNoCheckNames(t *testing.T) {
	for _, tk := range toolkits.All() {
		t.Run(tk.Name, func(t *testing.T) {
			checked := map[string]bool{}
			for _, c := range checkedPermissions(tk) {
				checked[c.resourceType+"#"+c.permission] = true
			}
			for resourceType, perms := range definedPermissions(tk) {
				for _, p := range perms {
					if schema.IsMemoryAudiencePermission(p) {
						continue
					}
					assert.True(t, checked[resourceType+"#"+p],
						"%s defines %s#%s but no subcommand check names it",
						tk.Name, resourceType, p)
				}
			}
		})
	}
}

// checkedPermission is one (resourceType, permission) pair a toolkit's checks
// require, with where it came from for the failure message.
type checkedPermission struct {
	resourceType string
	permission   string
	where        string
}

// checkedPermissions collects every pair the toolkit's checks name — the
// toolkit-level default and each subcommand override.
func checkedPermissions(tk toolkit.Toolkit) []checkedPermission {
	var out []checkedPermission
	add := func(p *authz.Permission, where string) {
		if p == nil {
			return
		}
		if p.Check != nil {
			out = append(out, checkedPermission{p.Check.ResourceType, p.Check.Permission, where})
		}
	}
	add(tk.Permission, "the toolkit default")
	for _, sc := range tk.Subcommands {
		add(sc.Permission, "`"+tk.Name+" "+strings.Join(sc.Path, " ")+"`")
	}
	return out
}

// definedPermissions indexes the toolkit's own schema fragment by resource type.
func definedPermissions(tk toolkit.Toolkit) map[string][]string {
	out := map[string][]string{}
	if tk.SpiceDBSchema == nil {
		return out
	}
	for _, r := range tk.SpiceDBSchema.Resources {
		perms := make([]string, 0, len(r.Permissions))
		for _, p := range r.Permissions {
			perms = append(perms, p.Name)
		}
		sort.Strings(perms)
		out[r.Name] = perms
	}
	return out
}
