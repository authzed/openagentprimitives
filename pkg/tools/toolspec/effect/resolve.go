package effect

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// Resolve expands template strings in effects against the parsed Call and returns the resolved view.
func Resolve(e toolkit.Effects, call *parser.Call, env map[string]string, cwd string) Resolved {
	exp := expander{call: call, env: env, cwd: cwd}
	return Resolved{
		Destructive: e.Destructive,
		Reads:       append([]string(nil), e.Reads...),
		Writes:      append([]string(nil), e.Writes...),
		Network:     NetworkResolved{Destinations: exp.expandAll(e.Network.Destinations)},
		Filesystem:  FilesystemResolved{Paths: exp.expandAll(e.Filesystem.Paths)},
		Creds: CredsResolved{
			Required: append([]string(nil), e.Creds.Required...),
			Writes:   append([]string(nil), e.Creds.Writes...),
		},
	}
}

// unresolvedPrefix marks a destination/path entry whose template referenced
// something unknown or malformed (an authoring bug in the toolspec), as opposed
// to a well-formed reference to a recognized-but-absent optional value. The
// marker is a string no realistic allow-list hostname or filesystem prefix can
// equal or contain, so the validator's deny-unless-listed checks
// (notSubset / notUnderAny) flag it and the egress/filesystem gate fails CLOSED
// instead of vacuously allowing a silently-dropped entry.
const unresolvedPrefix = "\x00<unresolved:"

// IsUnresolved reports whether a resolved destination/path is the force-deny
// sentinel emitted for an unknown/malformed effect template. Such entries must
// never be treated as a satisfied allow-list match.
func IsUnresolved(s string) bool {
	return strings.HasPrefix(s, unresolvedPrefix)
}

type expander struct {
	call *parser.Call
	env  map[string]string
	cwd  string
}

func (ex expander) expandAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		v, outcome := ex.expand(s)
		// dropEntry: a well-formed reference to a recognized but genuinely-unset
		// optional value (e.g. an unset {flag:x}). Dropping it is legitimate.
		// resolved / forceDeny: keep the entry. forceDeny carries the sentinel so
		// the allow-list check denies it (fail closed) rather than vacuously
		// passing on an empty set.
		if outcome == dropEntry {
			continue
		}
		out = append(out, v)
	}
	return out
}

// expandOutcome classifies the result of resolving a single template string.
type expandOutcome int

const (
	// resolved: the string was produced by resolving all templates against
	// present values (or had no templates).
	resolved expandOutcome = iota
	// dropEntry: the string referenced a recognized-but-absent optional value;
	// the entry is legitimately omitted from the resolved effects.
	dropEntry
	// forceDeny: the string referenced an unknown/malformed value (a toolspec
	// authoring bug); the returned value is the un-allowlistable sentinel.
	forceDeny
)

// expand processes a single template string, classifying the result so the
// caller can distinguish a legitimate optional-unset drop from a misauthored
// template that must fail closed.
func (ex expander) expand(s string) (string, expandOutcome) {
	// Fast path: no template markers.
	if !strings.ContainsAny(s, "{") {
		return s, resolved
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '{' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// find closing brace
		end := strings.IndexByte(s[i+1:], '}')
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		tmpl := s[i+1 : i+1+end]
		i = i + 1 + end + 1
		val, ok := ex.resolveTmpl(tmpl)
		if !ok {
			// A recognized-but-absent optional resolves to "" (drop the whole
			// entry — legitimate). A malformed/unknown reference resolves to the
			// sentinel (force-deny the whole entry — fail closed).
			if IsUnresolved(val) {
				return val, forceDeny
			}
			return "", dropEntry
		}
		b.WriteString(val)
	}
	return b.String(), resolved
}

// resolveTmpl resolves a single {kind:key} (or {cwd}) template.
//
// Returns:
//   - (value, true): resolved from a present value.
//   - ("", false): a recognized kind whose value is genuinely absent (unset
//     optional) — the caller drops the entry, which is legitimate.
//   - (sentinel, false): an unknown kind or malformed/colon-less template — a
//     toolspec authoring bug — so the caller force-denies the entry rather than
//     silently dropping it and failing the allow-list open.
func (ex expander) resolveTmpl(tmpl string) (string, bool) {
	if tmpl == "cwd" {
		// cwd is a recognized source; an empty cwd is an absent optional, not a
		// malformed reference.
		return ex.cwd, ex.cwd != ""
	}
	colon := strings.IndexByte(tmpl, ':')
	if colon < 0 {
		// Malformed: no kind:key separator. Force-deny.
		return ex.sentinel(tmpl), false
	}
	kind, key := tmpl[:colon], tmpl[colon+1:]
	switch kind {
	case "flag":
		if ex.call == nil {
			return "", false
		}
		v, ok := ex.call.Flags[key]
		if !ok {
			return "", false
		}
		return fmt.Sprint(v), true
	case "env":
		v, ok := ex.env[key]
		if !ok {
			return "", false
		}
		return v, true
	case "positional":
		if ex.call == nil {
			return "", false
		}
		v, ok := ex.call.Positional[key]
		if !ok {
			return "", false
		}
		return fmt.Sprint(v), true
	}
	// Unknown kind. Force-deny.
	return ex.sentinel(tmpl), false
}

func (ex expander) sentinel(tmpl string) string {
	return unresolvedPrefix + tmpl + ">"
}
