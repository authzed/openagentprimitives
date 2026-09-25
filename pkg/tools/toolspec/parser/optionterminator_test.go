package parser

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// gitLikeToolkit mirrors the shape of the real git toolkit that matters for
// option-terminator and optional-argument handling: a repeatable global `-c`,
// a subcommand whose own short `-c` collides with it, subcommands with
// optional-argument flags, and subcommands where `--` means "pathspec" vs
// plain end-of-options.
func gitLikeToolkit() *toolkit.Toolkit {
	return &toolkit.Toolkit{
		Name: "git", Version: "1", ToolkitRevision: "r",
		Target: toolkit.Target{Binary: "git"},
		Parser: toolkit.ParserConfig{Kind: "declarative"},
		GlobalFlags: []toolkit.Flag{
			{Short: "c", Type: "stringList"},
			{Long: "git-dir", Type: "path"},
		},
		Subcommands: []toolkit.Subcommand{
			{
				// `--` is plain end-of-options: post-`--` tokens keep filling
				// the declared slots in order.
				Path: []string{"clone"},
				Positional: []toolkit.Positional{
					{Name: "repository", Type: "string", Required: true},
					{Name: "directory", Type: "path"},
				},
				Flags:   []toolkit.Flag{{Long: "recurse-submodules", Type: "string", OptionalValue: true}},
				Effects: emptyEffects(),
			},
			{
				Path: []string{"push"},
				Positional: []toolkit.Positional{
					{Name: "remote", Type: "string"},
					{Name: "refspec", Type: "string"},
				},
				Flags: []toolkit.Flag{
					{Long: "force", Short: "f", Type: "bool"},
					{Long: "force-with-lease", Type: "string", OptionalValue: true},
				},
				Effects: emptyEffects(),
			},
			{
				// `--` separates revisions from pathspecs: post-`--` tokens
				// start binding at `paths`, never at `revision-range`.
				Path: []string{"log"},
				Positional: []toolkit.Positional{
					{Name: "revision-range", Type: "string"},
					{Name: "paths", Type: "stringList", AfterDashDash: true},
				},
				Flags: []toolkit.Flag{
					{Long: "decorate", Type: "string", OptionalValue: true},
					{Long: "grep", Type: "string"},
					{Long: "unified", Short: "U", Type: "int", OptionalValue: true},
				},
				Effects: emptyEffects(),
			},
			{
				Path: []string{"checkout"},
				Positional: []toolkit.Positional{
					{Name: "branch-or-tree-ish", Type: "string"},
					{Name: "pathspec", Type: "stringList", AfterDashDash: true},
				},
				Effects: emptyEffects(),
			},
			{
				// Subcommand-local `-c` must shadow the global `-c`, matching
				// git: a global only takes effect BEFORE the subcommand.
				Path:       []string{"switch"},
				Positional: []toolkit.Positional{{Name: "branch", Type: "string"}},
				Flags:      []toolkit.Flag{{Long: "create", Short: "c", Type: "string"}},
				Effects:    emptyEffects(),
			},
		},
	}
}

// TestDeclarative_OptionalValueFlag covers flags the wrapped CLI declares with
// an OPTIONAL argument (git's PARSE_OPT_OPTARG: `--decorate[=<mode>]`,
// `--force-with-lease[=<ref>]`, `-U[<n>]`, …). git never consumes the following
// token for these, so neither may we: eating it silently steals a positional
// and desynchronizes call.Positional from the argv the binary actually sees —
// which is how a positional constraint gets bypassed.
func TestDeclarative_OptionalValueFlag(t *testing.T) {
	p := &Declarative{}
	tk := gitLikeToolkit()

	cases := []struct {
		name     string
		argv     []string
		wantFlag map[string]any
		wantPos  map[string]any
	}{
		{
			name:     "bare optional-value flag: presence recorded, next token stays a positional",
			argv:     []string{"push", "--force-with-lease", "origin", "refs/heads/main"},
			wantFlag: map[string]any{"force-with-lease": true},
			wantPos:  map[string]any{"remote": "origin", "refspec": "refs/heads/main"},
		},
		{
			name:     "attached optional-value flag: value bound, positionals unshifted",
			argv:     []string{"push", "--force-with-lease=refs/heads/main", "origin", "refs/heads/agent/x"},
			wantFlag: map[string]any{"force-with-lease": "refs/heads/main"},
			wantPos:  map[string]any{"remote": "origin", "refspec": "refs/heads/agent/x"},
		},
		{
			name:     "bare optional-value flag before a required-value flag",
			argv:     []string{"log", "--decorate", "--grep", "fix", "v1.0"},
			wantFlag: map[string]any{"decorate": true, "grep": "fix"},
			wantPos:  map[string]any{"revision-range": "v1.0"},
		},
		{
			name:     "optional-value int flag: bare form does not consume the revision",
			argv:     []string{"log", "-U", "v1.0"},
			wantFlag: map[string]any{"unified": true},
			wantPos:  map[string]any{"revision-range": "v1.0"},
		},
		{
			name:     "optional-value int flag: attached long form coerces to int",
			argv:     []string{"log", "--unified=3", "v1.0"},
			wantFlag: map[string]any{"unified": int64(3)},
			wantPos:  map[string]any{"revision-range": "v1.0"},
		},
		{
			name:     "plain bool flag is unaffected",
			argv:     []string{"push", "--force", "origin", "main"},
			wantFlag: map[string]any{"force": true},
			wantPos:  map[string]any{"remote": "origin", "refspec": "main"},
		},
		{
			name:     "required-value flag still consumes the next token",
			argv:     []string{"log", "--grep", "fix"},
			wantFlag: map[string]any{"grep": "fix"},
			wantPos:  map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := p.Parse(tk, tc.argv)
			require.NoError(t, err, "Parse")
			assert.Equal(t, tc.wantFlag, call.Flags, "Flags")
			assert.Equal(t, tc.wantPos, call.Positional, "Positional")
		})
	}
}

// TestDeclarative_DashDashBindsPositionals covers the option terminator. Tokens
// after `--` are protected from flag parsing but are STILL positional arguments
// the binary acts on, so they must be bound to the declared slots — leaving
// them only on call.Tail makes every `call.positional[...]` constraint
// bypassable by inserting a `--`.
//
// Where they bind is per-subcommand, because `--` is overloaded in git: plain
// end-of-options for `clone`/`push`, but the revision/pathspec separator for
// `log`/`checkout`. AfterDashDash marks the slot the tail starts filling.
func TestDeclarative_DashDashBindsPositionals(t *testing.T) {
	p := &Declarative{}
	tk := gitLikeToolkit()

	cases := []struct {
		name    string
		argv    []string
		wantPos map[string]any
	}{
		{
			name:    "plain end-of-options: tail keeps filling slots in order",
			argv:    []string{"clone", "--", "https://example.invalid/r.git", "dst"},
			wantPos: map[string]any{"repository": "https://example.invalid/r.git", "directory": "dst"},
		},
		{
			name:    "dash-leading value after -- binds rather than vanishing",
			argv:    []string{"clone", "--", "--upload-pack=/bin/sh"},
			wantPos: map[string]any{"repository": "--upload-pack=/bin/sh"},
		},
		{
			name:    "pathspec separator: tail starts at the AfterDashDash slot",
			argv:    []string{"log", "--", "a.txt", "b.txt"},
			wantPos: map[string]any{"paths": []string{"a.txt", "b.txt"}},
		},
		{
			name:    "revision before --, pathspec after",
			argv:    []string{"log", "v1.0", "--", "a.txt"},
			wantPos: map[string]any{"revision-range": "v1.0", "paths": []string{"a.txt"}},
		},
		{
			name:    "checkout pathspec after -- does not land in the tree-ish slot",
			argv:    []string{"checkout", "--", "some/file"},
			wantPos: map[string]any{"pathspec": []string{"some/file"}},
		},
		{
			name:    "no -- at all: unchanged in-order binding",
			argv:    []string{"push", "origin", "main"},
			wantPos: map[string]any{"remote": "origin", "refspec": "main"},
		},
		{
			name:    "push tail fills remote+refspec in order",
			argv:    []string{"push", "--", "origin", "refs/heads/main"},
			wantPos: map[string]any{"remote": "origin", "refspec": "refs/heads/main"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := p.Parse(tk, tc.argv)
			require.NoError(t, err, "Parse")
			assert.Equal(t, tc.wantPos, call.Positional, "Positional")
		})
	}
}

// TestDeclarative_DashDashTailPreserved keeps call.Tail populated alongside the
// positional binding: `call.tail` is part of the CEL surface specs may already
// reference, and binding must not silently empty it.
func TestDeclarative_DashDashTailPreserved(t *testing.T) {
	p := &Declarative{}
	call, err := p.Parse(gitLikeToolkit(), []string{"log", "v1.0", "--", "a.txt"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, []string{"a.txt"}, call.Tail, "Tail")
}

// TestDeclarative_DashDashOverflowFailsClosed proves a tail that cannot be
// bound to a declared slot is a parse error, not silently-dropped argv. Dropping
// it is what let `git checkout -- <path>` reach the binary with an empty
// call.Positional.
func TestDeclarative_DashDashOverflowFailsClosed(t *testing.T) {
	p := &Declarative{}
	tk := gitLikeToolkit()

	cases := []struct {
		name string
		argv []string
	}{
		{
			name: "more tail tokens than declared slots",
			argv: []string{"clone", "--", "https://example.invalid/r.git", "dst", "extra"},
		},
		{
			name: "pre-`--` positionals overrun the AfterDashDash slot",
			argv: []string{"log", "v1.0", "v2.0", "--", "a.txt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Parse(tk, tc.argv)
			require.Error(t, err, "Parse must reject unbindable tail")
			var pe *ParseError
			require.True(t, errors.As(err, &pe), "error must be a *ParseError, got %T", err)
			assert.Equal(t, KindArgCountMismatch, pe.Kind, "ParseError.Kind")
		})
	}
}

// TestDeclarative_UnmarkedVariadicBindsTail covers the shape every other case
// in this file misses: a trailing variadic slot that does NOT set
// AfterDashDash, because for its binary `--` is a plain option terminator
// (`cat a.txt -- b.txt`). Every stringList in gitLikeToolkit marks the slot,
// so this branch — the one a single-command toolkit actually takes — went
// unexercised, and the tail was dropped from call.Positional while still being
// executed. That is exactly the call.positional bypass
// toolkit.Positional.AfterDashDash's doc says cannot happen.
func TestDeclarative_UnmarkedVariadicBindsTail(t *testing.T) {
	p := &Declarative{}

	// Single trailing variadic, no subcommand token: the cat/echo shape.
	filesOnly := tkWith(toolkit.Subcommand{
		Path:       []string{},
		Positional: []toolkit.Positional{{Name: "files", Type: "stringList"}},
		Effects:    emptyEffects(),
	})
	// A leading required slot in front of the variadic: the `run <image>
	// <cmd...>` shape, where the tail is dropped only once the pre-`--` count
	// has already passed the variadic's index.
	imageThenArgs := tkWith(toolkit.Subcommand{
		Path: []string{"run"},
		Positional: []toolkit.Positional{
			{Name: "image", Type: "string", Required: true},
			{Name: "args", Type: "stringList"},
		},
		Effects: emptyEffects(),
	})

	cases := []struct {
		name    string
		tk      *toolkit.Toolkit
		argv    []string
		wantPos map[string]any
	}{
		{
			name:    "tail continues the variadic slot rather than vanishing",
			tk:      filesOnly,
			argv:    []string{"a.txt", "--", "b.txt"},
			wantPos: map[string]any{"files": []string{"a.txt", "b.txt"}},
		},
		{
			name:    "bare `--` with only a tail: every value still binds",
			tk:      filesOnly,
			argv:    []string{"--", "a.txt", "b.txt"},
			wantPos: map[string]any{"files": []string{"a.txt", "b.txt"}},
		},
		{
			name:    "dash-leading tail value binds rather than vanishing",
			tk:      filesOnly,
			argv:    []string{"a.txt", "--", "--/etc/shadow"},
			wantPos: map[string]any{"files": []string{"a.txt", "--/etc/shadow"}},
		},
		{
			name:    "no `--` at all: in-order binding unchanged",
			tk:      filesOnly,
			argv:    []string{"a.txt", "b.txt"},
			wantPos: map[string]any{"files": []string{"a.txt", "b.txt"}},
		},
		{
			name:    "variadic behind a filled leading slot still takes the tail",
			tk:      imageThenArgs,
			argv:    []string{"run", "img", "cmd", "--", "extra"},
			wantPos: map[string]any{"image": "img", "args": []string{"cmd", "extra"}},
		},
		{
			name:    "empty variadic before `--`: the tail alone fills it",
			tk:      imageThenArgs,
			argv:    []string{"run", "img", "--", "cmd"},
			wantPos: map[string]any{"image": "img", "args": []string{"cmd"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := p.Parse(tc.tk, tc.argv)
			require.NoError(t, err, "Parse")
			assert.Equal(t, tc.wantPos, call.Positional, "Positional")
		})
	}
}

// TestDeclarative_MarkedVariadicDoesNotDoubleBindTail guards the other side of
// the fix: when a slot DOES mark AfterDashDash the tail already has a home, so
// appending it a second time would duplicate every post-`--` value. `git log
// v1.0 -- a.txt` must yield paths=[a.txt], never [a.txt a.txt].
func TestDeclarative_MarkedVariadicDoesNotDoubleBindTail(t *testing.T) {
	p := &Declarative{}
	call, err := p.Parse(gitLikeToolkit(), []string{"log", "v1.0", "--", "a.txt", "b.txt"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, []string{"a.txt", "b.txt"}, call.Positional["paths"], "Positional[paths]")
}

// TestDeclarative_SubcommandFlagShadowsGlobal proves a subcommand's own flag
// wins over a same-named global — what indexFlags already documents, and what
// git does (a global is only honored BEFORE the subcommand). Without it,
// `git switch -c <branch>` parses as the global `-c <config>` and a spec that
// scopes `-c` to committer identity rejects a plain branch creation.
func TestDeclarative_SubcommandFlagShadowsGlobal(t *testing.T) {
	p := &Declarative{}
	call, err := p.Parse(gitLikeToolkit(), []string{"switch", "-c", "feature-x"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, "feature-x", call.Flags["create"], "Flags[create] — subcommand flag must win")
	assert.NotContains(t, call.Flags, "c", "global -c must not capture the subcommand's -c")
	assert.Empty(t, call.Positional, "the branch name is the flag's value, not a positional")
}

// TestDeclarative_LeadingGlobalStillBinds guards the other direction: a global
// placed before the subcommand (where git honors it) is still captured.
func TestDeclarative_LeadingGlobalStillBinds(t *testing.T) {
	p := &Declarative{}
	call, err := p.Parse(gitLikeToolkit(), []string{"-c", "user.name=Ada", "switch", "main"})
	require.NoError(t, err, "Parse")
	assert.Equal(t, []string{"user.name=Ada"}, call.Flags["c"], "Flags[c]")
	assert.Equal(t, "main", call.Positional["branch"], "Positional[branch]")
}
