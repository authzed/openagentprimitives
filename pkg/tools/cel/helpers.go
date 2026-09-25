package cel

import (
	"net/url"
	"path/filepath"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// init registers every built-in helper into the package-level registry
// so Env / MCPEnv and the LLM authoring prompts see the same source of
// truth. Each entry pairs the CEL binding with the LLM-facing
// signature/doc the generator splices into its system prompt.
func init() {
	Register(Helper{
		Name:       "path.isUnder",
		Signature:  "path.isUnder(p string, prefix string) -> bool",
		Doc:        "True if p (after Clean) equals prefix or is contained under it. Use to constrain filesystem-path arguments.",
		Example:    `path.isUnder(call.flag("file"), "/work/shared")`,
		Scope:      ScopeToolspec,
		EnvOptions: []cel.EnvOption{pathHelpers()},
	})
	Register(Helper{
		Name:       "host.of",
		Signature:  "host.of(url string) -> string",
		Doc:        "Lowercase host component of url. Empty string if url doesn't parse.",
		Scope:      ScopeToolspec | ScopeMCP,
		EnvOptions: []cel.EnvOption{hostHelpers()},
	})
	Register(Helper{
		Name:      "host.matches",
		Signature: "host.matches(host string, pattern string) -> bool",
		Doc:       `True if host equals pattern, or pattern starts with "*." and host has that suffix (one or more labels).`,
		Example:   `host.matches(host.of(call.flag("url")), "*.internal")`,
		Scope:     ScopeToolspec | ScopeMCP,
		// host.* is one Lib bound to both signatures; the lib was
		// registered above on the host.of entry, so don't double-add.
	})
	Register(Helper{
		Name:       "glob.match",
		Signature:  "glob.match(pattern string, s string) -> bool",
		Doc:        "filepath.Match-style glob. Use for shell-style wildcards on resource names.",
		Scope:      ScopeToolspec | ScopeMCP,
		EnvOptions: []cel.EnvOption{globHelpers()},
	})
	Register(Helper{
		Name:       "semver.satisfies",
		Signature:  "semver.satisfies(version string, range string) -> bool",
		Doc:        `True if version satisfies a npm-style semver range (e.g. ">=2.40.0 <3.0.0").`,
		Scope:      ScopeToolspec | ScopeMCP,
		EnvOptions: []cel.EnvOption{semverHelpers()},
	})
	// call.* member overloads — scoped to Toolspec only; the MCP env
	// has no `call` variable, so binding them there would compile but
	// be useless / misleading in the LLM-facing docs.
	Register(Helper{
		Name:       "call.flag",
		Signature:  `call.flag(name string) -> string`,
		Doc:        `Value of flag name (long form). Empty string if the flag wasn't set on this call.`,
		Scope:      ScopeToolspec,
		EnvOptions: []cel.EnvOption{callMethodHelpers()},
	})
	Register(Helper{
		Name:      "call.hasFlag",
		Signature: `call.hasFlag(name string) -> bool`,
		Doc:       `True if the flag is present on this call (regardless of value).`,
		Scope:     ScopeToolspec,
		// callMethodLib bound above; no extra EnvOptions needed.
	})
	Register(Helper{
		Name:      "call.hasEnv",
		Signature: `call.hasEnv(name string) -> bool`,
		Doc:       `True if env variable name is present on this call.`,
		Scope:     ScopeToolspec,
	})
}

// --- path.* -----------------------------------------------------------------

func pathHelpers() cel.EnvOption {
	return cel.Function("path.isUnder",
		cel.Overload("path_isUnder_string_string",
			[]*cel.Type{cel.StringType, cel.StringType},
			cel.BoolType,
			cel.BinaryBinding(func(p, prefix ref.Val) ref.Val {
				ps, _ := p.Value().(string)
				pr, _ := prefix.Value().(string)
				return types.Bool(isUnder(ps, pr))
			}),
		),
	)
}

func isUnder(p, prefix string) bool {
	p = filepath.Clean(p)
	prefix = filepath.Clean(prefix)
	if p == prefix {
		return true
	}
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(p, prefix)
}

// --- host.* -----------------------------------------------------------------

func hostHelpers() cel.EnvOption {
	return cel.Lib(hostLib{})
}

type hostLib struct{}

func (hostLib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("host.of",
			cel.Overload("host_of_string",
				[]*cel.Type{cel.StringType},
				cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					s, _ := v.Value().(string)
					u, err := url.Parse(s)
					if err != nil {
						return types.String("")
					}
					return types.String(strings.ToLower(u.Host))
				}),
			),
		),
		cel.Function("host.matches",
			cel.Overload("host_matches_string_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(func(h, pat ref.Val) ref.Val {
					return types.Bool(hostMatches(h.Value().(string), pat.Value().(string)))
				}),
			),
		),
	}
}

func (hostLib) ProgramOptions() []cel.ProgramOption { return nil }

func hostMatches(h, pat string) bool {
	if pat == h {
		return true
	}
	if strings.HasPrefix(pat, "*.") {
		suffix := pat[1:] // ".internal"
		return strings.HasSuffix(h, suffix) && len(h) > len(suffix)
	}
	return false
}

// --- glob.* -----------------------------------------------------------------

func globHelpers() cel.EnvOption {
	return cel.Function("glob.match",
		cel.Overload("glob_match_string_string",
			[]*cel.Type{cel.StringType, cel.StringType},
			cel.BoolType,
			cel.BinaryBinding(func(pat, s ref.Val) ref.Val {
				ok, _ := filepath.Match(pat.Value().(string), s.Value().(string))
				return types.Bool(ok)
			}),
		),
	)
}

// --- semver.* ---------------------------------------------------------------

func semverHelpers() cel.EnvOption {
	return cel.Function("semver.satisfies",
		cel.Overload("semver_satisfies_string_string",
			[]*cel.Type{cel.StringType, cel.StringType},
			cel.BoolType,
			cel.BinaryBinding(func(ver, rng ref.Val) ref.Val {
				v, err := semver.NewVersion(ver.Value().(string))
				if err != nil {
					return types.Bool(false)
				}
				c, err := semver.NewConstraint(rng.Value().(string))
				if err != nil {
					return types.Bool(false)
				}
				return types.Bool(c.Check(v))
			}),
		),
	)
}

// --- call.* methods ---------------------------------------------------------

func callMethodHelpers() cel.EnvOption {
	mapType := cel.MapType(cel.StringType, types.DynType)
	return cel.Lib(callMethodLib{mapType: mapType})
}

type callMethodLib struct {
	mapType *cel.Type
}

func (l callMethodLib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("flag",
			cel.MemberOverload("call_flag_string",
				[]*cel.Type{l.mapType, cel.StringType},
				cel.StringType,
				cel.BinaryBinding(func(call, name ref.Val) ref.Val {
					m, errVal := callMap("flag", call)
					if errVal != nil {
						return errVal
					}
					flags, _ := m["flags"].(map[string]any)
					v, ok := flags[name.Value().(string)]
					if !ok {
						return types.String("")
					}
					// A SET flag whose value is not a string is
					// unanswerable, not empty. Returning "" made it
					// indistinguishable from an ABSENT flag, which is the one
					// thing an author writes `== ""` to test — so a shipped
					// constraint of the form
					//
					//   !call.hasFlag("all-namespaces") || call.flag("all-namespaces") == ""
					//
					// evaluated true for every call, flag present or not. Bool
					// flags are stored as a Go true by the parser; int and
					// stringList flags are equally non-string.
					//
					// Erroring is the same answer callMap gives a bad
					// receiver, for the same reason: the validator propagates
					// an eval error, so the constraint fails CLOSED instead of
					// quietly passing. An author who means "the flag is not
					// set" writes !call.hasFlag(...), which is unaffected.
					s, ok := v.(string)
					if !ok {
						return types.NewErr(
							"call.flag(%q): the flag is set to a %T, not a string — "+
								"use call.hasFlag(%q) to test whether it was passed",
							name.Value().(string), v, name.Value().(string))
					}
					return types.String(s)
				}),
			),
		),
		cel.Function("hasFlag",
			cel.MemberOverload("call_hasFlag_string",
				[]*cel.Type{l.mapType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(func(call, name ref.Val) ref.Val {
					m, errVal := callMap("hasFlag", call)
					if errVal != nil {
						return errVal
					}
					flags, _ := m["flags"].(map[string]any)
					_, ok := flags[name.Value().(string)]
					return types.Bool(ok)
				}),
			),
		),
		cel.Function("hasEnv",
			cel.MemberOverload("call_hasEnv_string",
				[]*cel.Type{l.mapType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(func(call, name ref.Val) ref.Val {
					m, errVal := callMap("hasEnv", call)
					if errVal != nil {
						return errVal
					}
					env, _ := m["env"].(map[string]string)
					_, ok := env[name.Value().(string)]
					return types.Bool(ok)
				}),
			),
		),
	}
}

func (callMethodLib) ProgramOptions() []cel.ProgramOption { return nil }

// callMap unwraps the `call` receiver of a call.* member helper, returning a
// CEL error value when it is not the native shape the helpers read.
//
// The receiver is declared map(string, dyn) and cel-go enforces that with a
// runtime type guard before invoking the binding — but the guard compares CEL
// types, not Go representations. The toolspec validator binds a native
// map[string]any (see callAsMap) and matches; a map LITERAL in an
// author-written constraint also matches the guard and arrives as
// map[ref.Val]ref.Val. Discarding the assertion's ok there yielded a nil map
// and then the helper's zero value, so a deny rule shaped like
// `<call>.hasFlag("force")` silently stopped denying. Erroring is the only
// safe answer: the validator propagates an eval error, so the constraint fails
// closed instead of quietly passing.
func callMap(helper string, call ref.Val) (map[string]any, ref.Val) {
	m, ok := call.Value().(map[string]any)
	if !ok {
		return nil, types.NewErr("call.%s: receiver must be a call map, got %T", helper, call.Value())
	}
	return m, nil
}
