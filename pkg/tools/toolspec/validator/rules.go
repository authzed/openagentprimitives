package validator

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/effect"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// ruleDeny names a structured rule that fired, with a human-readable message.
type ruleDeny struct {
	Path    string
	Message string
}

// evaluateStructuredRules runs deny.effects.* and allow.* against the resolved call effects.
// Returns the list of rule paths that would deny; caller decides whether exceptions lift them.
func evaluateStructuredRules(sp *spec.Spec, r effect.Resolved) []ruleDeny {
	var denies []ruleDeny

	if sp.Deny.Effects.Destructive && r.Destructive {
		denies = append(denies, ruleDeny{
			Path:    "deny.effects.destructive",
			Message: "destructive operations are not permitted",
		})
	}
	if inter := intersect(sp.Deny.Effects.Reads, r.Reads); len(inter) > 0 {
		denies = append(denies, ruleDeny{
			Path:    "deny.effects.reads",
			Message: fmt.Sprintf("reads %v are denied", inter),
		})
	}
	if inter := intersect(sp.Deny.Effects.Writes, r.Writes); len(inter) > 0 {
		denies = append(denies, ruleDeny{
			Path:    "deny.effects.writes",
			Message: fmt.Sprintf("writes %v are denied", inter),
		})
	}
	if sp.Deny.Effects.Creds.Writes && len(r.Creds.Writes) > 0 {
		denies = append(denies, ruleDeny{
			Path:    "deny.effects.creds.writes",
			Message: fmt.Sprintf("credential writes %v are denied", r.Creds.Writes),
		})
	}

	if sp.Allow.Network.Set {
		if extra := notSubset(r.Network.Destinations, sp.Allow.Network.Destinations); len(extra) > 0 {
			denies = append(denies, ruleDeny{
				Path:    "allow.network.destinations",
				Message: fmt.Sprintf("network destinations %v not in allow list %v", extra, sp.Allow.Network.Destinations),
			})
		}
	}
	if sp.Allow.Filesystem.Set {
		if extra := notUnderAny(r.Filesystem.Paths, sp.Allow.Filesystem.PathsUnder); len(extra) > 0 {
			denies = append(denies, ruleDeny{
				Path:    "allow.filesystem.pathsUnder",
				Message: fmt.Sprintf("filesystem paths %v not under %v", extra, sp.Allow.Filesystem.PathsUnder),
			})
		}
	}
	if sp.Allow.Creds.Set {
		if extra := notSubset(r.Creds.Required, sp.Allow.Creds.Required); len(extra) > 0 {
			denies = append(denies, ruleDeny{
				Path:    "allow.creds.required",
				Message: fmt.Sprintf("required creds %v not in allow list %v", extra, sp.Allow.Creds.Required),
			})
		}
	}
	return denies
}

func intersect(a, b []string) []string {
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	var out []string
	for _, y := range b {
		if m[y] {
			out = append(out, y)
		}
	}
	return out
}

func notSubset(actual, allowed []string) []string {
	m := map[string]bool{}
	for _, a := range allowed {
		m[a] = true
	}
	var out []string
	for _, a := range actual {
		if !m[a] {
			out = append(out, a)
		}
	}
	return out
}

func notUnderAny(paths, prefixes []string) []string {
	var out []string
	for _, p := range paths {
		ok := false
		for _, pre := range prefixes {
			if p == pre || strings.HasPrefix(p, strings.TrimSuffix(pre, "/")+"/") {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, p)
		}
	}
	return out
}
