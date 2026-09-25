package parser

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// flagIndex maps long/short flag names to their toolkit.Flag descriptor for
// O(1) lookup during the argv walk. Subcommand-local flags shadow globals.
type flagIndex struct {
	byLong  map[string]*toolkit.Flag
	byShort map[string]*toolkit.Flag
}

func indexFlags(tk *toolkit.Toolkit, sc *toolkit.Subcommand) flagIndex {
	fi := flagIndex{byLong: map[string]*toolkit.Flag{}, byShort: map[string]*toolkit.Flag{}}
	// keepExisting: globals must NOT clobber a same-named subcommand flag.
	// The wrapped CLI honors a global only BEFORE the subcommand, so in the
	// post-subcommand region the subcommand's own meaning is the right one.
	// git's `-c` is the case that bites: as a global it is `-c <key>=<value>`,
	// but `git switch -c <branch>` is switch's --create. Letting the global win
	// made a plain branch creation parse as a config override — which a spec
	// that scopes `-c` to committer identity then rejected.
	add := func(flags []toolkit.Flag, keepExisting bool) {
		for i := range flags {
			f := &flags[i]
			if f.Long != "" {
				if _, taken := fi.byLong[f.Long]; !taken || !keepExisting {
					fi.byLong[f.Long] = f
				}
			}
			if f.Short != "" {
				if _, taken := fi.byShort[f.Short]; !taken || !keepExisting {
					fi.byShort[f.Short] = f
				}
			}
		}
	}
	add(sc.Flags, false)
	add(tk.GlobalFlags, true)
	return fi
}

// flagKey returns the map key under which a flag's parsed value is stored in
// call.Flags. Long name when present; otherwise the short name (for short-only
// flags such as git's -C, which has no long form).
func flagKey(f *toolkit.Flag) string {
	if f.Long != "" {
		return f.Long
	}
	return f.Short
}

// splitLongFlag handles "--name=value" and "--name".
func splitLongFlag(tok string) (name string, value string, hasValue bool) {
	t := tok[2:]
	if eq := strings.IndexByte(t, '='); eq >= 0 {
		return t[:eq], t[eq+1:], true
	}
	return t, "", false
}

// assignFlagValue writes f's value into call.Flags, returning the number of
// extra tokens consumed beyond the current index (0 if the flag was inline,
// 1 if a following token was the value).
func assignFlagValue(f *toolkit.Flag, inlineValue string, hasInline bool, rest []string, i int, call *Call) (int, error) {
	// key keys call.Flags and labels error details — long name, or short
	// name for short-only flags (e.g. git's -C).
	key := flagKey(f)
	// An optional-value flag written bare takes no argument in the wrapped CLI,
	// so consuming the next token would steal a positional the binary is going
	// to act on. Record presence only; `true` (rather than the type's zero
	// value) keeps a value comparison in a constraint from matching something
	// the caller never supplied.
	if f.OptionalValue && !hasInline {
		call.Flags[key] = true
		return 0, nil
	}
	switch f.Type {
	case "bool":
		if hasInline {
			return 0, &ParseError{Kind: KindFlagTypeMismatch, Detail: key + " is bool, takes no value"}
		}
		call.Flags[key] = true
		return 0, nil
	case "string", "path", "url":
		v, consumed, err := takeStringValue(key, inlineValue, hasInline, rest, i)
		if err != nil {
			return 0, err
		}
		call.Flags[key] = v
		return consumed, nil
	case "int":
		v, consumed, err := takeStringValue(key, inlineValue, hasInline, rest, i)
		if err != nil {
			return 0, err
		}
		n, parseErr := parseInt64(v)
		if parseErr != nil {
			return 0, &ParseError{Kind: KindFlagTypeMismatch, Detail: key + ": " + parseErr.Error()}
		}
		call.Flags[key] = n
		return consumed, nil
	case "enum":
		v, consumed, err := takeStringValue(key, inlineValue, hasInline, rest, i)
		if err != nil {
			return 0, err
		}
		ok := false
		for _, allowed := range f.Values {
			if v == allowed {
				ok = true
				break
			}
		}
		if !ok {
			return 0, &ParseError{Kind: KindEnumValueInvalid, Detail: fmt.Sprintf("%s=%q not in %v", key, v, f.Values)}
		}
		call.Flags[key] = v
		return consumed, nil
	case "stringList":
		v, consumed, err := takeStringValue(key, inlineValue, hasInline, rest, i)
		if err != nil {
			return 0, err
		}
		var values []string
		if f.SplitOn != "" {
			values = strings.Split(v, f.SplitOn)
		} else {
			values = []string{v}
		}
		existing, _ := call.Flags[key].([]string)
		call.Flags[key] = append(existing, values...)
		return consumed, nil
	default:
		return 0, &ParseError{Kind: KindFlagTypeMismatch, Detail: "unsupported flag type: " + f.Type}
	}
}

func takeStringValue(long, inlineValue string, hasInline bool, rest []string, i int) (string, int, error) {
	if hasInline {
		return inlineValue, 0, nil
	}
	if i+1 >= len(rest) {
		return "", 0, &ParseError{Kind: KindMissingFlagValue, Detail: long}
	}
	return rest[i+1], 1, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	var sign int64 = 1
	if len(s) == 0 {
		return 0, fmt.Errorf("empty int")
	}
	i := 0
	if s[0] == '-' {
		sign = -1
		i = 1
	}
	if i == len(s) {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n * sign, nil
}
