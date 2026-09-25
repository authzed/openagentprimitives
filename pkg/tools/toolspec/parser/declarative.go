package parser

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// Declarative is the single generic argv walker driven by the toolkit schema.
// Flag-handling helpers (indexing, value assignment, type coercion) live in
// flags.go.
type Declarative struct{}

func (d *Declarative) Parse(tk *toolkit.Toolkit, argv []string) (*Call, error) {
	// git (and similar tools) require their global flags BEFORE the
	// subcommand: `git [global flags] <subcommand> [subcommand args]`.
	// Consume a leading prefix of recognized global flags first, then
	// match the subcommand on the remainder.
	globalEnd, leading, err := consumeLeadingGlobals(tk, argv)
	if err != nil {
		return nil, err
	}
	subArgv := argv[globalEnd:]

	sc, bestLen := matchSubcommand(tk, subArgv)
	if len(subArgv) == 0 && sc == nil {
		detail := "empty argv"
		if globalEnd > 0 {
			// argv was non-empty; leading global flags consumed all of it.
			detail = "no subcommand after global flags"
		}
		return nil, &ParseError{Kind: KindUnknownSubcommand, Detail: detail}
	}
	if sc == nil {
		return nil, &ParseError{
			Kind:   KindUnknownSubcommand,
			Detail: fmt.Sprintf("%q does not match any subcommand", strings.Join(subArgv, " ")),
		}
	}
	call := &Call{
		Subcommand:     strings.Join(sc.Path, " "),
		SubcommandPath: append([]string(nil), sc.Path...),
		Flags:          map[string]any{},
		Positional:     map[string]any{},
		// Argv is the full original input, leading globals included.
		Argv: append([]string(nil), argv...),
	}

	// Record the leading global-flag values onto the call before parsing
	// the post-subcommand remainder.
	for k, v := range leading {
		call.Flags[k] = v
	}

	rest := subArgv[bestLen:]
	tail, rest := splitTail(rest)
	call.Tail = tail
	if err := parseRest(tk, sc, rest, tail, call); err != nil {
		return nil, err
	}
	return call, nil
}

// consumeLeadingGlobals walks argv from index 0 and consumes a prefix made up
// solely of recognized GLOBAL flags (and their values). It returns the index
// where scanning stopped (the start of the subcommand region) and a map of the
// consumed global-flag values keyed by flagKey.
//
// Scanning stops at the first token that is NOT a recognized global flag — a
// non-global "--name", a non-global "-x", or any token that doesn't start with
// "-" (the subcommand token itself). A non-global flag in the leading position
// is left in place so subcommand matching reports it as UnknownSubcommand
// rather than the parser silently consuming it.
func consumeLeadingGlobals(tk *toolkit.Toolkit, argv []string) (int, map[string]any, error) {
	// Index only the toolkit's global flags — subcommand-local flags are
	// not valid before the subcommand.
	gi := flagIndex{byLong: map[string]*toolkit.Flag{}, byShort: map[string]*toolkit.Flag{}}
	for i := range tk.GlobalFlags {
		f := &tk.GlobalFlags[i]
		if f.Long != "" {
			gi.byLong[f.Long] = f
		}
		if f.Short != "" {
			gi.byShort[f.Short] = f
		}
	}

	// scratch Call to reuse assignFlagValue's type-coercion/value logic.
	scratch := &Call{Flags: map[string]any{}}
	i := 0
	for i < len(argv) {
		tok := argv[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			name, value, hasValue := splitLongFlag(tok)
			f, ok := gi.byLong[name]
			if !ok {
				return i, scratch.Flags, nil // not a global flag — subcommand region
			}
			consumed, err := assignFlagValue(f, value, hasValue, argv, i, scratch)
			if err != nil {
				return 0, nil, err
			}
			i += 1 + consumed
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			name := tok[1:]
			f, ok := gi.byShort[name]
			if !ok {
				return i, scratch.Flags, nil // not a global short flag — subcommand region
			}
			consumed, err := assignFlagValue(f, "", false, argv, i, scratch)
			if err != nil {
				return 0, nil, err
			}
			i += 1 + consumed
		default:
			// A bare token (not "-"-prefixed): the subcommand begins here.
			return i, scratch.Flags, nil
		}
	}
	return i, scratch.Flags, nil
}

// parseRest consumes flags and positional values from rest (the pre-`--`
// region) and binds them together with tail (the post-`--` region).
func parseRest(tk *toolkit.Toolkit, sc *toolkit.Subcommand, rest, tail []string, call *Call) error {
	flagIndex := indexFlags(tk, sc)
	var positionals []string
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			name, value, hasValue := splitLongFlag(tok)
			f, ok := flagIndex.byLong[name]
			if !ok {
				return &ParseError{Kind: KindUnknownFlag, Detail: "--" + name}
			}
			consumed, err := assignFlagValue(f, value, hasValue, rest, i, call)
			if err != nil {
				return err
			}
			i += consumed
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			name := tok[1:]
			f, ok := flagIndex.byShort[name]
			if !ok {
				return &ParseError{Kind: KindUnknownFlag, Detail: "-" + name}
			}
			consumed, err := assignFlagValue(f, "", false, rest, i, call)
			if err != nil {
				return err
			}
			i += consumed
		default:
			positionals = append(positionals, tok)
		}
	}
	return assignPositionals(sc, positionals, tail, call)
}

// assignPositionals binds positional values to the subcommand's declared slots.
// head holds the values before any `--`; tail holds the values after it.
//
// Post-`--` values are ordinary arguments the binary acts on — `--` only stops
// the binary's OPTION parsing — so they must land in a declared slot like any
// other. Left unbound as a raw Call.Tail nobody validates, `git checkout --
// <path>` reaches the sandbox with an empty call.Positional and every
// `call.positional[...]` constraint over it is short-circuited.
func assignPositionals(sc *toolkit.Subcommand, head, tail []string, call *Call) error {
	declared := sc.Positional

	// tailStart is the slot the post-`--` values begin filling. A subcommand
	// that marks a slot AfterDashDash pins them there — git's `log [<rev>]
	// [-- <path>…]`, where a bare `git log -- a.txt` sets paths and leaves
	// revision-range unset. Unmarked, `--` is a plain option terminator and the
	// tail just continues where head left off (`git clone -- <repo> <dir>`).
	tailStart := len(head)
	if len(tail) > 0 {
		for i, p := range declared {
			if p.AfterDashDash {
				tailStart = i
				break
			}
		}
		if tailStart < len(head) {
			return &ParseError{
				Kind: KindArgCountMismatch,
				Detail: fmt.Sprintf("got %d positional args before %q, which holds the post-`--` args (slot %d)",
					len(head), declared[tailStart].Name, tailStart),
			}
		}
	}

	// A variadic stringList in the LAST slot consumes any number of remaining
	// values (zero, when not required), so the strict count checks apply only
	// without one. Safe to skip: the variadic absorbs both overruns — head
	// values past its index, and (see the binding loop) the whole tail when no
	// slot claimed it. head can never reach a variadic slot the tail also
	// targets, since that slot is last and head is bounded by tailStart.
	hasVariadic := len(declared) > 0 && declared[len(declared)-1].Type == "stringList"
	if !hasVariadic {
		if len(head) > len(declared) {
			return &ParseError{
				Kind:   KindArgCountMismatch,
				Detail: fmt.Sprintf("got %d positional args, declared %d", len(head), len(declared)),
			}
		}
		if tailStart+len(tail) > len(declared) {
			return &ParseError{
				Kind: KindArgCountMismatch,
				Detail: fmt.Sprintf("got %d positional args after `--`, only %d slots remain of %d declared",
					len(tail), len(declared)-tailStart, len(declared)),
			}
		}
	}

	for i, p := range declared {
		// Slots at or past tailStart draw from the tail; earlier slots from the
		// head. Either group may run out — an unsupplied optional slot is
		// simply absent from call.Positional.
		src, off := head, 0
		if i >= tailStart {
			src, off = tail, tailStart
		}
		j := i - off
		if j >= len(src) {
			if p.Required {
				return &ParseError{
					Kind:   KindArgCountMismatch,
					Detail: fmt.Sprintf("missing required positional %q", p.Name),
				}
			}
			continue
		}
		if p.Type == "stringList" && i == len(declared)-1 {
			// Variadic: consume all remaining positional values into a []string.
			remaining := append([]string(nil), src[j:]...)
			if i < tailStart {
				// This last slot draws from head, so nothing else can take the
				// tail. Without this the tail is dropped from call.Positional
				// while still riding call.Argv into the binary, making every
				// `call.positional[…]` constraint over this slot bypassable by
				// inserting a `--`. Guarded on i < tailStart because when the
				// tail has its own slot, src IS tail and appending duplicates.
				remaining = append(remaining, tail...)
			}
			if len(remaining) == 1 && p.SplitOn != "" {
				remaining = strings.Split(remaining[0], p.SplitOn)
			}
			call.Positional[p.Name] = remaining
			break
		}
		call.Positional[p.Name] = src[j]
	}
	return nil
}

// IdentifySubcommand returns the subcommand argv selects, after skipping any
// recognized leading global flags (e.g. `git -C /repo clone …`), or nil when
// argv matches none. It performs NO flag/positional validation — it is a
// lightweight lookup for callers that need only the resolved subcommand (e.g.
// to read its declared per-call timeout) without the full Parse contract. The
// authoritative, validating parse remains (*Declarative).Parse.
func IdentifySubcommand(tk *toolkit.Toolkit, argv []string) *toolkit.Subcommand {
	globalEnd, _, err := consumeLeadingGlobals(tk, argv)
	if err != nil {
		// Best-effort: a malformed leading global is the authoritative
		// parser's problem; fall back to matching from the front so a
		// subcommand without preceding globals is still identified.
		globalEnd = 0
	}
	sc, _ := matchSubcommand(tk, argv[globalEnd:])
	return sc
}

// matchSubcommand returns the longest subcommand whose Path is a prefix of argv.
func matchSubcommand(tk *toolkit.Toolkit, argv []string) (*toolkit.Subcommand, int) {
	var best *toolkit.Subcommand
	bestLen := 0
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if len(sc.Path) > len(argv) {
			continue
		}
		if len(sc.Path) < bestLen {
			continue
		}
		match := true
		for j, p := range sc.Path {
			if argv[j] != p {
				match = false
				break
			}
		}
		if match {
			best = sc
			bestLen = len(sc.Path)
		}
	}
	return best, bestLen
}

// splitTail extracts the post-"--" tail (if any) from rest.
func splitTail(rest []string) (tail []string, head []string) {
	for i, tok := range rest {
		if tok == "--" {
			return append([]string(nil), rest[i+1:]...), rest[:i]
		}
	}
	return nil, rest
}
